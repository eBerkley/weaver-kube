package routing

import (
	"math"
	"slices"
	"sort"

	"github.com/eberkley/weaver/runtime/protos"
)

// EqualSlices returns an assignment with slices of roughly equal size.
// Replicas are assigned to slices in a round robin fashion. The returned
// assignment has a version of 0.
func EqualSlices(replicas []string) *protos.Assignment {
	if len(replicas) == 0 {
		return &protos.Assignment{}
	}

	// Note that the replicas should be sorted. This is required because we
	// want to do a deterministic assignment of slices to replicas among
	// different invocations, to avoid unnecessary churn while generating new
	// assignments.
	replicas = slices.Clone(replicas)
	sort.Strings(replicas)

	// Form n roughly equally sized slices, where n is the least power of two
	// larger than the number of replicas.
	//
	// TODO(mwhittaker): Shouldn't we pick a number divisible by the number of
	// replicas? Otherwise, not every replica gets the same number of slices.
	n := nextPowerOfTwo(len(replicas))
	slices := make([]*protos.Assignment_Slice, n)
	start := uint64(0)
	delta := math.MaxUint64 / uint64(n)
	for i := 0; i < n; i++ {
		slices[i] = &protos.Assignment_Slice{Start: start}
		start += delta
	}

	// Assign replicas to slices in a round robin fashion.
	for i, slice := range slices {
		slice.Replicas = []string{replicas[i%len(replicas)]}
	}
	return &protos.Assignment{Slices: slices}
}

// nextPowerOfTwo returns the least power of 2 that is greater or equal to x.
func nextPowerOfTwo(x int) int {
	switch {
	case x == 0:
		return 1
	case x&(x-1) == 0:
		// x is already power of 2.
		return x
	default:
		return int(math.Pow(2, math.Ceil(math.Log2(float64(x)))))
	}
}
