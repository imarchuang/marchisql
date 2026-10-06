package storage

import (
	"encoding/json"
	"sort"
	"time"
)

// VersionInfo is one link of a key's chain, with the CLOG status of the
// transactions that created and superseded it.
type VersionInfo struct {
	Key        string          `json:"key"`
	Fields     json.RawMessage `json:"fields"`
	Xmin       uint64          `json:"xmin"`
	XminStatus string          `json:"xmin_status"`
	Xmax       uint64          `json:"xmax"`
	XmaxStatus string          `json:"xmax_status"`
	Segment    string          `json:"segment"`
}

// Versions returns the chain for key, oldest first. xmax 0 is reported as
// "alive" because no transaction has superseded it.
func (e *Engine) Versions(key string) []VersionInfo {
	chain := e.Heap.Chain(key)
	out := make([]VersionInfo, len(chain))
	for i, v := range chain {
		xmaxStatus := "alive"
		if v.Xmax != 0 {
			xmaxStatus = e.Tx.Status(v.Xmax).String()
		}
		out[i] = VersionInfo{
			Key:        v.Key,
			Fields:     v.Fields,
			Xmin:       v.Xmin,
			XminStatus: e.Tx.Status(v.Xmin).String(),
			Xmax:       v.Xmax,
			XmaxStatus: xmaxStatus,
			Segment:    v.Segment,
		}
	}
	return out
}

// TxInfo is one open transaction: its snapshot and how long it has been open.
type TxInfo struct {
	ID       uint64   `json:"txid"`
	AgeMs    int64    `json:"age_ms"`
	Snapshot Snapshot `json:"snapshot"`
}

// Transactions lists open transactions, oldest txid first.
func (e *Engine) Transactions() []TxInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]TxInfo, 0, len(e.open))
	now := time.Now()
	for _, tx := range e.open {
		out = append(out, TxInfo{
			ID:       tx.ID,
			AgeMs:    now.Sub(tx.StartedAt).Milliseconds(),
			Snapshot: tx.Snap,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SegmentStat counts live and dead versions whose latest record is in one segment.
type SegmentStat struct {
	Segment string `json:"segment"`
	Live    int    `json:"live"`
	Dead    int    `json:"dead"`
}

// HeapReport is the horizon plus per-segment live/dead counts.
type HeapReport struct {
	Horizon  uint64        `json:"horizon"`
	Segments []SegmentStat `json:"segments"`
}

// HeapStats groups versions by the segment that holds their latest record.
// Dead uses the same rule as GC: xmax committed and older than the horizon.
func (e *Engine) HeapStats() HeapReport {
	e.mu.Lock()
	defer e.mu.Unlock()
	rep := HeapReport{Horizon: e.horizonLocked()}
	counts := map[string]*SegmentStat{}
	for _, v := range e.Heap.Versions() {
		st := counts[v.Segment]
		if st == nil {
			st = &SegmentStat{Segment: v.Segment}
			counts[v.Segment] = st
		}
		if e.dead(v, rep.Horizon) {
			st.Dead++
		} else {
			st.Live++
		}
	}
	rep.Segments = make([]SegmentStat, 0, len(counts))
	for _, st := range counts {
		rep.Segments = append(rep.Segments, *st)
	}
	sort.Slice(rep.Segments, func(i, j int) bool { return rep.Segments[i].Segment < rep.Segments[j].Segment })
	return rep
}

// SnapshotAge is how many transaction ids have been allocated since snap was
// taken. Zero means nothing has begun since.
func (e *Engine) SnapshotAge(snap Snapshot) uint64 {
	next := e.Tx.NextTxid()
	if next <= snap.Xmax {
		return 0
	}
	return next - snap.Xmax
}
