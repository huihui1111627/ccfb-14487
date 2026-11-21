// Command topomgr is the node resource topology manager entry point:
//
//	topomgr topo   --topo configs/dual-socket.json
//	topomgr stress --topo configs/dual-socket.json --events 4000 --seed 7
//	topomgr serve  --topo configs/dual-socket.json --addr :8080 [--dumb]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"time"

	"topomgr/manager"
	"topomgr/policy"
	"topomgr/sim"
	"topomgr/topo"
	"topomgr/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	topoPath := fs.String("topo", "configs/dual-socket.json", "topology description file")
	switch cmd {
	case "topo":
		_ = fs.Parse(os.Args[2:])
		runTopo(*topoPath)
	case "stress":
		events := fs.Int("events", 4000, "number of random events")
		seed := fs.Int64("seed", 7, "RNG seed (same trace for both managers)")
		pressure := fs.Int("pressure", 24, "target max live exclusive containers")
		daemonStrategy := fs.String("daemon-strategy", "compact", "compact|spread")
		jsonOut := fs.Bool("json", false, "emit machine-readable JSON report")
		_ = fs.Parse(os.Args[2:])
		runStress(*topoPath, *events, *seed, *pressure, *daemonStrategy, *jsonOut)
	case "serve":
		addr := fs.String("addr", ":8080", "listen address")
		dumb := fs.Bool("dumb", false, "run the topology-ignorant baseline")
		seed := fs.Int64("seed", 42, "live demo RNG seed")
		speed := fs.Float64("speed", 1.0, "demo event multiplier per second")
		_ = fs.Parse(os.Args[2:])
		runServe(*topoPath, *addr, *dumb, *seed, *speed)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: topomgr [topo|stress|serve] [flags]")
}

func loadTopology(path string) *topo.Topology {
	t, err := topo.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load topology: %v\n", err)
		os.Exit(1)
	}
	return t
}

func runTopo(path string) {
	t := loadTopology(path)
	fmt.Printf("topology %q: %d sockets, %d NUMA nodes, %d logical CPUs, %d devices\n",
		t.Name, len(t.Sockets), len(t.Nodes), t.CPUs, len(t.Devices))
	for _, sock := range t.Sockets {
		fmt.Printf(" socket %d: nodes %v\n", sock.ID, sock.Nodes)
		for _, nid := range sock.Nodes {
			n := t.Nodes[nid]
			fmt.Printf("   node %d: %d cores, %d MB RAM, %d hugepages, devices %v\n",
				nid, len(n.Cores), n.MemoryMB, n.Hugepages, n.Devices)
		}
	}
	fmt.Println(" distance matrix:")
	ids := t.AllNodes()
	fmt.Print("      ")
	for _, j := range ids {
		fmt.Printf(" N%-3d", j)
	}
	fmt.Println()
	for _, i := range ids {
		fmt.Printf(" N%-3d", i)
		for _, j := range ids {
			fmt.Printf(" %-4d", t.Distance(i, j))
		}
		fmt.Println()
	}
}

func runStress(path string, events int, seed int64, pressure int, daemonStrategy string, asJSON bool) {
	t := loadTopology(path)
	cfg := sim.DefaultConfig()
	cfg.Events = events
	cfg.Seed = seed
	cfg.MaxLiveExclusive = pressure
	cfg.DaemonStrategy = daemonStrategy

	aware, err := sim.Run(t, cfg, false)
	must(err)
	dumb, err := sim.Run(t, cfg, true)
	must(err)

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		must(enc.Encode(map[string]*sim.Report{"aware": aware, "baseline": dumb}))
		return
	}
	printReport("拓扑感知分配", aware)
	printReport("对照: 忽略拓扑 (dumb)", dumb)
	printDelta(aware, dumb)
}

func printReport(title string, r *sim.Report) {
	fmt.Println("================================================================")
	fmt.Printf(" %s\n", title)
	fmt.Println("================================================================")
	fmt.Printf(" 请求 %d  接受 %d (%.1f%%)  拒绝 %d (%.1f%%)  峰值在线 %d\n",
		r.Requests, r.Accepted, 100*r.AcceptRate, r.Rejected, 100*(1-r.AcceptRate), r.PeakLive)
	fmt.Printf(" 容器组: 请求 %d 接受 %d\n", r.GroupRequests, r.GroupAccepted)
	fmt.Printf(" 单节点命中率: %.1f%%   全维度偏好率: %.1f%%\n",
		100*r.SingleNodeRate, 100*r.PreferredRate)
	fmt.Printf(" 物理核拆散(峰值): %d/%d (%.1f%%)   峰值游离超线程槽: %.1f%%   结束时拆散: %.1f%%\n",
		r.PeakSplitCores, r.Frag.TotalCores, r.PeakFragPct, r.PeakLoosePct, r.FragmentationPct)
	l := r.Latency
	fmt.Printf(" 延迟模型 ms: p50 %.2f  p90 %.2f  p99 %.2f  max %.2f  mean %.2f  (样本 %d)\n",
		l.P50, l.P90, l.P99, l.Max, l.Mean, len(l.Samples))
	if len(r.RejectReasons) > 0 {
		fmt.Println(" 拒绝原因:")
		for k, v := range r.RejectReasons {
			fmt.Printf("   %5d  %s\n", v, k)
		}
	}
}

func printDelta(aware, dumb *sim.Report) {
	fmt.Println("================================================================")
	fmt.Println(" 差异 (aware 相对 dumb)")
	fmt.Println("================================================================")
	fmt.Printf(" 拒绝率:            %.1f%% -> %.1f%%\n",
		100*(1-dumb.AcceptRate), 100*(1-aware.AcceptRate))
	fmt.Printf(" 物理核拆散峰值:  %.1f%% -> %.1f%%\n",
		dumb.PeakFragPct, aware.PeakFragPct)
	fmt.Printf(" p99 延迟:          %.2f ms -> %.2f ms  (%.1f%%)\n",
		dumb.Latency.P99, aware.Latency.P99,
		100*(aware.Latency.P99-dumb.Latency.P99)/dumb.Latency.P99)
	fmt.Printf(" p90 延迟:          %.2f ms -> %.2f ms  (%.1f%%)\n",
		dumb.Latency.P90, aware.Latency.P90,
		100*(aware.Latency.P90-dumb.Latency.P90)/dumb.Latency.P90)
	fmt.Printf(" 均值延迟:          %.2f ms -> %.2f ms  (%.1f%%)\n",
		dumb.Latency.Mean, aware.Latency.Mean,
		100*(aware.Latency.Mean-dumb.Latency.Mean)/dumb.Latency.Mean)
}

// runServe starts the dashboard with a self-driving demo workload.
func runServe(path, addr string, dumb bool, seed int64, speed float64) {
	t := loadTopology(path)
	mg := manager.New(t, manager.Dumb(dumb))
	if _, err := mg.ReserveDaemons(2, "compact"); err != nil {
		must(err)
	}
	go demoDriver(mg, t, seed, speed)

	srv := web.NewServer(mg)
	fmt.Printf("dashboard on http://localhost%s  (topo=%s, dumb=%v)\n",
		addr, t.Name, dumb)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		must(err)
	}
}

// demoDriver keeps a small rolling population of containers so the page
// shows movement without any external client.
func demoDriver(mg *manager.Manager, t *topo.Topology, seed int64, speed float64) {
	rng := rand.New(rand.NewSource(seed))
	seq := 0
	var live []string
	ticker := time.NewTicker(time.Duration(float64(time.Second) / speed))
	defer ticker.Stop()
	policies := []policy.Policy{policy.BestEffort, policy.Restricted, policy.MustSingleNode}
	for range ticker.C {
		if len(live) > 0 && (len(live) > 14 || rng.Float64() < 0.4) {
			i := rng.Intn(len(live))
			_ = mg.Release(live[i])
			live = append(live[:i], live[i+1:]...)
			continue
		}
		seq++
		id := fmt.Sprintf("demo-%d", seq)
		n := 1 + rng.Intn(11)
		if rng.Float64() < 0.7 {
			n += n % 2
		}
		spec := manager.ContainerSpec{
			ID: id, CPUs: n, Exclusive: true,
			MemMB:  int64(2048 + rng.Intn(12288)),
			Policy: policies[rng.Intn(len(policies))],
		}
		if rng.Float64() < 0.3 {
			spec.Hugepages = int64(128 * (1 + rng.Intn(6)))
		}
		var acq, accel []string
		for _, d := range t.Devices {
			if d.Kind == "acq" {
				acq = append(acq, d.ID)
			} else {
				accel = append(accel, d.ID)
			}
		}
		if len(acq) > 0 && rng.Float64() < 0.4 {
			spec.Devices = append(spec.Devices, acq[rng.Intn(len(acq))])
		}
		if len(accel) > 0 && rng.Float64() < 0.3 {
			spec.Devices = append(spec.Devices, accel[rng.Intn(len(accel))])
		}
		if _, err := mg.Start(spec); err == nil {
			live = append(live, id)
		}
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
