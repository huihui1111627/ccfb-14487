// Package allocator contains the four per-resource allocators
// (CPU, memory, hugepages, devices). Each allocator answers a request
// with several feasible NUMA-node combinations (bitmaps), marking the
// ones it prefers; the manager intersects the dimensions via the policy
// package and only then commits the chosen plan.
package allocator

import "topomgr/topo/numaset"

// Candidate is one feasible NUMA-node combination for one dimension.
type Candidate struct {
	Nodes     numaset.Set
	Preferred bool
	Reason    string
}

// CPUPlan is a concrete set of logical CPUs produced for one node set.
type CPUPlan struct {
	CPUs            []int
	WholeCores      []int // physical cores taken with all siblings
	SplitCoreIDs    []int // physical cores of which only some siblings were taken
	NewSplitCoreIDs []int // physical cores we are the first to break apart
}

// MemPlan records how much RAM is taken from each node.
type MemPlan struct {
	MBPerNode map[int]int64
}

// HugePlan records hugepages taken from each node.
type HugePlan struct {
	PagesPerNode map[int]int64
}

// DevPlan records which devices were grabbed.
type DevPlan struct {
	Devices []string
}
