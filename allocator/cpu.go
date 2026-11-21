package allocator

import (
	"fmt"
	"sort"

	"topomgr/topo"
	"topomgr/topo/numaset"
)

const maxCandidates = 16

// CPUAllocator owns the exclusive-CPU pool. Logical CPUs are either:
// reserved for node daemons, exclusively owned by one container, or in
// the shared pool usable by non-exclusive (burstable) containers.
type CPUAllocator struct {
	topo     *topo.Topology
	reserved map[int]bool
	owner    map[int]string // logical cpu -> container id
	version  int            // bumped whenever the shared pool changes
}

func NewCPUAllocator(t *topo.Topology) *CPUAllocator {
	return &CPUAllocator{
		topo:     t,
		reserved: map[int]bool{},
		owner:    map[int]string{},
	}
}

// PoolVersion changes on every alloc/release/reserve; shared containers
// watch it to refresh their usable-CPU set.
func (a *CPUAllocator) PoolVersion() int { return a.version }

// Reserve marks CPUs as held by node daemons.
func (a *CPUAllocator) Reserve(cpus []int) error {
	for _, cpu := range cpus {
		if a.reserved[cpu] || a.owner[cpu] != "" {
			return fmt.Errorf("cpu %d not free for reservation", cpu)
		}
	}
	for _, cpu := range cpus {
		a.reserved[cpu] = true
	}
	a.version++
	return nil
}

// ReserveDaemons picks whole physical cores for system daemons.
// strategy "compact" (default) puts every daemon core on one NUMA node,
// keeping every other node fully intact for aligned container placement;
// "spread" round-robins whole cores across nodes instead.
func (a *CPUAllocator) ReserveDaemons(nCores int, strategy string) ([]int, error) {
	need := nCores
	var picked []int
	order := append([]int(nil), a.topo.AllNodes()...)
	if strategy == "spread" {
		// round-robin node order, repeatedly
		var rr []int
		for len(rr) < need {
			rr = append(rr, order...)
		}
		order = rr
	}
	for _, nid := range order {
		if need == 0 {
			break
		}
		for _, cid := range a.topo.Nodes[nid].Cores {
			if need == 0 {
				break
			}
			core := a.topo.Cores[cid]
			if a.coreFullyFree(core) {
				picked = append(picked, core.CPUs...)
				need--
			}
		}
	}
	if need > 0 {
		return nil, fmt.Errorf("not enough whole free cores to reserve %d daemon cores", nCores)
	}
	if err := a.Reserve(picked); err != nil {
		return nil, err
	}
	return picked, nil
}

func (a *CPUAllocator) coreFullyFree(c *topo.PhysCore) bool {
	for _, cpu := range c.CPUs {
		if a.reserved[cpu] || a.owner[cpu] != "" {
			return false
		}
	}
	return true
}

// freeSiblings returns the currently allocatable logical CPUs of a core.
func (a *CPUAllocator) freeSiblings(c *topo.PhysCore) []int {
	var out []int
	for _, cpu := range c.CPUs {
		if !a.reserved[cpu] && a.owner[cpu] == "" {
			out = append(out, cpu)
		}
	}
	return out
}

// planWithin dry-runs an allocation of nCPUs restricted to a node set.
// Allocation order:
//  1. whole untouched physical cores (both SMT siblings together),
//  2. free siblings of cores that are already split (fill the holes
//     before breaking any new core),
//  3. at most ONE newly split core for the odd remainder.
func (a *CPUAllocator) planWithin(nCPUs int, nodes numaset.Set) (*CPUPlan, bool) {
	var wholeCores, splitCores []int
	freeTotal := 0
	for _, nid := range nodes.Members() {
		for _, cid := range a.topo.Nodes[nid].Cores {
			core := a.topo.Cores[cid]
			free := a.freeSiblings(core)
			freeTotal += len(free)
			if len(free) == 0 {
				continue
			}
			if len(free) == len(core.CPUs) {
				wholeCores = append(wholeCores, cid)
			} else {
				splitCores = append(splitCores, cid)
			}
		}
	}
	if freeTotal < nCPUs {
		return nil, false
	}

	smt := len(a.topo.Cores[a.topo.Nodes[nodes.Members()[0]].Cores[0]].CPUs)
	plan := &CPUPlan{}
	need := nCPUs

	// 1. whole cores
	for len(wholeCores) > 0 && need >= smt {
		cid := wholeCores[0]
		wholeCores = wholeCores[1:]
		plan.WholeCores = append(plan.WholeCores, cid)
		plan.CPUs = append(plan.CPUs, a.topo.Cores[cid].CPUs...)
		need -= smt
	}

	// 2. fill from already-split cores (never create new breakage here)
	for _, cid := range splitCores {
		if need == 0 {
			break
		}
		free := a.freeSiblings(a.topo.Cores[cid])
		take := min(need, len(free))
		plan.CPUs = append(plan.CPUs, free[:take]...)
		plan.SplitCoreIDs = append(plan.SplitCoreIDs, cid)
		need -= take
	}

	// 3. one newly split core for the remainder (< smt)
	if need > 0 {
		if len(wholeCores) == 0 {
			return nil, false // defensive: freeTotal accounting guarantees this cannot happen
		}
		cid := wholeCores[0]
		core := a.topo.Cores[cid]
		plan.CPUs = append(plan.CPUs, core.CPUs[:need]...)
		plan.NewSplitCoreIDs = append(plan.NewSplitCoreIDs, cid)
		plan.SplitCoreIDs = append(plan.SplitCoreIDs, cid)
		need = 0
	}
	sort.Ints(plan.CPUs)
	return plan, true
}

// Candidates enumerates feasible node combinations, tightest first.
func (a *CPUAllocator) Candidates(nCPUs int, within numaset.Set, dumb bool) []Candidate {
	if dumb {
		// Baseline: ignore topology entirely, one pool over all nodes.
		if plan, ok := a.DumbPlanWithin(nCPUs, within); ok {
			return []Candidate{{Nodes: within, Preferred: false,
				Reason: "dumb: linear pool grab, " + planSummary(plan)}}
		}
		return nil
	}
	var out []Candidate
	for _, set := range enumerateSets(a.topo, within) {
		plan, ok := a.planWithin(nCPUs, set)
		if !ok {
			continue
		}
		preferred := len(plan.NewSplitCoreIDs) == 0
		reason := planSummary(plan)
		out = append(out, Candidate{
			Nodes: set, Preferred: preferred, Reason: reason,
		})
		if len(out) >= maxCandidates {
			break
		}
	}
	return out
}

func planSummary(p *CPUPlan) string {
	s := fmt.Sprintf("%d whole cores", len(p.WholeCores))
	if len(p.NewSplitCoreIDs) > 0 {
		s += fmt.Sprintf(", %d newly split", len(p.NewSplitCoreIDs))
	}
	return s
}

// PlanWithin re-runs the dry plan for a node set chosen by the solver.
func (a *CPUAllocator) PlanWithin(nCPUs int, nodes numaset.Set) (*CPUPlan, bool) {
	return a.planWithin(nCPUs, nodes)
}

// DumbPlanWithin is the baseline: grab n free logical CPUs in a
// topology-blind interleaved order inside the set, with no whole-core
// preference and no NUMA awareness. Real generic cpuset managers number
// cores across the package (0,1,2... are different physical cores, the
// SMT siblings come in a second id range), so linear allocation lands on
// distinct physical cores and strands their siblings — exactly the
// "single SMT brother shared with another container" failure this
// project exists to prevent.
func (a *CPUAllocator) DumbPlanWithin(nCPUs int, nodes numaset.Set) (*CPUPlan, bool) {
	plan := &CPUPlan{}
	taken := map[int]bool{}
	for _, cpu := range a.interleavedFreeCPUs(nodes) {
		if len(plan.CPUs) >= nCPUs {
			break
		}
		cid := a.topo.CoreOfCPU(cpu)
		plan.CPUs = append(plan.CPUs, cpu)
		taken[cpu] = true
		// A core is whole only if the plan ends up holding every sibling.
		// Record split status tentatively; it is reconciled after the loop.
		plan.SplitCoreIDs = append(plan.SplitCoreIDs, cid)
	}
	if len(plan.CPUs) < nCPUs {
		return nil, false
	}
	// Reconcile: a touched core is whole only when every sibling is in
	// the plan; otherwise it is a newly split core.
	seen := map[int]bool{}
	var split, whole []int
	for _, cpu := range plan.CPUs {
		cid := a.topo.CoreOfCPU(cpu)
		if seen[cid] {
			continue
		}
		seen[cid] = true
		core := a.topo.Cores[cid]
		allSiblings := true
		for _, sib := range core.CPUs {
			if !taken[sib] {
				allSiblings = false
				break
			}
		}
		if allSiblings {
			whole = append(whole, cid)
		} else {
			split = append(split, cid)
		}
	}
	plan.WholeCores = whole
	plan.SplitCoreIDs = split
	plan.NewSplitCoreIDs = append([]int(nil), split...)
	sort.Ints(plan.CPUs)
	plan.WholeCores = dedupSortedInts(plan.WholeCores)
	plan.SplitCoreIDs = dedupSortedInts(plan.SplitCoreIDs)
	plan.NewSplitCoreIDs = dedupSortedInts(plan.NewSplitCoreIDs)
	return plan, true
}

// interleavedFreeCPUs lists allocatable CPUs in package-interleaved
// order: first sibling of every core, then second sibling of every core.
func (a *CPUAllocator) interleavedFreeCPUs(nodes numaset.Set) []int {
	var cores []*topo.PhysCore
	for _, nid := range nodes.Members() {
		for _, cid := range a.topo.Nodes[nid].Cores {
			cores = append(cores, a.topo.Cores[cid])
		}
	}
	smt := 2
	for _, c := range cores {
		if len(c.CPUs) > smt {
			smt = len(c.CPUs)
		}
	}
	var out []int
	for h := 0; h < smt; h++ {
		for _, core := range cores {
			if h >= len(core.CPUs) {
				continue
			}
			cpu := core.CPUs[h]
			if !a.reserved[cpu] && a.owner[cpu] == "" {
				out = append(out, cpu)
			}
		}
	}
	return out
}

func dedupSortedInts(xs []int) []int {
	sort.Ints(xs)
	if len(xs) < 2 {
		return xs
	}
	out := xs[:1]
	for _, x := range xs[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}

// AllocWithin commits the given CPUs to container id.
func (a *CPUAllocator) AllocWithin(id string, plan *CPUPlan) error {
	for _, cpu := range plan.CPUs {
		if a.reserved[cpu] || a.owner[cpu] != "" {
			return fmt.Errorf("cpu %d already taken", cpu)
		}
	}
	for _, cpu := range plan.CPUs {
		a.owner[cpu] = id
	}
	a.version++
	return nil
}

// Release returns CPUs to the shared pool.
func (a *CPUAllocator) Release(id string, cpus []int) {
	for _, cpu := range cpus {
		if a.owner[cpu] == id {
			delete(a.owner, cpu)
		}
	}
	a.version++
}

// SharedCPUs is the pool available to non-exclusive containers.
func (a *CPUAllocator) SharedCPUs() []int {
	var out []int
	for cpu := 0; cpu < a.topo.CPUs; cpu++ {
		if !a.reserved[cpu] && a.owner[cpu] == "" {
			out = append(out, cpu)
		}
	}
	return out
}

// Owner returns the exclusive owner of a logical CPU, or "".
func (a *CPUAllocator) Owner(cpu int) string { return a.owner[cpu] }

// Reserved reports whether a CPU is held by node daemons.
func (a *CPUAllocator) Reserved(cpu int) bool { return a.reserved[cpu] }

// FragStats quantifies physical-core breakage for the acceptance report.
// FragStats quantifies physical-core breakage for the acceptance report.
type FragStats struct {
	TotalCores       int `json:"totalCores"`
	WholeCores       int `json:"wholeCores"`
	SplitCores       int `json:"splitCores"`
	FreeSiblingLoose int `json:"freeSiblingLoose"`
}

// FragStats quantifies physical-core breakage for the acceptance report.
// A core is "whole" only when every sibling shares one fate: all free,
// all daemon-reserved, or all owned by the same container. Any mix
// (owner + free, two different owners, owner + reserved ...) is split.
func (a *CPUAllocator) FragStats() FragStats {
	st := FragStats{}
	for _, core := range a.topo.Cores {
		st.TotalCores++
		free, reserved := 0, 0
		owners := map[string]int{}
		for _, cpu := range core.CPUs {
			switch {
			case a.reserved[cpu]:
				reserved++
			case a.owner[cpu] != "":
				owners[a.owner[cpu]]++
			default:
				free++
			}
		}
		homogeneous := (free == len(core.CPUs)) ||
			(reserved == len(core.CPUs)) ||
			(len(owners) == 1 && free == 0 && reserved == 0)
		if homogeneous {
			st.WholeCores++
			continue
		}
		st.SplitCores++
		st.FreeSiblingLoose += free
	}
	return st
}

// EnumerateSets lists non-empty subsets of within, ordered by:
// popcount, then real NUMA diameter (SLIT distance, not id adjacency),
// then sum distance to the tightest anchor node.
func EnumerateSets(t *topo.Topology, within numaset.Set) []numaset.Set {
	return enumerateSets(t, within)
}

// enumerateSets lists non-empty subsets of within, ordered by:
// popcount, then real NUMA diameter (SLIT distance, not id adjacency),
// then sum distance to the tightest anchor node.
func enumerateSets(t *topo.Topology, within numaset.Set) []numaset.Set {
	members := within.Members()
	n := len(members)
	var sets []numaset.Set
	for mask := 1; mask < 1<<uint(n); mask++ {
		var s numaset.Set
		for i := 0; i < n; i++ {
			if mask&(1<<uint(i)) != 0 {
				s |= numaset.Of(members[i])
			}
		}
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
	return sets
}
