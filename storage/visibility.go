package storage

import "sort"

// Snapshot is the read boundary for a transaction: three integers and a
// short list. Taking one copies nothing — that is the entire trick of MVCC.
//
//	Xmin   smallest txid still in flight when the snapshot was taken
//	       (== Xmax when nothing was in flight)
//	Xmax   next txid to be handed out; the "now" of the snapshot
//	Active txids in flight at snapshot time, sorted ascending
//
// The active list exists because txid order is not commit order: a
// transaction holding a low txid can commit after a higher one. Anything in
// flight at snapshot time must stay invisible to this snapshot even if it
// commits later.
type Snapshot struct {
	Xmin   uint64   `json:"xmin"`
	Xmax   uint64   `json:"xmax"`
	Active []uint64 `json:"active,omitempty"`
}

// inFlight reports whether txid was in flight at snapshot time. The active
// list only covers [Xmin, Xmax): anything older had already ended, anything
// newer did not exist yet.
func (s Snapshot) inFlight(txid uint64) bool {
	if txid < s.Xmin || txid >= s.Xmax {
		return false
	}
	i := sort.Search(len(s.Active), func(i int) bool { return s.Active[i] >= txid })
	return i < len(s.Active) && s.Active[i] == txid
}

// LatestSnapshot is the milestone-2 trivial snapshot: {active: [], xmax:
// nextTxid}. Every read sees the latest committed state — read-committed-ish
// behavior, including non-repeatable reads, until milestone 3 pins real
// snapshots to transactions.
func LatestSnapshot(nextTxid uint64) Snapshot {
	return Snapshot{Xmin: nextTxid, Xmax: nextTxid}
}

// visible is the heart of MVCC: a pure function of a version, a snapshot,
// and the CLOG. No locks, no copies, no I/O — two integer fields checked
// against a commit-status map.
//
// The question is asked in two parts: was this version BORN in my view of
// the world, and is it still ALIVE in my view of the world. Each clause maps
// to an anomaly it prevents; see VISIBILITY.md for the line-by-line walk.
func visible(v *Version, snap Snapshot, clog *Clog) bool {
	// Born?
	if clog.Status(v.Xmin) != StatusCommitted {
		return false // creator aborted or never committed: dirty-read barrier
	}
	if v.Xmin >= snap.Xmax {
		return false // born after my snapshot
	}
	if snap.inFlight(v.Xmin) {
		return false // in flight at snapshot time, committed later
	}

	// Still alive?
	if v.Xmax == 0 {
		return true // nobody has touched it
	}
	switch clog.Status(v.Xmax) {
	case StatusAborted:
		return true // the killer failed; the version lives on
	case StatusInProgress:
		// The kill is not committed yet, so it is not a kill. This clause
		// must come before the snapshot-boundary checks: with the trivial
		// snapshot (empty active list) a live uncommitted killer is
		// otherwise indistinguishable from a committed one.
		return true
	}
	// The killer committed — but did my snapshot see the kill?
	if snap.inFlight(v.Xmax) {
		return true // in flight at snapshot time, committed later
	}
	if v.Xmax >= snap.Xmax {
		return true // killed after my snapshot
	}
	return false // killed before my snapshot
}
