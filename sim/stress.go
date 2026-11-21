// Package sim drives a long random trace of container start/stop events
// against a manager and turns the resulting placements into acceptance,
// fragmentation and latency metrics. The same trace is replayed against
// the topology-aware manager and the dumb baseline so the comparison is
// apples-to-apples.
package sim

import (
	"fmt"
	"math/rand"

	"topomgr/allocator"
	"topomgr/latency"
	"topomgr/manager"
	"topomgr/policy"
	"topomgr/topo"
)

type Config struct {
	Seed              int64
	Events            int
	MaxLiveExclusive  int // pressure level
	MaxCPUs           int // biggest single request (logical cpus)
	GroupProbability  float64
	SharedProbability float64
	DeviceProbability float64
	HugeProbability   float64
	DaemonCores       int
	DaemonStrategy    string
	LatencySamples    int // samples drawn per live container per tick
}

func DefaultConfig() Config {
	return Config{
		Seed: 1, Events: 4000, MaxLiveExclusive: 24, MaxCPUs: 12,
		GroupProbability: 0.18, SharedProbability: 0.15,
		DeviceProbability: 0.35, HugeProbability: 0.30,
		DaemonCores: 2, DaemonStrategy: "compact",
		LatencySamples: 4,
	}
}

type Report struct {
	Dumb             bool                 `json:"dumb"`
	Requests         int                  `json:"requests"`
	Accepted         int                  `json:"accepted"`
	Rejected         int                  `json:"rejected"`
	AcceptRate       float64              `json:"acceptRate"`
	RejectReasons    map[string]int       `json:"rejectReasons"`
	SingleNodeRate   float64              `json:"singleNodeRate"` // accepted exclusive containers landing on one node
	PreferredRate    float64              `json:"preferredRate"`
	GroupRequests    int                  `json:"groupRequests"`
	GroupAccepted    int                  `json:"groupAccepted"`
	Frag             allocator.FragStats  `json:"frag"`
	PeakSplitCores   int                  `json:"peakSplitCores"`      // highest split-core count observed mid-trace
	PeakFragPct      float64              `json:"peakFragPct"`         // highest fragmentation percentage observed mid-trace
	FragmentationPct float64              `json:"fragmentationPct"`    // split cores / total cores at end of trace
	LooseSiblingPct  float64              `json:"looseSiblingPct"`     // stranded free SMT slots / total logical cpus
	PeakLoosePct     float64              `json:"peakLooseSiblingPct"` // highest stranded-slot percentage observed
	Latency          latency.Distribution `json:"latency"`
	PeakLive         int                  `json:"peakLive"`
}

type runner struct {
	cfg    Config
	t      *topo.Topology
	mg     *manager.Manager
	rng    *rand.Rand
	seq    int
	live   []string // exclusive + shared container ids
	report Report
	model  latency.Model
	lat    []float64
}

// Run replays one generated trace. Pass dumb=true for the baseline
// manager that ignores topology and just carves the global pool.
func Run(t *topo.Topology, cfg Config, dumb bool) (*Report, error) {
	r := &runner{
		cfg:   cfg,
		t:     t,
		mg:    manager.New(t, manager.Dumb(dumb)),
		rng:   rand.New(rand.NewSource(cfg.Seed)),
		model: latency.DefaultModel(),
	}
	r.report.Dumb = dumb
	r.report.RejectReasons = map[string]int{}

	if _, err := r.mg.ReserveDaemons(cfg.DaemonCores, cfg.DaemonStrategy); err != nil {
		return nil, fmt.Errorf("daemon reserve: %w", err)
	}

	exclusiveLive := 0
	for step := 0; step < cfg.Events; step++ {
		// Stop phase: when over the pressure target, prefer releasing.
		overPressure := exclusiveLive >= cfg.MaxLiveExclusive
		if len(r.live) > 0 && (overPressure || r.rng.Float64() < 0.42) {
			idx := r.rng.Intn(len(r.live))
			id := r.live[idx]
			if c, ok := r.mg.Container(id); ok && !c.Shared {
				exclusiveLive--
			}
			_ = r.mg.Release(id)
			r.live = append(r.live[:idx], r.live[idx+1:]...)
		} else if r.rng.Float64() < cfg.GroupProbability {
			ids := r.startGroup()
			if len(ids) > 0 {
				r.live = append(r.live, ids...)
				for _, id := range ids {
					if c, ok := r.mg.Container(id); ok && !c.Shared {
						exclusiveLive++
					}
				}
			}
		} else {
			id, shared := r.startOne()
			if id != "" {
				r.live = append(r.live, id)
				if !shared {
					exclusiveLive++
				}
			}
		}
		if len(r.live) > r.report.PeakLive {
			r.report.PeakLive = len(r.live)
		}
		r.observeFrag()
		r.sampleLatency()
	}

	return r.finalize()
}

// observeFrag records the worst physical-core breakage seen while the
// node was populated; end-of-trace stats alone miss everything because
// released containers return cores to the pool before finalization.
func (r *runner) observeFrag() {
	frag := r.mg.CPUAlloc().FragStats()
	if frag.SplitCores > r.report.PeakSplitCores {
		r.report.PeakSplitCores = frag.SplitCores
	}
	pctFrag := pct100(frag.SplitCores, frag.TotalCores)
	if pctFrag > r.report.PeakFragPct {
		r.report.PeakFragPct = pctFrag
	}
	loose := pct100(frag.FreeSiblingLoose, r.t.CPUs)
	if loose > r.report.PeakLoosePct {
		r.report.PeakLoosePct = loose
	}
}

func pct100(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func (r *runner) startOne() (id string, shared bool) {
	r.seq++
	id = fmt.Sprintf("c%d", r.seq)
	spec := r.randomSpec(id)
	if spec.CPUs == 0 {
		shared = true
	}
	if _, err := r.mg.Start(spec); err != nil {
		return "", false
	}
	return id, shared
}

func (r *runner) startGroup() []string {
	r.seq++
	gid := fmt.Sprintf("g%d", r.seq)
	n := 2 + r.rng.Intn(2) // 2-3 containers per group
	specs := make([]manager.ContainerSpec, 0, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		cid := fmt.Sprintf("%s-%d", gid, i)
		spec := r.randomSpec(cid)
		specs = append(specs, spec)
		ids = append(ids, cid)
	}
	colocate := r.rng.Float64() < 0.6
	if err := r.mg.StartGroup(manager.GroupSpec{
		ID: gid, Colocate: colocate, Containers: specs,
	}); err != nil {
		return nil
	}
	return ids
}

func (r *runner) randomSpec(id string) manager.ContainerSpec {
	spec := manager.ContainerSpec{
		ID:     id,
		Policy: r.randomPolicy(),
		MemMB:  int64(2048 + r.rng.Intn(14336)),
	}
	if r.rng.Float64() >= r.cfg.SharedProbability {
		// Exclusive: integer logical CPUs, biased toward even numbers so
		// whole physical cores (SMT=2) usually satisfy the request.
		n := 1 + r.rng.Intn(r.cfg.MaxCPUs)
		if r.rng.Float64() < 0.7 {
			n += n % 2 // round up to even -> whole-core allocations
		}
		spec.CPUs = n
		spec.Exclusive = true
	}
	if r.rng.Float64() < r.cfg.HugeProbability {
		spec.Hugepages = int64(128 * (1 + r.rng.Intn(8)))
	}
	if r.rng.Float64() < r.cfg.DeviceProbability {
		spec.Devices = r.pickDevices()
	}
	return spec
}

func (r *runner) randomPolicy() policy.Policy {
	// Mostly best-effort so both managers keep the node populated; the
	// stricter policies stay present enough to exercise rejection paths.
	switch r.rng.Intn(10) {
	case 0, 1:
		return policy.MustSingleNode
	case 2, 3, 4:
		return policy.Restricted
	default:
		return policy.BestEffort
	}
}

func (r *runner) pickDevices() []string {
	var acq, accel []string
	for _, d := range r.t.Devices {
		switch d.Kind {
		case "acq":
			acq = append(acq, d.ID)
		case "accel":
			accel = append(accel, d.ID)
		}
	}
	var out []string
	if len(acq) > 0 && r.rng.Float64() < 0.6 {
		out = append(out, acq[r.rng.Intn(len(acq))])
	}
	if len(accel) > 0 && r.rng.Float64() < 0.5 {
		out = append(out, accel[r.rng.Intn(len(accel))])
	}
	return out
}

func (r *runner) sampleLatency() {
	containers := r.mg.Containers()
	// Cap the per-tick sample count: once the node is heavily loaded,
	// randomly subsample containers so the trace stays O(events) rather
	// than O(events × live) while keeping the distribution representative.
	const maxPerTick = 12
	if len(containers) > maxPerTick {
		r.rng.Shuffle(len(containers), func(i, j int) {
			containers[i], containers[j] = containers[j], containers[i]
		})
		containers = containers[:maxPerTick]
	}
	for _, c := range containers {
		for k := 0; k < r.cfg.LatencySamples; k++ {
			r.lat = append(r.lat, r.model.Sample(r.rng, r.t, r.mg, c))
		}
	}
}

func (r *runner) finalize() (*Report, error) {
	st := r.mg.Stats()
	r.report.Requests = st.Requests
	r.report.Accepted = st.Accepted
	r.report.Rejected = st.Rejected
	r.report.RejectReasons = st.RejectReasons
	r.report.GroupRequests = st.GroupRequests
	r.report.GroupAccepted = st.GroupAccepted
	if st.Requests > 0 {
		r.report.AcceptRate = float64(st.Accepted) / float64(st.Requests)
	}
	if st.Accepted > 0 {
		r.report.PreferredRate = float64(st.PreferredHits) / float64(st.Accepted)
		r.report.SingleNodeRate = float64(st.SingleNodeHits) / float64(st.Accepted)
	}
	r.report.Frag = r.mg.CPUAlloc().FragStats()
	if r.report.Frag.TotalCores > 0 {
		r.report.FragmentationPct = 100 * float64(r.report.Frag.SplitCores) /
			float64(r.report.Frag.TotalCores)
	}
	if r.t.CPUs > 0 {
		r.report.LooseSiblingPct = 100 * float64(r.report.Frag.FreeSiblingLoose) /
			float64(r.t.CPUs)
	}
	r.report.Latency = latency.Summarize(r.lat)
	return &r.report, nil
}
