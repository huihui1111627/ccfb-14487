package manager

import (
	"fmt"
	"time"

	"topomgr/topo"
	"topomgr/topo/numaset"
)

// now is indirected so tests/stress can reason about timestamps; in
// production it is wall clock.
var now = time.Now

func cpusToNodes(t *topo.Topology, cpus []int) numaset.Set {
	var s numaset.Set
	for _, cpu := range cpus {
		s |= numaset.Of(t.NodeOfCPU(cpu))
	}
	return s
}

func nodesString(t *topo.Topology, s numaset.Set) string {
	return s.String()
}

// SharedCPUsLabel renders a shared container's current pool for events.
func (c *Container) SharedCPUsLabel() string {
	var s numaset.Set
	for _, cpu := range c.SharedCPUs {
		// topo lookup is done by callers that have the machine; keep the
		// label cheap and just report the count here.
		_ = cpu
	}
	_ = s
	return fmt.Sprintf("shared-pool:%d-cpus", len(c.SharedCPUs))
}
