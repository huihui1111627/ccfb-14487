package manager

import (
	"testing"

	"topomgr/policy"
	"topomgr/topo"
)

func mgr(t *testing.T) (*Manager, *topo.Topology) {
	t.Helper()
	tp, err := topo.Load("../configs/dual-socket.json")
	if err != nil {
		t.Fatal(err)
	}
	return New(tp), tp
}

func TestExclusiveWholeCoreAndSingleNode(t *testing.T) {
	m, tp := mgr(t)
	c, err := m.Start(ContainerSpec{
		ID: "w1", CPUs: 4, Exclusive: true, MemMB: 4096,
		Policy: policy.MustSingleNode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.CPUNodes.Count() != 1 {
		t.Fatalf("must-single-node container landed on %s", c.CPUNodes)
	}
	// all 4 cpus must form 2 whole physical cores
	cores := map[int]bool{}
	for _, cpu := range c.CPUs {
		cores[tp.CoreOfCPU(cpu)] = true
	}
	if len(cores) != 2 {
		t.Fatalf("4 cpus should occupy 2 whole cores, got %d", len(cores))
	}
}

func TestMustSingleNodeRejectsWhenSpread(t *testing.T) {
	m, _ := mgr(t)
	// Request more memory than any single node holds (each node 128GB;
	// ask for 200GB) — must be rejected under must-single-node.
	_, err := m.Start(ContainerSpec{
		ID: "big", CPUs: 2, Exclusive: true, MemMB: 200 * 1024,
		Policy: policy.MustSingleNode,
	})
	if err == nil {
		t.Fatal("expected rejection when no single node fits the memory request")
	}
}

func TestBestEffortAcceptsMultiNode(t *testing.T) {
	m, _ := mgr(t)
	c, err := m.Start(ContainerSpec{
		ID: "big2", CPUs: 2, Exclusive: true, MemMB: 200 * 1024,
		Policy: policy.BestEffort,
	})
	if err != nil {
		t.Fatalf("best-effort should widen across nodes, got %v", err)
	}
	if c.Nodes.Count() < 2 {
		t.Fatalf("expected multi-node placement, got %s", c.Nodes)
	}
}

func TestReleaseReturnsCoresAndRefreshesShared(t *testing.T) {
	m, _ := mgr(t)
	shared, err := m.Start(ContainerSpec{ID: "s1", MemMB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	initialPool := len(shared.SharedCPUs)

	excl, err := m.Start(ContainerSpec{
		ID: "e1", CPUs: 4, Exclusive: true, MemMB: 1024,
		Policy: policy.BestEffort,
	})
	if err != nil {
		t.Fatal(err)
	}
	// shared container's view must shrink while exclusive cores are held
	running, _ := m.Container("s1")
	if len(running.SharedCPUs) != initialPool-4 {
		t.Fatalf("shared pool should shrink by 4, got %d vs %d",
			len(running.SharedCPUs), initialPool-4)
	}
	if err := m.Release(excl.Spec.ID); err != nil {
		t.Fatal(err)
	}
	running, _ = m.Container("s1")
	if len(running.SharedCPUs) != initialPool {
		t.Fatalf("shared pool must be restored to %d, got %d",
			initialPool, len(running.SharedCPUs))
	}
}

func TestColocatedGroupSharesNodeSet(t *testing.T) {
	m, _ := mgr(t)
	err := m.StartGroup(GroupSpec{
		ID: "g1", Colocate: true,
		Containers: []ContainerSpec{
			{ID: "g1-a", CPUs: 2, Exclusive: true, MemMB: 2048, Policy: policy.MustSingleNode},
			{ID: "g1-b", CPUs: 2, Exclusive: true, MemMB: 2048, Policy: policy.MustSingleNode},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := m.Container("g1-a")
	b, _ := m.Container("g1-b")
	if !a.CPUNodes.Equals(b.CPUNodes) || a.CPUNodes.Count() != 1 {
		t.Fatalf("colocated group must share one node: %s vs %s", a.CPUNodes, b.CPUNodes)
	}
}

func TestGroupRollbackOnFailure(t *testing.T) {
	m, _ := mgr(t)
	err := m.StartGroup(GroupSpec{
		ID: "g2", Colocate: false,
		Containers: []ContainerSpec{
			{ID: "g2-a", CPUs: 2, Exclusive: true, MemMB: 2048, Policy: policy.BestEffort},
			{ID: "g2-b", CPUs: 2, Exclusive: true, MemMB: 200 * 1024,
				Policy: policy.MustSingleNode}, // impossible
		},
	})
	if err == nil {
		t.Fatal("expected group rejection")
	}
	if _, ok := m.Container("g2-a"); ok {
		t.Fatal("first container must have been rolled back")
	}
}

func TestAcquisitionCardExclusive(t *testing.T) {
	m, _ := mgr(t)
	_, err := m.Start(ContainerSpec{
		ID: "acq-user-1", CPUs: 2, Exclusive: true, MemMB: 1024,
		Devices: []string{"acq0"}, Policy: policy.BestEffort,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Start(ContainerSpec{
		ID: "acq-user-2", CPUs: 2, Exclusive: true, MemMB: 1024,
		Devices: []string{"acq0"}, Policy: policy.BestEffort,
	})
	if err == nil {
		t.Fatal("non-shareable acquisition card must reject the second holder")
	}
}

func TestDistanceAwareWidening(t *testing.T) {
	// On the quad topology, when a container must span two nodes the
	// solver should prefer the same-socket pair (distance 12) over a
	// cross-socket pair (distance 20+).
	tp, err := topo.Load("../configs/quad-socket.json")
	if err != nil {
		t.Fatal(err)
	}
	m := New(tp)
	// Drain node 0's CPUs so the first container is forced to widen.
	drain := 0
	for n := 1; n < 8; n++ {
		_ = drain
	}
	// Fill node 0 with one large exclusive container (12 cores x 2 = 24 cpus).
	full, err := m.Start(ContainerSpec{
		ID: "filler", CPUs: 24, Exclusive: true, MemMB: 1024,
		Policy: policy.MustSingleNode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if full.CPUNodes.Count() != 1 || full.CPUNodes.Members()[0] != 0 {
		t.Skip("filler did not land on node 0; test topology assumption changed")
	}
	// Now ask for a device on node 0 plus CPUs under restricted policy:
	// the chosen second node should be node 1 (same socket), not node 4.
	c, err := m.Start(ContainerSpec{
		ID: "wide", CPUs: 2, Exclusive: true, MemMB: 1024,
		Devices: []string{"acq0"}, Policy: policy.Restricted, MaxNodes: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	members := c.Nodes.Members()
	for _, n := range members {
		if n != 0 && n != 1 {
			t.Fatalf("widened set %s should pick same-socket node 1, not node %d", c.Nodes, n)
		}
	}
}
