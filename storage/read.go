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

// GetInTx is Get within a transaction: the transaction's own uncommitted
// writes are visible to it (read-your-own-writes), everything else goes
// through the pinned snapshot.
func (e *Engine) GetInTx(tx *Tx, key string) (*Version, ScanStats) {
	if v, ok := e.ownWrite(tx, key); ok {
		return v, ScanStats{Scanned: 1}
	}
	if e.ownDeleted(tx, key) {
		return nil, ScanStats{Scanned: 1, SkippedInvisible: 1}
	}
	return e.Get(tx.Snap, key)
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

// ScanInTx is Scan within a transaction: the snapshot supplies the committed
// view, then the transaction's own write set is overlaid on top — its
// updates replace the committed version, its inserts appear, its deletes
// remove the key.
func (e *Engine) ScanInTx(tx *Tx) ([]*Version, ScanStats) {
	out, st := e.Scan(tx.Snap)

	e.writesMu.Lock()
	ws := e.writes[tx.ID]
	e.writesMu.Unlock()
	if len(ws) == 0 {
		return out, st
	}

	// Index the snapshot result by key for overlay.
	byKey := make(map[string]*Version, len(out))
	for _, v := range out {
		byKey[v.Key] = v
	}
	for key, v := range ws {
		if v == nil {
			delete(byKey, key) // own delete
		} else {
			byKey[key] = v // own insert or update
		}
	}
	merged := make([]*Version, 0, len(byKey))
	for _, v := range byKey {
		merged = append(merged, v)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Key < merged[j].Key })
	return merged, st
}
