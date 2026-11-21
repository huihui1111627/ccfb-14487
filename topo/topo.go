// Package topo defines the simulated hardware topology: sockets, NUMA
// memory nodes, physical cores with SMT siblings, and attached devices.
package topo

import (
	"sort"

	"topomgr/topo/numaset"
)

// PhysCore is one physical core with its SMT siblings (logical CPUs).
type PhysCore struct {
	ID     int   `json:"id"`
	Node   int   `json:"node"`
	Socket int   `json:"socket"`
	CPUs   []int `json:"cpus"`
}

// Device is a PCIe device (acquisition card / accelerator).
type Device struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"` // "acq" (acquisition card) or "accel"
	Node      int    `json:"node"` // NUMA node it is physically wired to
	Shareable bool   `json:"shareable"`
}

// NUMANode is one memory node with RAM, hugepages and cores.
type NUMANode struct {
	ID        int      `json:"id"`
	Socket    int      `json:"socket"`
	MemoryMB  int64    `json:"memoryMB"`
	Hugepages int64    `json:"hugepages"` // number of 2MB hugepages
	Cores     []int    `json:"cores"`     // physical core ids
	Devices   []string `json:"devices"`
}

// Socket is one processor package, containing one or more NUMA nodes
// (plain single-node sockets, or SNC/sub-NUMA-clustering multi-node ones).
type Socket struct {
	ID    int   `json:"id"`
	Nodes []int `json:"nodes"`
}

// Topology is the full machine description.
type Topology struct {
	Name      string              `json:"name"`
	Sockets   []*Socket           `json:"sockets"`
	Nodes     map[int]*NUMANode   `json:"nodes"`
	Cores     map[int]*PhysCore   `json:"cores"`
	Devices   map[string]*Device  `json:"devices"`
	CPUs      int                 `json:"cpus"`
	Distances map[int]map[int]int `json:"distances"` // normalized ACPI SLIT values, 10 = local

	cpuCore map[int]int // logical cpu -> physical core id
}

// AllNodes returns all NUMA node ids sorted.
func (t *Topology) AllNodes() []int {
	ids := make([]int, 0, len(t.Nodes))
	for id := range t.Nodes {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// AllNodesSet is the bitmap containing every node.
func (t *Topology) AllNodesSet() numaset.Set {
	var s numaset.Set
	for id := range t.Nodes {
		s |= numaset.Of(id)
	}
	return s
}

// CPUsOf returns the logical CPU ids belonging to the given node set.
func (t *Topology) CPUsOf(nodes numaset.Set) []int {
	var cpus []int
	for _, nid := range nodes.Members() {
		for _, cid := range t.Nodes[nid].Cores {
			cpus = append(cpus, t.Cores[cid].CPUs...)
		}
	}
	sort.Ints(cpus)
	return cpus
}

// CoreOfCPU maps a logical CPU id to its physical core id.
func (t *Topology) CoreOfCPU(cpu int) int {
	if t.cpuCore == nil {
		t.cpuCore = map[int]int{}
		for cid, c := range t.Cores {
			for _, lc := range c.CPUs {
				t.cpuCore[lc] = cid
			}
		}
	}
	return t.cpuCore[cpu]
}

// NodeOfCPU maps a logical CPU id to its NUMA node.
func (t *Topology) NodeOfCPU(cpu int) int {
	return t.Cores[t.CoreOfCPU(cpu)].Node
}

// Distance returns the NUMA distance between two nodes (10 is local).
func (t *Topology) Distance(a, b int) int {
	if a == b {
		return 10
	}
	if row, ok := t.Distances[a]; ok {
		if d, ok := row[b]; ok {
			return d
		}
	}
	return 40 // unknown remote: worst plausible distance
}

// SetDiameter is the max pairwise distance inside a node set
// (10 for a single node). Used to score how "wide" an allocation is.
func (t *Topology) SetDiameter(s numaset.Set) int {
	members := s.Members()
	worst := 10
	for i := 0; i < len(members); i++ {
		for j := i + 1; j < len(members); j++ {
			if d := t.Distance(members[i], members[j]); d > worst {
				worst = d
			}
		}
	}
	return worst
}

// SetDistanceSum sums distance of every node to the closest anchor node
// (the set member minimizing total distance). Smaller = tighter cluster.
// Unlike node-id adjacency this follows the real SLIT table.
func (t *Topology) SetDistanceSum(s numaset.Set) int {
	members := s.Members()
	best := 0
	for _, anchor := range members {
		sum := 0
		for _, n := range members {
			sum += t.Distance(anchor, n)
		}
		if anchor == members[0] || sum < best {
			best = sum
		}
	}
	return best
}
