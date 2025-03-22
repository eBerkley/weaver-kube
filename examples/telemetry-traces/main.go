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

// This file contains an example of how to use the plugin API to configure how
// metrics and traces are exported. We register a plugin to export traces to
// Jaeger and a plugin to export metrics to Prometheus. Compile the telemetry
// binary and use it as you would "weaver kube". Use "prometheus.yaml" and
// "jaeger.yaml" to deploy Prometheus and Jaeger to a Kubernetes cluster.
//
//     $ kubectl apply \
//         -f jaeger.yaml \
//         -f prometheus.yaml \
//         -f $(telemetry deploy kube_deploy.yaml)

package main

import (
	"context"
	"fmt"

	"github.com/eberkley/weaver-kube/tool"
	"go.opentelemetry.io/otel/exporters/jaeger" //lint:ignore SA1019 TODO: Update
	"go.opentelemetry.io/otel/sdk/trace"
)

const (
	// The Jaeger ports. These values should be the same as the
	// ones in jaeger.yaml.
	jaegerPort     = 14268
)

func main() {
	// Export traces to Jaegar.
	jaegerURL := fmt.Sprintf("http://jaeger:%d/api/traces", jaegerPort)
	endpoint := jaeger.WithCollectorEndpoint(jaeger.WithEndpoint(jaegerURL))
	traceExporter, err := jaeger.New(endpoint)
	if err != nil {
		panic(err)
	}
	handleTraceSpans := func(ctx context.Context, spans []trace.ReadOnlySpan) error {
		return traceExporter.ExportSpans(ctx, spans)
	}


	tool.Run("telemetry", tool.Plugins{
		HandleTraceSpans: handleTraceSpans,
	})
}
