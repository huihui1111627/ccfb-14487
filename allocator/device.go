package allocator

import (
	"fmt"

	"topomgr/topo"
	"topomgr/topo/numaset"
)

// DevAllocator arbitrates acquisition cards and accelerators.
// A non-shareable device is exclusively owned; shareable accelerators
// may be opened by several containers at once.
type DevAllocator struct {
	topo   *topo.Topology
	owner  map[string]string // device id -> container id (exclusive devices)
	shared map[string]int    // device id -> number of containers holding it
}

func NewDevAllocator(t *topo.Topology) *DevAllocator {
	return &DevAllocator{
		topo:   t,
		owner:  map[string]string{},
		shared: map[string]int{},
	}
}

// Holder reports the exclusive holder (or "" for shareable devices) and
// the number of current holders.
func (a *DevAllocator) Holder(id string) (string, int) {
	if a.topo.Devices[id].Shareable {
		return "", a.shared[id]
	}
	return a.owner[id], 0
}

// Candidates answers "which node sets contain all requested devices,
// with them currently available". Each device pins its home node into
// the bitmap; preferred means every requested device is on one node
// (the whole data path stays local).
func (a *DevAllocator) Candidates(want []string, within numaset.Set, dumb bool) []Candidate {
	if len(want) == 0 {
		return []Candidate{{Nodes: within, Preferred: true, Reason: "no device requested"}}
	}
	var required numaset.Set
	for _, id := range want {
		d, ok := a.topo.Devices[id]
		if !ok {
			return nil
		}
		if !d.Shareable && a.owner[id] != "" {
			return nil // acquisition card already locked
		}
		required |= numaset.Of(d.Node)
	}
	if !within.Contains(required) {
		return nil
	}
	preferred := required.Count() == 1
	reason := "devices spread over multiple nodes"
	if preferred {
		reason = "all devices local to one node"
	}
	if dumb {
		// Baseline ignores the pin and pretends the whole pool works;
		// the manager still must include the device nodes to take them,
		// so dumb just suppresses the preference flag.
		preferred = false
		reason = "dumb: device locality ignored"
	}
	return []Candidate{{Nodes: required, Preferred: preferred, Reason: reason}}
}

func (a *DevAllocator) AllocWithin(id string, want []string) (*DevPlan, error) {
	plan := &DevPlan{Devices: append([]string(nil), want...)}
	for _, devID := range want {
		d := a.topo.Devices[devID]
		if d.Shareable {
			a.shared[devID]++
			continue
		}
		if cur, ok := a.owner[devID]; ok && cur != id {
			return nil, fmt.Errorf("device %s held by %s", devID, cur)
		}
		a.owner[devID] = id
	}
	return plan, nil
}

func (a *DevAllocator) Release(id string, plan *DevPlan) {
	for _, devID := range plan.Devices {
		d := a.topo.Devices[devID]
		if d.Shareable {
			if a.shared[devID] > 0 {
				a.shared[devID]--
			}
			continue
		}
		if a.owner[devID] == id {
			delete(a.owner, devID)
		}
	}
}
