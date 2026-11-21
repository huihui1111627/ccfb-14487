package manager

import (
	"sort"

	"topomgr/topo"
)

// Snapshot is the JSON payload rendered by the web dashboard.
type Snapshot struct {
	TopoName    string          `json:"topoName"`
	Dumb        bool            `json:"dumb"`
	PoolVersion int             `json:"poolVersion"`
	Sockets     []SocketView    `json:"sockets"`
	Devices     []DeviceView    `json:"devices"`
	Containers  []ContainerView `json:"containers"`
	Events      []Event         `json:"events"`
	Stats       Stats           `json:"stats"`
	Distances   [][]int         `json:"distances"`
	Frag        FragView        `json:"frag"`
}

type SocketView struct {
	ID    int        `json:"id"`
	Nodes []NodeView `json:"nodes"`
}

type NodeView struct {
	ID             int        `json:"id"`
	Socket         int        `json:"socket"`
	FreeMemMB      int64      `json:"freeMemMB"`
	TotalMemMB     int64      `json:"totalMemMB"`
	FreeHugepages  int64      `json:"freeHugepages"`
	TotalHugepages int64      `json:"totalHugepages"`
	Cores          []CoreView `json:"cores"`
	Devices        []string   `json:"devices"`
}

type CoreView struct {
	ID       int       `json:"id"`
	Siblings []CPUView `json:"siblings"`
	Split    bool      `json:"split"`
}

type CPUView struct {
	ID       int    `json:"id"`
	State    string `json:"state"` // free, reserved, shared, exclusive
	Owner    string `json:"owner,omitempty"`
	ColorIdx int    `json:"colorIdx"`
}

type DeviceView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Node      int    `json:"node"`
	Shareable bool   `json:"shareable"`
	Holder    string `json:"holder,omitempty"`
	Holders   int    `json:"holders"`
}

type ContainerView struct {
	ID           string   `json:"id"`
	GroupID      string   `json:"groupID,omitempty"`
	Shared       bool     `json:"shared"`
	Nodes        string   `json:"nodes"`
	CPUs         []int    `json:"cpus"`
	MemMB        int64    `json:"memMB"`
	Hugepages    int64    `json:"hugepages"`
	Devices      []string `json:"devices"`
	Policy       string   `json:"policy"`
	AllPreferred bool     `json:"allPreferred"`
	ColorIdx     int      `json:"colorIdx"`
	SharedCPUs   int      `json:"sharedCPUs"`
}

type FragView struct {
	TotalCores       int     `json:"totalCores"`
	SplitCores       int     `json:"splitCores"`
	WholeCores       int     `json:"wholeCores"`
	FragmentationPct float64 `json:"fragmentationPct"`
	LooseSiblingPct  float64 `json:"looseSiblingPct"`
}

// Snapshot builds a point-in-time, lock-consistent view of the machine.
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	colorOf := map[string]int{}
	idx := 0
	ids := make([]string, 0, len(m.containers))
	for id := range m.containers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		colorOf[id] = idx % 12
		idx++
	}

	snap := Snapshot{
		TopoName:    m.topo.Name,
		Dumb:        m.dumb,
		PoolVersion: m.cpu.PoolVersion(),
		Events:      append([]Event(nil), m.events...),
		Stats:       m.copyStatsLocked(),
	}

	for _, sock := range m.topo.Sockets {
		sv := SocketView{ID: sock.ID}
		for _, nid := range sock.Nodes {
			node := m.topo.Nodes[nid]
			nv := NodeView{
				ID: nid, Socket: sock.ID,
				FreeMemMB: m.mem.Free(nid), TotalMemMB: node.MemoryMB,
				FreeHugepages: m.huge.Free(nid), TotalHugepages: node.Hugepages,
				Devices: append([]string(nil), node.Devices...),
			}
			for _, cid := range node.Cores {
				core := m.topo.Cores[cid]
				cv := CoreView{ID: cid}
				ownersOnCore := map[string]bool{}
				for _, lc := range core.CPUs {
					view := CPUView{ID: lc}
					switch {
					case m.cpu.Reserved(lc):
						view.State = "reserved"
					case m.cpu.Owner(lc) != "":
						view.State = "exclusive"
						view.Owner = m.cpu.Owner(lc)
						view.ColorIdx = colorOf[view.Owner]
						ownersOnCore[view.Owner] = true
					default:
						view.State = "shared"
					}
					cv.Siblings = append(cv.Siblings, view)
				}
				cv.Split = len(ownersOnCore) > 1 || m.coreSplitLocked(core)
				nv.Cores = append(nv.Cores, cv)
			}
			sv.Nodes = append(sv.Nodes, nv)
		}
		snap.Sockets = append(snap.Sockets, sv)
	}

	devIDs := make([]string, 0, len(m.topo.Devices))
	for id := range m.topo.Devices {
		devIDs = append(devIDs, id)
	}
	sort.Strings(devIDs)
	for _, id := range devIDs {
		d := m.topo.Devices[id]
		dv := DeviceView{ID: id, Kind: d.Kind, Node: d.Node, Shareable: d.Shareable}
		dv.Holder, dv.Holders = m.devHolderLocked(id)
		snap.Devices = append(snap.Devices, dv)
	}

	for _, id := range ids {
		c := m.containers[id]
		cv := ContainerView{
			ID: id, GroupID: c.GroupID, Shared: c.Shared,
			Nodes: c.Nodes.String(), CPUs: append([]int(nil), c.CPUs...),
			MemMB: c.Spec.MemMB, Hugepages: c.Spec.Hugepages,
			Devices: append([]string(nil), c.Devices...),
			Policy:  string(c.Spec.Policy), AllPreferred: c.AllPreferred,
			ColorIdx: colorOf[id], SharedCPUs: len(c.SharedCPUs),
		}
		snap.Containers = append(snap.Containers, cv)
	}

	// distance matrix in node-id order
	nodeIDs := m.topo.AllNodes()
	mat := make([][]int, len(nodeIDs))
	for i, a := range nodeIDs {
		row := make([]int, len(nodeIDs))
		for j, b := range nodeIDs {
			row[j] = m.topo.Distance(a, b)
		}
		mat[i] = row
	}
	snap.Distances = mat

	frag := m.cpu.FragStats()
	snap.Frag = FragView{
		TotalCores: frag.TotalCores, SplitCores: frag.SplitCores,
		WholeCores:       frag.WholeCores,
		FragmentationPct: pct100(frag.SplitCores, frag.TotalCores),
		LooseSiblingPct:  pct100(frag.FreeSiblingLoose, m.topo.CPUs),
	}
	return snap
}

func (m *Manager) coreSplitLocked(c *topo.PhysCore) bool {
	states := map[string]bool{}
	for _, lc := range c.CPUs {
		switch {
		case m.cpu.Reserved(lc):
			states["reserved"] = true
		case m.cpu.Owner(lc) != "":
			states["owner:"+m.cpu.Owner(lc)] = true
		default:
			states["free"] = true
		}
	}
	return len(states) > 1
}

func (m *Manager) devHolderLocked(id string) (string, int) {
	// reach into the device allocator through a narrow helper
	return m.dev.Holder(id)
}

func (m *Manager) copyStatsLocked() Stats {
	s := m.stats
	s.RejectReasons = map[string]int{}
	for k, v := range m.stats.RejectReasons {
		s.RejectReasons[k] = v
	}
	return s
}

func pct100(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}
