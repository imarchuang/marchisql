package storage

import "sort"

// ScanStats feeds the X-Marchisql-* response headers: how many versions the
// read examined, and how many were skipped as invisible. The mechanism
// should be observable without a debugger.
type ScanStats struct {
	Scanned          int
	SkippedInvisible int
}

// Get returns the newest visible version of key under snap, or nil if the
// key has no visible version. The chain is walked newest-first; the first
// visible version wins.
func (e *Engine) Get(snap Snapshot, key string) (*Version, ScanStats) {
	var st ScanStats
	chain := e.Heap.Chain(key)
	for i := len(chain) - 1; i >= 0; i-- {
		st.Scanned++
		if visible(chain[i], snap, e.Tx.clog) {
			return chain[i], st
		}
		st.SkippedInvisible++
	}
	return nil, st
}

// Scan returns the newest visible version of every key, sorted by key.
//
// A consistent snapshot makes at most one version per key visible (see
// VISIBILITY.md), so walking newest-first and taking the first visible
// version per key is exact. Every version is still examined — and counted —
// so the stats describe the full heap walk.
func (e *Engine) Scan(snap Snapshot) ([]*Version, ScanStats) {
	var st ScanStats
	vs := e.Heap.Versions()
	emitted := make(map[string]struct{})
	out := make([]*Version, 0)
	for i := len(vs) - 1; i >= 0; i-- {
		v := vs[i]
		st.Scanned++
		if !visible(v, snap, e.Tx.clog) {
			st.SkippedInvisible++
			continue
		}
		if _, dup := emitted[v.Key]; dup {
			continue // a newer visible version of this key already won
		}
		emitted[v.Key] = struct{}{}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, st
}
