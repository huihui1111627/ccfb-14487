package allocator

import (
	"fmt"
	"sort"

	"topomgr/topo"
	"topomgr/topo/numaset"
)

// MemAllocator tracks free RAM per NUMA node in MB.
type MemAllocator struct {
	topo *topo.Topology
	free map[int]int64
}

func NewMemAllocator(t *topo.Topology) *MemAllocator {
	a := &MemAllocator{topo: t, free: map[int]int64{}}
	for id, n := range t.Nodes {
		a.free[id] = n.MemoryMB
	}
	return a
}

func (a *MemAllocator) Free(node int) int64 { return a.free[node] }

// Candidates returns node combinations that hold enough free RAM.
// Single-node fits are preferred; multi-node plans are only offered
// when no single node can satisfy the request.
func (a *MemAllocator) Candidates(mb int64, within numaset.Set, dumb bool) []Candidate {
	var out []Candidate
	sets := enumerateSets(a.topo, within)
	if dumb {
		sets = []numaset.Set{within}
	}
	for _, set := range sets {
		var total int64
		for _, nid := range set.Members() {
			total += a.free[nid]
		}
		if total < mb {
			continue
		}
		preferred := false
		reason := "spilled across nodes"
		if set.Count() == 1 && a.free[set.Members()[0]] >= mb {
			preferred = true
			reason = "fits on one node"
		}
		out = append(out, Candidate{Nodes: set, Preferred: preferred, Reason: reason})
		if len(out) >= maxCandidates || dumb {
			break
		}
	}
	return out
}

// PlanWithin spreads mb over the given nodes, filling the node with the
// most free capacity first so the allocation stays as local as possible.
func (a *MemAllocator) PlanWithin(mb int64, nodes numaset.Set) (*MemPlan, bool) {
	ids := nodes.Members()
	var total int64
	for _, nid := range ids {
		total += a.free[nid]
	}
	if total < mb {
		return nil, false
	}
	sort.Slice(ids, func(i, j int) bool { return a.free[ids[i]] > a.free[ids[j]] })
	plan := &MemPlan{MBPerNode: map[int]int64{}}
	need := mb
	for _, nid := range ids {
		take := min64(need, a.free[nid])
		if take > 0 {
			plan.MBPerNode[nid] = take
			need -= take
		}
	}
	return plan, need == 0
}

func (a *MemAllocator) AllocWithin(plan *MemPlan) error {
	for nid, mb := range plan.MBPerNode {
		if mb > a.free[nid] {
			return fmt.Errorf("node %d memory overcommit", nid)
		}
	}
	for nid, mb := range plan.MBPerNode {
		a.free[nid] -= mb
	}
	return nil
}

func (a *MemAllocator) Release(plan *MemPlan) {
	for nid, mb := range plan.MBPerNode {
		a.free[nid] += mb
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
