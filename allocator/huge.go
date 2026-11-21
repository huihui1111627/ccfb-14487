package allocator

import (
	"topomgr/topo"
	"topomgr/topo/numaset"
)

// HugeAllocator tracks free 2MB hugepages per NUMA node. Hugepages are
// a separate pool from ordinary RAM and cannot be split across nodes
// without losing TLB benefits, so single-node fits rank highest.
type HugeAllocator struct {
	topo *topo.Topology
	free map[int]int64
}

func NewHugeAllocator(t *topo.Topology) *HugeAllocator {
	a := &HugeAllocator{topo: t, free: map[int]int64{}}
	for id, n := range t.Nodes {
		a.free[id] = n.Hugepages
	}
	return a
}

func (a *HugeAllocator) Free(node int) int64 { return a.free[node] }

func (a *HugeAllocator) Candidates(pages int64, within numaset.Set, dumb bool) []Candidate {
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
		if total < pages {
			continue
		}
		preferred := set.Count() == 1 && a.free[set.Members()[0]] >= pages
		reason := "hugepages spilled"
		if preferred {
			reason = "hugepages local"
		}
		out = append(out, Candidate{Nodes: set, Preferred: preferred, Reason: reason})
		if len(out) >= maxCandidates || dumb {
			break
		}
	}
	return out
}

func (a *HugeAllocator) PlanWithin(pages int64, nodes numaset.Set) (*HugePlan, bool) {
	ids := nodes.Members()
	var total int64
	for _, nid := range ids {
		total += a.free[nid]
	}
	if total < pages {
		return nil, false
	}
	plan := &HugePlan{PagesPerNode: map[int]int64{}}
	need := pages
	for _, nid := range ids {
		take := min64(need, a.free[nid])
		if take > 0 {
			plan.PagesPerNode[nid] = take
			need -= take
		}
	}
	return plan, need == 0
}

func (a *HugeAllocator) AllocWithin(plan *HugePlan) {
	for nid, p := range plan.PagesPerNode {
		a.free[nid] -= p
	}
}

func (a *HugeAllocator) Release(plan *HugePlan) {
	for nid, p := range plan.PagesPerNode {
		a.free[nid] += p
	}
}
