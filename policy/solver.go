// Package policy intersects the per-dimension candidate bitmaps and
// selects the narrowest node combination every dimension can accept.
// How far the combination is allowed to widen before the container is
// rejected is a policy decision.
package policy

import (
	"sort"

	"topomgr/allocator"
	"topomgr/topo"
	"topomgr/topo/numaset"
)

type Policy string

const (
	BestEffort     Policy = "best-effort"      // widen as far as needed
	Restricted     Policy = "restricted"       // widen at most to maxNodes (default 2)
	MustSingleNode Policy = "must-single-node" // never leave one node
)

type Request struct {
	ContainerID string
	Policy      Policy
	MaxNodes    int // for Restricted; <=0 means 2
	Dumb        bool

	CPUBound bool
	CPU      []allocator.Candidate

	MemBound bool
	Mem      []allocator.Candidate

	HugeBound bool
	Huge      []allocator.Candidate

	DevBound bool
	Dev      []allocator.Candidate
}

type Decision struct {
	Accepted     bool
	Nodes        numaset.Set
	AllPreferred bool
	Score        int
	RejectReason string
	PerDim       map[string]allocator.Candidate
}

type boundCands struct {
	name  string
	cands []allocator.Candidate
}

// Solve picks the tightest node set every active dimension offers.
func Solve(t *topo.Topology, req Request) Decision {
	dims := []boundCands{}
	if req.CPUBound {
		dims = append(dims, boundCands{"cpu", req.CPU})
	}
	if req.MemBound {
		dims = append(dims, boundCands{"mem", req.Mem})
	}
	if req.HugeBound {
		dims = append(dims, boundCands{"huge", req.Huge})
	}
	if req.DevBound {
		dims = append(dims, boundCands{"dev", req.Dev})
	}
	if len(dims) == 0 {
		return Decision{Accepted: true, Nodes: t.AllNodesSet()}
	}
	for _, d := range dims {
		if len(d.cands) == 0 {
			return Decision{RejectReason: "dimension " + d.name + " has no feasible node set"}
		}
	}

	// Union of every candidate set: anything usable.
	var universe numaset.Set
	for _, d := range dims {
		for _, c := range d.cands {
			universe |= c.Nodes
		}
	}

	// All distinct candidate sets, ordered tightest-first according to
	// the real SLIT geometry (popcount, then diameter, then distance sum).
	uniq := map[numaset.Set]bool{}
	for _, d := range dims {
		for _, c := range d.cands {
			uniq[c.Nodes] = true
		}
	}
	sets := make([]numaset.Set, 0, len(uniq))
	for s := range uniq {
		sets = append(sets, s)
	}
	sort.Slice(sets, func(i, j int) bool {
		a, b := sets[i], sets[j]
		if ca, cb := a.Count(), b.Count(); ca != cb {
			return ca < cb
		}
		if da, db := t.SetDiameter(a), t.SetDiameter(b); da != db {
			return da < db
		}
		if sa, sb := t.SetDistanceSum(a), t.SetDistanceSum(b); sa != sb {
			return sa < sb
		}
		return a < b
	})

	maxNodes := req.MaxNodes
	if maxNodes <= 0 {
		maxNodes = 2
	}

	for _, set := range sets {
		if req.Policy == MustSingleNode && set.Count() > 1 {
			return Decision{RejectReason: "must-single-node: no single node satisfies all dimensions"}
		}
		if req.Policy == Restricted && set.Count() > maxNodes {
			return Decision{RejectReason: "restricted: would need more than maxNodes nodes"}
		}
		per := map[string]allocator.Candidate{}
		allPref := true
		feasible := true
		for _, d := range dims {
			cand, ok := pickCandidate(d.cands, set)
			if !ok {
				feasible = false
				break
			}
			per[d.name] = cand
			if !cand.Preferred {
				allPref = false
			}
		}
		if !feasible {
			continue
		}
		if req.Dumb {
			allPref = false
		}
		return Decision{
			Accepted: true, Nodes: set, AllPreferred: allPref,
			Score:  set.Count()*100 + t.SetDiameter(set)*10 + t.SetDistanceSum(set),
			PerDim: per,
		}
	}

	// BestEffort: fall back to the full universe if every dimension can
	// serve within it (each allocator was queried over the full pool).
	if req.Policy == BestEffort {
		per := map[string]allocator.Candidate{}
		ok := true
		for _, d := range dims {
			cand, found := pickCandidate(d.cands, universe)
			if !found {
				ok = false
				break
			}
			per[d.name] = cand
		}
		if ok {
			return Decision{Accepted: true, Nodes: universe, PerDim: per,
				Score: universe.Count() * 100}
		}
	}
	return Decision{RejectReason: "no node set accepted by every dimension under policy " + string(req.Policy)}
}

// pickCandidate finds a dimension candidate whose node set is contained
// in the proposed set (the dimension can fit inside the wider answer).
// Preference flags of the wider set are inherited from the best offer.
func pickCandidate(cands []allocator.Candidate, set numaset.Set) (allocator.Candidate, bool) {
	best := -1
	for i, c := range cands {
		if !set.Contains(c.Nodes) {
			continue
		}
		if best == -1 || (c.Preferred && !cands[best].Preferred) || c.Nodes.Count() < cands[best].Nodes.Count() {
			best = i
		}
	}
	if best == -1 {
		return allocator.Candidate{}, false
	}
	return cands[best], true
}
