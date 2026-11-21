package topo

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// ConfigDevice mirrors one entry of the "devices" array in the JSON file.
type ConfigDevice struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Node      int    `json:"node"`
	Shareable bool   `json:"shareable"`
}

// ConfigSocket describes a socket: how many NUMA nodes it splits into,
// the cores per node, the SMT width, and the memory/hugepage capacity.
type ConfigSocket struct {
	Nodes            int   `json:"nodes"`
	CoresPerNode     int   `json:"coresPerNode"`
	SMT              int   `json:"smt"`
	MemoryMBPerNode  int64 `json:"memoryMBPerNode"`
	HugepagesPerNode int64 `json:"hugepagesPerNode"`
}

// Config is the on-disk topology description format.
type Config struct {
	Name    string         `json:"name"`
	Sockets []ConfigSocket `json:"sockets"`
	Devices []ConfigDevice `json:"devices"`
	// Distances is optional and keyed "a-b"; values are ACPI-SLIT-like
	// numbers where 10 means local. Missing pairs are derived from
	// socket topology (same socket 12, cross socket 20/22 ...).
	Distances map[string]int `json:"distances,omitempty"`
}

// Load reads and validates a topology description file.
func Load(path string) (*Topology, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("topo: parse %s: %w", path, err)
	}
	return Build(cfg)
}

// Build turns a Config into a validated Topology, assigning logical CPU
// ids deterministically: core k on node n gets cpus k*SMT..k*SMT+SMT-1
// inside a global linear id space per node.
func Build(cfg Config) (*Topology, error) {
	if len(cfg.Sockets) == 0 {
		return nil, fmt.Errorf("topo: need at least one socket")
	}
	t := &Topology{
		Name:    cfg.Name,
		Nodes:   map[int]*NUMANode{},
		Cores:   map[int]*PhysCore{},
		Devices: map[string]*Device{},
	}
	cpuSeq, coreSeq, nodeSeq := 0, 0, 0
	for sid, sc := range cfg.Sockets {
		if sc.Nodes < 1 || sc.CoresPerNode < 1 || sc.SMT < 1 {
			return nil, fmt.Errorf("topo: socket %d has non-positive geometry", sid)
		}
		sock := &Socket{ID: sid}
		for n := 0; n < sc.Nodes; n++ {
			nid := nodeSeq
			nodeSeq++
			node := &NUMANode{
				ID:        nid,
				Socket:    sid,
				MemoryMB:  sc.MemoryMBPerNode,
				Hugepages: sc.HugepagesPerNode,
			}
			for c := 0; c < sc.CoresPerNode; c++ {
				cid := coreSeq
				coreSeq++
				cpus := make([]int, sc.SMT)
				for h := 0; h < sc.SMT; h++ {
					cpus[h] = cpuSeq
					cpuSeq++
				}
				pc := &PhysCore{ID: cid, Node: nid, Socket: sid, CPUs: cpus}
				t.Cores[cid] = pc
				node.Cores = append(node.Cores, cid)
			}
			sock.Nodes = append(sock.Nodes, nid)
			t.Nodes[nid] = node
		}
		t.Sockets = append(t.Sockets, sock)
	}
	t.CPUs = cpuSeq

	for _, d := range cfg.Devices {
		if _, ok := t.Nodes[d.Node]; !ok {
			return nil, fmt.Errorf("topo: device %s references unknown node %d", d.ID, d.Node)
		}
		if d.Kind != "acq" && d.Kind != "accel" {
			return nil, fmt.Errorf("topo: device %s has unknown kind %q", d.ID, d.Kind)
		}
		t.Devices[d.ID] = &Device{
			ID: d.ID, Kind: d.Kind, Node: d.Node, Shareable: d.Shareable,
		}
		t.Nodes[d.Node].Devices = append(t.Nodes[d.Node].Devices, d.ID)
	}
	for _, n := range t.Nodes {
		sort.Strings(n.Devices)
	}

	t.Distances = buildDistances(t, cfg.Distances)
	return t, nil
}

// buildDistances fills the full node-pair distance matrix. Explicit
// entries in the config win; otherwise same-socket nodes get 12 and
// cross-socket nodes get 20 (or 22 for machines with >2 sockets, so
// quad-socket rings do not pretend all remotes are equal).
func buildDistances(t *Topology, explicit map[string]int) map[int]map[int]int {
	dist := map[int]map[int]int{}
	ids := t.AllNodes()
	cross := 20
	if len(t.Sockets) > 2 {
		cross = 22
	}
	for _, a := range ids {
		dist[a] = map[int]int{}
		for _, b := range ids {
			d := 10
			if a != b {
				if t.Nodes[a].Socket == t.Nodes[b].Socket {
					d = 12
				} else {
					d = cross
				}
			}
			dist[a][b] = d
		}
	}
	for pair, v := range explicit {
		var a, b int
		if _, err := fmt.Sscanf(pair, "%d-%d", &a, &b); err != nil {
			continue
		}
		if _, ok := dist[a]; !ok {
			continue
		}
		if _, ok := dist[a][b]; !ok {
			continue
		}
		dist[a][b] = v
		dist[b][a] = v
	}
	return dist
}
