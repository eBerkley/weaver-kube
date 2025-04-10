// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package impl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/eberkley/weaver-kube/internal/impl/routing"
	"github.com/eberkley/weaver/runtime"
	"github.com/eberkley/weaver/runtime/envelope"
	"github.com/eberkley/weaver/runtime/logging"
	"github.com/eberkley/weaver/runtime/metrics"
	"github.com/eberkley/weaver/runtime/protos"
	"github.com/eberkley/weaver/runtime/traces"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/sdk/trace"
	"golang.org/x/sync/errgroup"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	watch2 "k8s.io/client-go/tools/watch"
)

// BabysitterOptions configure a babysitter. See tool.Plugins for details.
type BabysitterOptions struct {
	HandleLogEntry   func(context.Context, *protos.LogEntry) error
	HandleTraceSpans func(context.Context, []trace.ReadOnlySpan) error
	HandleMetrics    func(context.Context, []*metrics.MetricSnapshot) error
}

// babysitter starts and manages a weavelet inside the Pod.
type babysitter struct {
	ctx             context.Context
	cfg             *BabysitterConfig
	opts            BabysitterOptions
	app             *protos.AppConfig
	envelope        *envelope.Envelope
	clientset       *kubernetes.Clientset
	logger          *slog.Logger
	printer         *logging.PrettyPrinter
	localComponents []string // Components running in this pod

	mu       sync.Mutex
	watching map[string]struct{} // components being watched
}

var _ envelope.EnvelopeHandler = &babysitter{}

func NewBabysitter(ctx context.Context, app *protos.AppConfig, config *BabysitterConfig, components []string, opts BabysitterOptions) (*babysitter, error) {
	// Create the envelope.
	wlet := &protos.WeaveletArgs{
		App:             app.Name,
		DeploymentId:    config.DeploymentId,
		Id:              uuid.New().String(),
		RunMain:         slices.Contains(components, runtime.Main),
		InternalAddress: fmt.Sprintf(":%d", internalPort),
	}
	logger := logging.StderrLogger(logging.Options{
		App:       app.Name,
		Component: "babysitter",
		Weavelet:  wlet.Id,
		Attrs:     []string{"serviceweaver/system", ""},
	})
	e, err := envelope.NewEnvelope(ctx, wlet, app, envelope.Options{Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("NewBabysitter: create envelope: %w", err)
	}

	// Create a Kubernetes client set.
	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("NewBabysitter: get kube config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("NewBabysitter: get kube client set: %w", err)
	}

	// Create the babysitter.
	b := &babysitter{
		ctx:       ctx,
		cfg:       config,
		opts:      opts,
		app:       app,
		envelope:  e,
		clientset: clientset,
		logger:    logger,
		watching:  map[string]struct{}{},
	}

	// Create the pretty printer for logging, if there is no log handler.
	if opts.HandleLogEntry == nil {
		b.printer = logging.NewPrettyPrinter(false /*colors disabled*/)
	}
	// Initialize the colocated components
	b.localComponents = components

	// Inform the weavelet of the components it should host.
	if err := b.envelope.UpdateComponents(components); err != nil {
		return nil, fmt.Errorf("NewBabysitter: update components: %w", err)
	}

	return b, nil
}

func (b *babysitter) Serve() error {
	group, ctx := errgroup.WithContext(b.ctx)
	if b.opts.HandleMetrics != nil {
		// Periodically call b.opts.HandleMetrics with the set of metrics.
		group.Go(func() error {
			ticker := time.NewTicker(b.cfg.Telemetry.Metrics.ExportInterval.AsDuration())
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
					if err := b.opts.HandleMetrics(ctx, b.readMetrics()); err != nil {
						return err
					}
				}
			}
		})
	}

	// Run the envelope and handle messages from the weavelet.
	group.Go(func() error {
		return b.envelope.Serve(b)
	})

	return group.Wait()
}

// ActivateComponent implements the envelope.EnvelopeHandler interface.
func (b *babysitter) ActivateComponent(_ context.Context, request *protos.ActivateComponentRequest) (*protos.ActivateComponentReply, error) {
	go func() {
		if err := b.watchPods(b.ctx, request.Component); err != nil {
			// TODO(mwhittaker): Log this error.
			fmt.Fprintf(os.Stderr, "watchPods(%q): %v", request.Component, err)
		}
	}()
	return &protos.ActivateComponentReply{}, nil
}

// watchPods watches the pods hosting the provided component, updating the
// routing info whenever the set of pods changes.
func (b *babysitter) watchPods(ctx context.Context, component string) error {
	b.mu.Lock()
	if _, ok := b.watching[component]; ok {
		// The pods for this component are already being watched.
		b.mu.Unlock()
		return nil
	}
	b.watching[component] = struct{}{}
	b.mu.Unlock()

	// Watch the pods running the requested component.
	groupEntry, ok := b.cfg.Groups[component]
	// rs, ok := b.cfg.Groups[component]
	if !ok || groupEntry == nil {
		return fmt.Errorf("unable to determine group name for component %s", component)
	}

	rs := groupEntry.GroupNames

	addrs := map[string]string{}
	var mu sync.Mutex

	for _, rgroup := range rs {
		group := rgroup
		go func() {
			err := b.watchPodsForGroup(ctx, component, group, &mu, addrs)
			if err != nil {
				slog.Error("Failed to watch pods for group", "group", group, "error", err)
			}
		}()
	}
	return nil

}

func (b *babysitter) watchPodsForGroup(ctx context.Context, component string, group string, mu *sync.Mutex, addrs map[string]string) error {
	name := deploymentName(b.app.Name, group, b.cfg.DeploymentId)
	opts := metav1.ListOptions{LabelSelector: fmt.Sprintf("serviceweaver/name=%s", name)}

	// Create a watcher to watch pod changes for the component.
	//
	// Note that we're trying a few times to create a watcher. If we're not able
	// to create a watcher successfully, we should not continue.
	var watcher watch.Interface
	var err error
	err = wait.ExponentialBackoff(wait.Backoff{
		Duration: 1 * time.Second,
		Factor:   1.5,
		Jitter:   0.2,
		Steps:    10, // Maximum retry attempts
	}, func() (bool, error) {
		watcher, err = watch2.NewRetryWatcher("1", &cache.ListWatch{
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return b.clientset.CoreV1().Pods(b.cfg.Namespace).Watch(ctx, opts)
			},
		})
		if err != nil {
			return false, err // retry to create the watcher
		}
		return true, nil // watcher created successfully
	})
	if err != nil {
		panic(fmt.Errorf("unable to create RetryWatcher: %v", err))
	}

	// Repeatedly receive events from Kubernetes, updating the set of pod
	// addresses appropriately. Abort when the channel is closed or the context
	// is canceled.
	// addrs := map[string]string{}
	for {
		select {
		case <-ctx.Done():
			watcher.Stop()
			return ctx.Err()

		case event, ok := <-watcher.ResultChan():
			if !ok {
				return nil
			}

			mu.Lock()
			stateful := false
			changed := false
			var idxStr string
			switch event.Type {
			case watch.Added, watch.Modified:
				pod := event.Object.(*v1.Pod)
				idxStr, stateful = pod.Labels["apps.kubernetes.io/pod-index"]

				if stateful { // Index by ordinality
					if pod.Status.PodIP != "" && addrs[idxStr] != pod.Status.PodIP {
						addrs[idxStr] = pod.Status.PodIP
						changed = true
					}
				} else { // Index by pod name
					if pod.Status.PodIP != "" && addrs[pod.Name] != pod.Status.PodIP {
						addrs[pod.Name] = pod.Status.PodIP
						changed = true
					}
				}

			case watch.Deleted:
				pod := event.Object.(*v1.Pod)
				idxStr, stateful = pod.Labels["apps.kubernetes.io/pod-index"]

				if stateful { // Index by ordinality
					if _, ok := addrs[idxStr]; ok {
						delete(addrs, idxStr)
						changed = true
					}
				} else { // Index by pod name
					if _, ok := addrs[pod.Name]; ok {
						delete(addrs, pod.Name)
						changed = true
					}
				}
			}

			mu.Unlock()
			if !changed {
				continue
			}
			var replicas []string

			if stateful {
				fmt.Fprintf(os.Stderr, "babysitter.watchPodsForGroup: component %v is stateful\n", component)
				replicas = make([]string, len(addrs))
				for idx, addr := range addrs {
					i, err := strconv.Atoi(idx)
					if err != nil {
						fmt.Fprintf(os.Stderr, "watchPodsForGroup: failure parsing stateful replica addr list: %v, error key: %v\n", addrs, err)
						continue
					}
					replicas[i] = fmt.Sprintf("tcp://%s:%d", addr, internalPort)
				}

			} else {
				fmt.Fprintf(os.Stderr, "babysitter.watchPodsForGroup: component %v is not stateful\n", component)
				replicas = []string{}
				for _, addr := range addrs {
					replicas = append(replicas, fmt.Sprintf("tcp://%s:%d", addr, internalPort))
				}
			}

			routingInfo := &protos.RoutingInfo{
				Component:  component,
				Replicas:   replicas,
				Local:      slices.Contains(b.localComponents, component),
				Stateful:   stateful,
				Assignment: routing.EqualSlices(replicas),
			}
			fmt.Fprintf(os.Stderr, "babysitter.UpdateRoutingInfo: preparing to call envelope.UpdateRoutingInfo(%v)\n", routingInfo)

			if err := b.envelope.UpdateRoutingInfo(routingInfo); err != nil {
				// TODO(mwhittaker): Log this error.
				fmt.Fprintf(os.Stderr, "UpdateRoutingInfo(%v): %v", routingInfo, err)
			}
			fmt.Fprintf(os.Stderr, "babysitter.watchPodsForGroup: envelope.UpdateRoutingInfo successful.\n")
		}
	}
}

// GetListenerAddress implements the envelope.EnvelopeHandler interface.
func (b *babysitter) GetListenerAddress(_ context.Context, request *protos.GetListenerAddressRequest) (*protos.GetListenerAddressReply, error) {
	port, ok := b.cfg.Listeners[request.Name]
	if !ok {
		return nil, fmt.Errorf("listener %q not found", request.Name)
	}
	return &protos.GetListenerAddressReply{Address: fmt.Sprintf(":%d", port)}, nil
}

// ExportListener implements the envelope.EnvelopeHandler interface.
func (b *babysitter) ExportListener(context.Context, *protos.ExportListenerRequest) (*protos.ExportListenerReply, error) {
	return &protos.ExportListenerReply{ProxyAddress: ""}, nil
}

// LogBatch implements the envelope.EnvelopeHandler interface.
func (b *babysitter) LogBatch(ctx context.Context, batch *protos.LogEntryBatch) error {
	var errs []error
	for _, entry := range batch.Entries {
		if err := b.handleLogEntry(ctx, entry); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (b *babysitter) handleLogEntry(ctx context.Context, entry *protos.LogEntry) error {
	if b.opts.HandleLogEntry != nil {
		return b.opts.HandleLogEntry(ctx, entry)
	}
	fmt.Println(b.printer.Format(entry))
	return nil
}

// HandleTraceSpans implements the envelope.EnvelopeHandler interface.
func (b *babysitter) HandleTraceSpans(ctx context.Context, spans *protos.TraceSpans) error {
	if b.opts.HandleTraceSpans == nil {
		return nil
	}

	var spansToExport []trace.ReadOnlySpan
	for _, span := range spans.Span {
		spansToExport = append(spansToExport, &traces.ReadSpan{Span: span})
	}
	return b.opts.HandleTraceSpans(ctx, spansToExport)
}

// GetSelfCertificate implements the envelope.EnvelopeHandler interface.
func (b *babysitter) GetSelfCertificate(context.Context, *protos.GetSelfCertificateRequest) (*protos.GetSelfCertificateReply, error) {
	panic("unused")
}

// VerifyClientCertificate implements the envelope.EnvelopeHandler interface.
func (b *babysitter) VerifyClientCertificate(context.Context, *protos.VerifyClientCertificateRequest) (*protos.VerifyClientCertificateReply, error) {
	panic("unused")
}

// VerifyServerCertificate implements the envelope.EnvelopeHandler interface.
func (b *babysitter) VerifyServerCertificate(context.Context, *protos.VerifyServerCertificateRequest) (*protos.VerifyServerCertificateReply, error) {
	panic("unused")
}

// readMetrics returns the latest metrics from the weavelet.
func (b *babysitter) readMetrics() []*metrics.MetricSnapshot {
	var ms []*metrics.MetricSnapshot
	ms = append(ms, metrics.Snapshot()...)
	m, err := b.envelope.GetMetrics()
	if err != nil {
		return ms
	}
	ms = append(ms, m...)
	return filterMetrics(ms, b.cfg.Telemetry.Metrics.AutoGenerateMetrics)
}

// filterMetrics returns a set of filtered metrics.
func filterMetrics(snaps []*metrics.MetricSnapshot, exportGeneratedMetrics bool) []*metrics.MetricSnapshot {
	if exportGeneratedMetrics {
		return snaps
	}
	var filtered []*metrics.MetricSnapshot
	// If the user didn't enable to export autogenerated metrics, filter out the
	// autogenerated metrics, before we export the metrics to Google Cloud.
	//
	// TODO(rgrandl): Add a method to MetricSnapshot to return whether a metric
	// is autogenerated.
	for _, snap := range snaps {
		if generated := snap.Labels["serviceweaver_generated"]; generated != "true" {
			filtered = append(filtered, snap)
		}
	}
	return filtered
}
