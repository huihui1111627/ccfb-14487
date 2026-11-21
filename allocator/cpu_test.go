package allocator

import (
	"testing"

	"topomgr/topo"
	"topomgr/topo/numaset"
)

func testTopo(t *testing.T) *topo.Topology {
	t.Helper()
	tp, err := topo.Load("../configs/dual-socket.json")
	if err != nil {
		t.Fatal(err)
	}
	return tp
}

// Even-sized requests on a fresh pool must take whole physical cores:
// both SMT siblings together, no core split.
func TestCPUAllocWholeCores(t *testing.T) {
	tp := testTopo(t)
	a := NewCPUAllocator(tp)
	plan, ok := a.PlanWithin(4, numaset.Of(0))
	if !ok {
		t.Fatal("expected feasible plan")
	}
	if len(plan.CPUs) != 4 {
		t.Fatalf("want 4 cpus, got %d", len(plan.CPUs))
	}
	if len(plan.WholeCores) != 2 || len(plan.NewSplitCoreIDs) != 0 {
		t.Fatalf("want 2 whole cores and no split, got whole=%v split=%v",
			plan.WholeCores, plan.NewSplitCoreIDs)
	}
	// every taken cpu's sibling must also be taken
	owned := map[int]bool{}
	for _, c := range plan.CPUs {
		owned[c] = true
	}
	for _, c := range plan.CPUs {
		core := tp.Cores[tp.CoreOfCPU(c)]
		for _, sib := range core.CPUs {
			if !owned[sib] {
				t.Fatalf("core %d split: cpu %d taken but sibling %d free", core.ID, c, sib)
			}
		}
	}
}

// An odd request must split at most ONE new core and must not scatter
// single siblings across several intact cores.
func TestCPUAllocOddSplitsOneCore(t *testing.T) {
	tp := testTopo(t)
	a := NewCPUAllocator(tp)
	plan, ok := a.PlanWithin(3, numaset.Of(0))
	if !ok {
		t.Fatal("expected feasible plan")
	}
	if len(plan.NewSplitCoreIDs) != 1 {
		t.Fatalf("want exactly 1 newly split core, got %v", plan.NewSplitCoreIDs)
	}
	if len(plan.WholeCores) != 1 {
		t.Fatalf("want 1 whole core + 1 sibling, got whole=%v", plan.WholeCores)
	}
}

// When a core is already split, a later small request must fill the
// existing hole before breaking a new core.
func TestCPUAllocFillsExistingHole(t *testing.T) {
	tp := testTopo(t)
	a := NewCPUAllocator(tp)
	first, _ := a.PlanWithin(1, numaset.Of(0))
	if err := a.AllocWithin("a", first); err != nil {
		t.Fatal(err)
	}
	second, ok := a.PlanWithin(1, numaset.Of(0))
	if !ok {
		t.Fatal("expected feasible plan")
	}
	if len(second.NewSplitCoreIDs) != 0 {
		t.Fatalf("second 1-cpu alloc must reuse the split core, new splits=%v",
			second.NewSplitCoreIDs)
	}
	if tp.CoreOfCPU(second.CPUs[0]) != tp.CoreOfCPU(first.CPUs[0]) {
		t.Fatal("second allocation should land on the sibling of the first")
	}
}

// Released CPUs return to the pool and the pool version bumps, so shared
// containers can detect the change and refresh their usable set.
func TestCPUReleaseRefreshesPool(t *testing.T) {
	tp := testTopo(t)
	a := NewCPUAllocator(tp)
	v0 := a.PoolVersion()
	plan, _ := a.PlanWithin(4, numaset.Of(0))
	_ = a.AllocWithin("a", plan)
	if a.PoolVersion() == v0 {
		t.Fatal("pool version must bump on alloc")
	}
	before := len(a.SharedCPUs())
	a.Release("a", plan.CPUs)
	after := len(a.SharedCPUs())
	if after <= before {
		t.Fatalf("shared pool must grow after release: before=%d after=%d", before, after)
	}
}

// Daemon reservation with the compact strategy stays on one node so the
// other nodes remain fully intact for aligned placement.
func TestDaemonReservationCompact(t *testing.T) {
	tp := testTopo(t)
	a := NewCPUAllocator(tp)
	cpus, err := a.ReserveDaemons(3, "compact")
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[int]bool{}
	for _, c := range cpus {
		nodes[tp.NodeOfCPU(c)] = true
	}
	if len(nodes) != 1 {
		t.Fatalf("compact reservation must land on one node, got %v", nodes)
	}
}

// The dumb interleaved baseline must actually break physical cores on an
// even-sized allocation that the aware allocator keeps whole.
func TestDumbInterleavesAndSplits(t *testing.T) {
	tp := testTopo(t)
	a := NewCPUAllocator(tp)
	plan, ok := a.DumbPlanWithin(4, tp.AllNodesSet())
	if !ok {
		t.Fatal("expected feasible dumb plan")
	}
	cores := map[int]bool{}
	for _, c := range plan.CPUs {
		cores[tp.CoreOfCPU(c)] = true
	}
	if len(cores) != 4 {
		t.Fatalf("dumb 4-cpu grab should touch 4 distinct cores, got %d", len(cores))
	}
	if len(plan.SplitCoreIDs) != 4 {
		t.Fatalf("all 4 touched cores should be split, got %v", plan.SplitCoreIDs)
	}
}

func TestFragStatsDetectsMixedCore(t *testing.T) {
	tp := testTopo(t)
	a := NewCPUAllocator(tp)
	plan, _ := a.PlanWithin(1, numaset.Of(0))
	_ = a.AllocWithin("a", plan)
	st := a.FragStats()
	if st.SplitCores == 0 {
		t.Fatal("a core with one owner and one free sibling must count as split")
	}
	if st.FreeSiblingLoose == 0 {
		t.Fatal("the free sibling must count as a stranded SMT slot")
	}
}
