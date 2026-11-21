package manager

import (
	"fmt"
	"sort"

	"topomgr/allocator"
	"topomgr/policy"
	"topomgr/topo"
	"topomgr/topo/numaset"
)

// StartGroup admits a whole container group atomically. With colocate,
// every container shares one jointly solved node set; otherwise each
// container is aligned independently.
func (m *Manager) StartGroup(g GroupSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.groups[g.ID]; exists {
		return fmt.Errorf("group %s already exists", g.ID)
	}
	for _, c := range g.Containers {
		if _, exists := m.containers[c.ID]; exists {
			return fmt.Errorf("container %s already exists", c.ID)
		}
	}

	m.stats.GroupRequests++

	if g.Colocate && len(g.Containers) > 0 {
		if err := m.startColocated(g); err != nil {
			m.stats.RejectReasons[err.Error()]++
			m.record(Event{Kind: "reject", ID: g.ID, Detail: "colocated group: " + err.Error()})
			return err
		}
		m.stats.GroupAccepted++
		m.groups[g.ID] = &groupRecord{spec: g, colocate: true}
		m.refreshAllShared()
		return nil
	}

	// Independent alignment: solve each container on its own, rolling
	// back everything if one fails so groups are admitted atomically.
	var started []*Container
	for _, spec := range g.Containers {
		c, err := m.admitLocked(spec, g.ID, 0)
		if err != nil {
			for _, done := range started {
				m.releaseLocked(done)
				delete(m.containers, done.Spec.ID)
			}
			m.stats.RejectReasons[err.Error()]++
			m.record(Event{Kind: "reject", ID: spec.ID, Detail: err.Error()})
			return err
		}
		started = append(started, c)
	}
	m.stats.GroupAccepted++
	m.groups[g.ID] = &groupRecord{spec: g}
	m.refreshAllShared()
	return nil
}

// startColocated solves one common node set for the whole group. Every
// dimension's candidates are computed per container; the union of the
// group's per-container needs must fit in the shared set, and each
// container's own allocations stay inside it.
func (m *Manager) startColocated(g GroupSpec) error {
	pol := g.Containers[0].Policy
	maxNodes := g.Containers[0].MaxNodes
	full := m.topo.AllNodesSet()

	// Per-container, per-dimension candidate lists; a group-level request
	// is built by intersecting on common sets.
	type perCand struct {
		cpu, mem, huge, dev []allocator.Candidate
	}
	all := make([]perCand, len(g.Containers))

	// Group-level feasibility: build merged candidate lists where each
	// candidate set must be offered by EVERY container for that dimension.
	mergeCPU, mergeMem, mergeHuge, mergeDev := true, true, true, true
	var cpuCands, memCands, hugeCands, devCands []allocator.Candidate
	for i, spec := range g.Containers {
		c, mem, huge, dev := m.candidatesLocked(spec, full)
		all[i] = perCand{c, mem, huge, dev}
		if spec.CPUs > 0 {
			if len(c) == 0 {
				mergeCPU = false
			}
			cpuCands = intersectCandidates(cpuCands, c)
		}
		if spec.MemMB > 0 {
			if len(mem) == 0 {
				mergeMem = false
			}
			memCands = intersectCandidates(memCands, mem)
		}
		if spec.Hugepages > 0 {
			if len(huge) == 0 {
				mergeHuge = false
			}
			hugeCands = intersectCandidates(hugeCands, huge)
		}
		if len(spec.Devices) > 0 {
			if len(dev) == 0 {
				mergeDev = false
			}
			devCands = unionCandidates(devCands, dev)
		}
	}
	if !mergeCPU || !mergeMem || !mergeHuge || !mergeDev {
		return fmt.Errorf("colocated group: a dimension cannot be satisfied anywhere")
	}

	req := policy.Request{
		ContainerID: g.ID, Policy: pol, MaxNodes: maxNodes, Dumb: m.dumb,
		CPUBound: len(cpuCands) > 0, CPU: cpuCands,
		MemBound: len(memCands) > 0, Mem: memCands,
		HugeBound: len(hugeCands) > 0, Huge: hugeCands,
		DevBound: len(devCands) > 0, Dev: devCands,
	}
	dec := policy.Solve(m.topo, req)
	if !dec.Accepted {
		return fmt.Errorf("colocated group solver: %s", dec.RejectReason)
	}

	// Commit every container inside the common set, rolling back on failure.
	var started []*Container
	for i, spec := range g.Containers {
		m.stats.Requests++
		c, err := m.commitLocked(spec, g.ID, dec.Nodes, all[i].cpu, all[i].mem, all[i].huge, all[i].dev)
		if err != nil {
			for _, done := range started {
				m.releaseLocked(done)
				delete(m.containers, done.Spec.ID)
			}
			m.stats.Rejected += len(g.Containers) - len(started)
			return err
		}
		m.stats.Accepted++
		c.AllPreferred = dec.AllPreferred
		started = append(started, c)
	}
	for _, c := range started {
		m.record(Event{Kind: "start", ID: c.Spec.ID,
			Detail: fmt.Sprintf("colocated group %s, %d cpus", g.ID, len(c.CPUs)),
			Nodes:  c.Nodes.String()})
	}
	return nil
}

// candidatesLocked asks every allocator for its feasible node sets.
func (m *Manager) candidatesLocked(spec ContainerSpec, within numaset.Set) (
	cpu, mem, huge, dev []allocator.Candidate,
) {
	if spec.CPUs > 0 {
		cpu = m.cpu.Candidates(spec.CPUs, within, m.dumb)
	}
	if spec.MemMB > 0 {
		mem = m.mem.Candidates(spec.MemMB, within, m.dumb)
	}
	if spec.Hugepages > 0 {
		huge = m.huge.Candidates(spec.Hugepages, within, m.dumb)
	}
	if len(spec.Devices) > 0 {
		dev = m.dev.Candidates(spec.Devices, within, m.dumb)
	}
	return
}

// Start admits one standalone container.
func (m *Manager) Start(spec ContainerSpec) (*Container, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.containers[spec.ID]; exists {
		return nil, fmt.Errorf("container %s already exists", spec.ID)
	}
	c, err := m.admitLocked(spec, "", 0)
	if err != nil {
		m.stats.RejectReasons[err.Error()]++
		m.record(Event{Kind: "reject", ID: spec.ID, Detail: err.Error()})
		return nil, err
	}
	m.refreshAllShared() // exclusive start shrinks the pool immediately
	return c, nil
}

// admitLocked runs the full intersect-and-solve flow for one container.
func (m *Manager) admitLocked(spec ContainerSpec, groupID string, forceNodes numaset.Set) (*Container, error) {
	m.stats.Requests++
	// Non-exclusive containers (CPUs == 0): no core pinning, they live
	// off the shared pool and follow it as it changes.
	if spec.CPUs == 0 && !spec.Exclusive {
		c, err := m.admitShared(spec, groupID)
		if err != nil {
			m.stats.Rejected++
			return nil, err
		}
		m.stats.Accepted++
		return c, nil
	}

	full := m.topo.AllNodesSet()
	cpuC, memC, hugeC, devC := m.candidatesLocked(spec, full)
	req := policy.Request{
		ContainerID: spec.ID, Policy: spec.Policy, MaxNodes: spec.MaxNodes,
		Dumb:     m.dumb,
		CPUBound: spec.CPUs > 0, CPU: cpuC,
		MemBound: spec.MemMB > 0, Mem: memC,
		HugeBound: spec.Hugepages > 0, Huge: hugeC,
		DevBound: len(spec.Devices) > 0, Dev: devC,
	}
	dec := policy.Solve(m.topo, req)
	if !dec.Accepted {
		m.stats.Rejected++
		return nil, fmt.Errorf("%s", dec.RejectReason)
	}
	nodes := dec.Nodes
	if forceNodes.Count() > 0 {
		nodes = forceNodes
	}
	c, err := m.commitLocked(spec, groupID, nodes, cpuC, memC, hugeC, devC)
	if err != nil {
		m.stats.Rejected++
		return nil, err
	}
	m.stats.Accepted++
	return c, nil
}

// commitLocked materializes a decision inside the given node set and
// performs allocator-level reservation, with rollback on any failure.
func (m *Manager) commitLocked(spec ContainerSpec, groupID string, nodes numaset.Set,
	cpuC, memC, hugeC, devC []allocator.Candidate,
) (*Container, error) {
	c := &Container{Spec: spec, GroupID: groupID, Nodes: nodes, StartedAt: now()}
	var rollback []func()

	if spec.CPUs > 0 {
		// Recompute against live allocator state inside the solved set:
		// candidates were generated before earlier group members committed,
		// so a previously feasible subset may now be occupied. Enumerate the
		// tightest live subset instead of trusting a stale candidate.
		plan, planNodes, ok := m.liveCPUPlan(spec.CPUs, nodes, cpuC)
		if !ok {
			return nil, fmt.Errorf("cpu: no live plan inside solved set %s", nodes)
		}
		if err := m.cpu.AllocWithin(spec.ID, plan); err != nil {
			return nil, err
		}
		c.cpuPlan = plan
		c.CPUs = append([]int(nil), plan.CPUs...)
		c.CPUNodes = planNodes
		rollback = append(rollback, func() { m.cpu.Release(spec.ID, plan.CPUs) })
	}

	if spec.MemMB > 0 {
		chosen, ok := fitCandidates(memC, nodes)
		if !ok {
			m.rollback(rollback)
			return nil, fmt.Errorf("mem: no candidate inside solved set %s", nodes)
		}
		plan, ok := m.mem.PlanWithin(spec.MemMB, chosen.Nodes)
		if !ok {
			m.rollback(rollback)
			return nil, fmt.Errorf("mem plan vanished")
		}
		if err := m.mem.AllocWithin(plan); err != nil {
			m.rollback(rollback)
			return nil, err
		}
		c.memPlan = plan
		c.MemNodes = chosen.Nodes
		rollback = append(rollback, func() { m.mem.Release(plan) })
	}

	if spec.Hugepages > 0 {
		chosen, ok := fitCandidates(hugeC, nodes)
		if !ok {
			m.rollback(rollback)
			return nil, fmt.Errorf("huge: no candidate inside solved set")
		}
		plan, ok := m.huge.PlanWithin(spec.Hugepages, chosen.Nodes)
		if !ok {
			m.rollback(rollback)
			return nil, fmt.Errorf("huge plan vanished")
		}
		m.huge.AllocWithin(plan)
		c.hugePlan = plan
		c.HugeNodes = chosen.Nodes
		rollback = append(rollback, func() { m.huge.Release(plan) })
	}

	if len(spec.Devices) > 0 {
		chosen, ok := fitCandidates(devC, nodes)
		if !ok {
			m.rollback(rollback)
			return nil, fmt.Errorf("dev: no candidate inside solved set")
		}
		plan, err := m.dev.AllocWithin(spec.ID, chosenDevices(spec.Devices, chosen))
		if err != nil {
			m.rollback(rollback)
			return nil, err
		}
		c.devPlan = plan
		c.Devices = plan.Devices
		c.DevNodes = chosen.Nodes
		rollback = append(rollback, func() { m.dev.Release(spec.ID, plan) })
	}

	if spec.CPUs > 0 {
		if c.CPUNodes.Count() == 1 {
			m.stats.SingleNodeHits++
		}
		m.stats.PreferredHits++
	}

	m.containers[spec.ID] = c
	m.record(Event{Kind: "start", ID: spec.ID,
		Detail: fmt.Sprintf("%d cpus, mem %dMB, huge %d, devs %v",
			len(c.CPUs), spec.MemMB, spec.Hugepages, spec.Devices),
		Nodes: nodes.String()})
	return c, nil
}

func (m *Manager) rollback(steps []func()) {
	for i := len(steps) - 1; i >= 0; i-- {
		steps[i]()
	}
}

// admitShared handles burststable containers: they take no exclusive
// cores, get a snapshot of the shared pool, and are refreshed on every
// pool version change.
func (m *Manager) admitShared(spec ContainerSpec, groupID string) (*Container, error) {
	c := &Container{
		Spec: spec, GroupID: groupID, Shared: true,
		Nodes: m.topo.AllNodesSet(), StartedAt: now(),
	}
	if spec.MemMB > 0 {
		plan, ok := m.mem.PlanWithin(spec.MemMB, m.topo.AllNodesSet())
		if !ok {
			return nil, fmt.Errorf("shared: not enough memory")
		}
		if err := m.mem.AllocWithin(plan); err != nil {
			return nil, err
		}
		c.memPlan = plan
		c.MemNodes = nodeSetOf(plan.MBPerNode)
	}
	if len(spec.Devices) > 0 {
		plan, err := m.dev.AllocWithin(spec.ID, spec.Devices)
		if err != nil {
			if c.memPlan != nil {
				m.mem.Release(c.memPlan)
			}
			return nil, err
		}
		c.devPlan = plan
		c.Devices = plan.Devices
		c.DevNodes = devNodeSet(m.topo, plan.Devices)
	}
	m.refreshSharedCPUs(c)
	m.containers[spec.ID] = c
	m.record(Event{Kind: "start", ID: spec.ID, Detail: "shared/burstable", Nodes: c.SharedCPUsLabel()})
	return c, nil
}

// Release stops a container and returns all its resources. Exclusive
// cores go back to the shared pool and every shared container is
// immediately refreshed to the new usable-CPU set.
func (m *Manager) Release(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.containers[id]
	if !ok {
		return fmt.Errorf("container %s not found", id)
	}
	m.releaseLocked(c)
	delete(m.containers, id)
	if c.GroupID != "" {
		// remove group when its last member leaves
		gr := m.groups[c.GroupID]
		alive := false
		for _, cc := range gr.spec.Containers {
			if _, still := m.containers[cc.ID]; still {
				alive = true
			}
		}
		if !alive {
			delete(m.groups, c.GroupID)
		}
	}
	m.refreshAllShared()
	m.record(Event{Kind: "release", ID: id, Detail: "resources returned to pools"})
	return nil
}

func (m *Manager) releaseLocked(c *Container) {
	if c.cpuPlan != nil {
		m.cpu.Release(c.Spec.ID, c.cpuPlan.CPUs)
	}
	if c.memPlan != nil {
		m.mem.Release(c.memPlan)
	}
	if c.hugePlan != nil {
		m.huge.Release(c.hugePlan)
	}
	if c.devPlan != nil {
		m.dev.Release(c.Spec.ID, c.devPlan)
	}
}

// refreshAllShared recomputes the usable set of every shared container.
func (m *Manager) refreshAllShared() {
	pool := m.cpu.SharedCPUs()
	for _, c := range m.containers {
		if !c.Shared {
			continue
		}
		before := len(c.SharedCPUs)
		c.SharedCPUs = append(c.SharedCPUs[:0], pool...)
		if before != len(pool) {
			m.record(Event{Kind: "refresh", ID: c.Spec.ID,
				Detail: fmt.Sprintf("shared cpuset now %d cpus (pool v%d)", len(pool), m.cpu.PoolVersion())})
		}
	}
}

func (m *Manager) refreshSharedCPUs(c *Container) {
	c.SharedCPUs = append([]int(nil), m.cpu.SharedCPUs()...)
}

// Container returns a runtime record by id.
func (m *Manager) Container(id string) (*Container, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.containers[id]
	return c, ok
}

// Containers returns all runtime records sorted by id.
func (m *Manager) Containers() []*Container {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Container, 0, len(m.containers))
	for _, c := range m.containers {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.ID < out[j].Spec.ID })
	return out
}

// PoolVersion exposes the shared-pool version.
func (m *Manager) PoolVersion() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cpu.PoolVersion()
}

// liveCPUPlan re-runs CPU planning against current allocator state.
// Tight subsets that were offered at request time are tried first, but
// if the allocator state changed (e.g. an earlier group member took
// cores), we widen to the full solved node set rather than failing when
// that set itself is still feasible.
func (m *Manager) liveCPUPlan(nCPUs int, nodes numaset.Set, offered []allocator.Candidate) (
	*allocator.CPUPlan, numaset.Set, bool,
) {
	allowed := map[numaset.Set]bool{}
	for _, c := range offered {
		allowed[c.Nodes] = true
	}
	for _, set := range m.topoSubsets(nodes) {
		if !allowed[set] {
			continue
		}
		if plan, ok := m.tryCPUPlan(nCPUs, set); ok {
			return plan, set, true
		}
	}
	// Fallback: the whole solved set, regardless of whether it was an
	// explicit per-dimension candidate — the solver already accepted it.
	if plan, ok := m.tryCPUPlan(nCPUs, nodes); ok {
		return plan, nodes, true
	}
	return nil, 0, false
}

func (m *Manager) tryCPUPlan(nCPUs int, set numaset.Set) (*allocator.CPUPlan, bool) {
	if m.dumb {
		return m.cpu.DumbPlanWithin(nCPUs, set)
	}
	return m.cpu.PlanWithin(nCPUs, set)
}

func (m *Manager) topoSubsets(nodes numaset.Set) []numaset.Set {
	return allocator.EnumerateSets(m.topo, nodes)
}

// --- small helpers ---

func fitCandidates(cands []allocator.Candidate, set numaset.Set) (allocator.Candidate, bool) {
	best := -1
	for i, c := range cands {
		if !set.Contains(c.Nodes) {
			continue
		}
		if best == -1 || c.Nodes.Count() < cands[best].Nodes.Count() ||
			(c.Preferred && !cands[best].Preferred) {
			best = i
		}
	}
	if best == -1 {
		return allocator.Candidate{}, false
	}
	return cands[best], true
}

// intersectCandidates keeps sets offered by both sides (group colocation
// requires a set every container can live in); preference is AND-ed.
func intersectCandidates(a, b []allocator.Candidate) []allocator.Candidate {
	if a == nil {
		out := make([]allocator.Candidate, len(b))
		copy(out, b)
		return out
	}
	var out []allocator.Candidate
	for _, x := range a {
		for _, y := range b {
			if x.Nodes.Equals(y.Nodes) {
				out = append(out, allocator.Candidate{
					Nodes: x.Nodes, Preferred: x.Preferred && y.Preferred,
					Reason: x.Reason + " | " + y.Reason,
				})
			}
		}
	}
	return out
}

// unionCandidates merges device requirements: the group set must contain
// every requested device's home node.
func unionCandidates(a, b []allocator.Candidate) []allocator.Candidate {
	if len(a) == 0 {
		return append([]allocator.Candidate(nil), b...)
	}
	var out []allocator.Candidate
	for _, x := range a {
		for _, y := range b {
			out = append(out, allocator.Candidate{
				Nodes:     x.Nodes.Union(y.Nodes),
				Preferred: x.Preferred && y.Preferred,
				Reason:    "group device union",
			})
		}
	}
	return out
}

func chosenDevices(want []string, chosen allocator.Candidate) []string {
	var out []string
	for _, d := range want {
		out = append(out, d)
	}
	return out
}

func nodeSetOf(m map[int]int64) numaset.Set {
	var s numaset.Set
	for n := range m {
		s |= numaset.Of(n)
	}
	return s
}

func devNodeSet(t *topo.Topology, devs []string) numaset.Set {
	var s numaset.Set
	for _, id := range devs {
		s |= numaset.Of(t.Devices[id].Node)
	}
	return s
}
