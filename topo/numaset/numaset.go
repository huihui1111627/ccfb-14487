// Package numaset is an immutable bitmap of NUMA node ids (max 64 nodes).
package numaset

import "fmt"

// Set is a bitmask of NUMA node ids. Bit i represents node i.
type Set uint64

const Full Set = ^Set(0)

func Of(ids ...int) Set {
	var s Set
	for _, id := range ids {
		if id < 0 || id >= 64 {
			panic(fmt.Sprintf("numaset: node id %d out of range", id))
		}
		s |= 1 << uint(id)
	}
	return s
}

func (s Set) Has(id int) bool   { return s&(1<<uint(id)) != 0 }
func (s Set) Union(o Set) Set   { return s | o }
func (s Set) Inter(o Set) Set   { return s & o }
func (s Set) AndNot(o Set) Set  { return s &^ o }
func (s Set) Empty() bool       { return s == 0 }
func (s Set) Equals(o Set) bool { return s == o }
func (s Set) Contains(o Set) bool {
	return s&o == o
}

// Count returns the number of set bits (popcount).
func (s Set) Count() int {
	n := 0
	for s != 0 {
		s &= s - 1
		n++
	}
	return n
}

// Members returns the sorted list of node ids in the set.
func (s Set) Members() []int {
	out := make([]int, 0, s.Count())
	for i := 0; i < 64; i++ {
		if s.Has(i) {
			out = append(out, i)
		}
	}
	return out
}

func (s Set) String() string {
	return fmt.Sprintf("N%v", s.Members())
}
