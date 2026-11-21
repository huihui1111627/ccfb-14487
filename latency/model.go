// Package latency models per-block processing latency from the actual
// placement the manager produced. The same workload trace is fed to both
// the topology-aware and the dumb ("ignore topology") manager so the
// difference in the latency distribution is attributable to placement.
package latency

import (
	"math"
	"math/rand"
	"sort"

	"topomgr/manager"
	"topomgr/topo"
	"topomgr/topo/numaset"
)

// Model turns a placement into a latency sample (milliseconds).
//
//	base  : compute time on a clean local core (the ~3 ms floor)
//	every remote NUMA access adds (distance-10)*remoteStep ms, weighted
//	by how much memory the container touches on that node
//	device traffic crossing a node adds a fixed penalty
//	a SMT sibling owned by another exclusive container adds noisy-neighbor
//	contention; a sibling left in the shared (burstable) pool adds less
//	everything is multiplied by lognormal jitter reproducing the
//	3-vs-30 ms tail observed on the badly placed real node.
type Model struct {
	BaseMS        float64
	RemoteStepMS  float64 // per (distance-10) unit, per remote node touched
	DeviceCrossMS float64
	SMTNoiseMS    float64
	SMTBurstMS    float64
	JitterSigma   float64
}

func DefaultModel() Model {
	return Model{
		BaseMS: 3.0, RemoteStepMS: 0.35, DeviceCrossMS: 4.0,
		SMTNoiseMS: 6.0, SMTBurstMS: 1.5, JitterSigma: 0.28,
	}
}

// Sample draws one latency sample for one processing block of c.
func (m Model) Sample(rng *rand.Rand, t *topo.Topology, mg *manager.Manager, c *manager.Container) float64 {
	lat := m.BaseMS

	anchor := AnchorNode(t, c)
	lat += nodeSetPenalty(t, anchor, c.MemNodes, m.RemoteStepMS, c.Spec.MemMB)
	lat += nodeSetPenalty(t, anchor, c.HugeNodes, m.RemoteStepMS*1.5, c.Spec.Hugepages)

	for _, devID := range c.Devices {
		if t.Devices[devID].Node != anchor {
			lat += m.DeviceCrossMS
		}
	}

	if !c.Shared {
		lat += m.smtPenalty(t, mg, c)
	}

	jitter := rng.NormFloat64() * m.JitterSigma
	return lat * math.Exp(jitter)
}

// smtPenalty inspects each physical core the container occupies.
func (m Model) smtPenalty(t *topo.Topology, mg *manager.Manager, c *manager.Container) float64 {
	cpu := mg.CPUAlloc()
	owned := map[int]bool{}
	for _, lc := range c.CPUs {
		owned[lc] = true
	}
	seenCore := map[int]bool{}
	var p float64
	for _, lc := range c.CPUs {
		cid := t.CoreOfCPU(lc)
		if seenCore[cid] {
			continue
		}
		seenCore[cid] = true
		for _, sib := range t.Cores[cid].CPUs {
			if owned[sib] {
				continue
			}
			switch {
			case cpu.Owner(sib) != "":
				p += m.SMTNoiseMS // hard noisy neighbor on the sibling
			case !cpu.Reserved(sib):
				p += m.SMTBurstMS // sibling in shared/burstable pool
			}
		}
	}
	return p
}

func nodeSetPenalty(t *topo.Topology, anchor int, nodes numaset.Set,
	step float64, weight int64,
) float64 {
	var p float64
	for _, n := range nodes.Members() {
		d := t.Distance(anchor, n)
		if d > 10 {
			w := 1.0
			if weight > 0 {
				w = math.Log2(1 + float64(weight))
			}
			p += float64(d-10) * step * w
		}
	}
	return p
}

// AnchorNode is the NUMA node the container's CPU work executes on.
func AnchorNode(t *topo.Topology, c *manager.Container) int {
	if len(c.CPUs) > 0 {
		return t.NodeOfCPU(c.CPUs[0])
	}
	if len(c.SharedCPUs) > 0 {
		return t.NodeOfCPU(c.SharedCPUs[0])
	}
	return c.Nodes.Members()[0]
}

// Distribution holds latency samples and precomputed percentiles.
type Distribution struct {
	Samples []float64 `json:"samples"`
	P50     float64   `json:"p50"`
	P90     float64   `json:"p90"`
	P99     float64   `json:"p99"`
	Max     float64   `json:"max"`
	Mean    float64   `json:"mean"`
}

func Summarize(samples []float64) Distribution {
	d := Distribution{Samples: append([]float64(nil), samples...)}
	if len(samples) == 0 {
		return d
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	sum := 0.0
	for _, v := range sorted {
		sum += v
	}
	d.Mean = sum / float64(len(sorted))
	d.P50 = pct(sorted, 0.50)
	d.P90 = pct(sorted, 0.90)
	d.P99 = pct(sorted, 0.99)
	d.Max = sorted[len(sorted)-1]
	return d
}

func pct(sorted []float64, q float64) float64 {
	idx := int(q * float64(len(sorted)-1))
	return sorted[idx]
}
