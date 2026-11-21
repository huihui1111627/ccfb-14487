// Package manager ties the four allocators and the policy solver into
// the node-level resource topology manager: container (and container
// group) admission, exclusive placement, shared-pool refresh on
// release, and snapshots for the web dashboard.
package manager

import (
	"fmt"
	"sync"
	"time"

	"topomgr/allocator"
	"topomgr/policy"
	"topomgr/topo"
	"topomgr/topo/numaset"
)

// ContainerSpec is one admission request. CPUs == 0 means the container
// does not pin cores and runs from the shared pool (burstable).
type ContainerSpec struct {
	ID        string        `json:"id"`
	CPUs      int           `json:"cpus"`
	Exclusive bool          `json:"exclusive"`
	MemMB     int64         `json:"memMB"`
	Hugepages int64         `json:"hugepages"`
	Devices   []string      `json:"devices"`
	Policy    policy.Policy `json:"policy"`
	MaxNodes  int           `json:"maxNodes,omitempty"`
}

// GroupSpec is a pod-like set of containers. Colocate=true forces every
// container in the group into one jointly chosen node set.
type GroupSpec struct {
	ID         string          `json:"id"`
	Colocate   bool            `json:"colocate"`
	Containers []ContainerSpec `json:"containers"`
}

// Container is the runtime record of an admitted container.
type Container struct {
	Spec         ContainerSpec `json:"spec"`
	GroupID      string        `json:"groupID,omitempty"`
	Nodes        numaset.Set   `json:"nodes"`
	CPUNodes     numaset.Set   `json:"cpuNodes"`
	CPUs         []int         `json:"cpus"`
	MemNodes     numaset.Set   `json:"memNodes"`
	HugeNodes    numaset.Set   `json:"hugeNodes"`
	DevNodes     numaset.Set   `json:"devNodes"`
	Devices      []string      `json:"devices"`
	AllPreferred bool          `json:"allPreferred"`
	Shared       bool          `json:"shared"`
	SharedCPUs   []int         `json:"sharedCPUs,omitempty"`
	StartedAt    time.Time     `json:"startedAt"`

	cpuPlan  *allocator.CPUPlan
	memPlan  *allocator.MemPlan
	hugePlan *allocator.HugePlan
	devPlan  *allocator.DevPlan
}

// Event is a timeline entry for the dashboard.
type Event struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"` // start, reject, release, refresh, reserve
	ID     string    `json:"id"`
	Detail string    `json:"detail"`
	Nodes  string    `json:"nodes,omitempty"`
}

// Stats are the acceptance counters used by the stress report.
type Stats struct {
	Requests       int            `json:"requests"`
	Accepted       int            `json:"accepted"`
	Rejected       int            `json:"rejected"`
	RejectReasons  map[string]int `json:"rejectReasons"`
	SingleNodeHits int            `json:"singleNodeHits"`
	PreferredHits  int            `json:"preferredHits"`
	GroupRequests  int            `json:"groupRequests"`
	GroupAccepted  int            `json:"groupAccepted"`
}

type Manager struct {
	mu sync.Mutex

	topo *topo.Topology
	cpu  *allocator.CPUAllocator
	mem  *allocator.MemAllocator
	huge *allocator.HugeAllocator
	dev  *allocator.DevAllocator

	dumb bool

	containers map[string]*Container
	groups     map[string]*groupRecord
	events     []Event
	stats      Stats
}

type groupRecord struct {
	spec     GroupSpec
	colocate bool
}

type Option func(*Manager)

func Dumb(d bool) Option { return func(m *Manager) { m.dumb = d } }

func New(t *topo.Topology, opts ...Option) *Manager {
	m := &Manager{
		topo:       t,
		cpu:        allocator.NewCPUAllocator(t),
		mem:        allocator.NewMemAllocator(t),
		huge:       allocator.NewHugeAllocator(t),
		dev:        allocator.NewDevAllocator(t),
		containers: map[string]*Container{},
		groups:     map[string]*groupRecord{},
	}
	m.stats.RejectReasons = map[string]int{}
	for _, o := range opts {
		o(m)
	}
	return m
}

func (m *Manager) Topo() *topo.Topology              { return m.topo }
func (m *Manager) CPUAlloc() *allocator.CPUAllocator { return m.cpu }
func (m *Manager) Dumb() bool                        { return m.dumb }

// ReserveDaemons reserves whole cores for node daemons.
func (m *Manager) ReserveDaemons(nCores int, strategy string) ([]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cpus, err := m.cpu.ReserveDaemons(nCores, strategy)
	if err != nil {
		return nil, err
	}
	m.record(Event{Kind: "reserve", ID: "daemons",
		Detail: fmt.Sprintf("%d daemon cores (%s strategy)", nCores, strategy),
		Nodes:  nodesString(m.topo, cpusToNodes(m.topo, cpus))})
	return cpus, nil
}

func (m *Manager) record(e Event) {
	e.At = time.Now()
	m.events = append(m.events, e)
	if len(m.events) > 500 {
		m.events = m.events[len(m.events)-500:]
	}
}

// Events returns a copy of the recent event timeline.
func (m *Manager) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, len(m.events))
	copy(out, m.events)
	return out
}

func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.stats
	s.RejectReasons = map[string]int{}
	for k, v := range m.stats.RejectReasons {
		s.RejectReasons[k] = v
	}
	return s
}
