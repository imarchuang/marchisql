// Package storage is the MVCC engine: a versioned heap, a transaction
// manager, and a commit-status log (CLOG). The design notes in this
// directory (VERSIONS.md first) explain the choices; PLAN.md at the repo
// root is the milestone roadmap.
package storage

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
)

// Engine wires the milestone-1 pieces together over one data directory:
//
//	data/
//	  heap/000001.seg   version records, append-only
//	  clog/clog.jsonl   txid → status transitions
type Engine struct {
	Heap *Heap
	Tx   *TxMgr

	// mu serializes writes (Update/Delete). Reads never take it — that is
	// the point of MVCC. Writes are rare and conflict-checked against the
	// latest version, so a single writer lock is honest and sufficient.
	mu sync.Mutex

	// writes tracks each open transaction's uncommitted write set, for
	// read-your-own-writes and commit/abort bookkeeping.
	writesMu sync.Mutex
	writes   map[uint64]writeSet

	// snaps is the xmin of every open transaction's snapshot. The GC
	// horizon is the minimum. Guarded by mu, same as writes.
	snaps map[uint64]uint64

	gcStop chan struct{}
	gcDone chan struct{}
}

// Open opens (creating if necessary) the engine rooted at dataDir.
func Open(dataDir string) (*Engine, error) {
	h, err := OpenHeap(filepath.Join(dataDir, "heap"), 0)
	if err != nil {
		return nil, err
	}
	t, err := OpenTxMgr(filepath.Join(dataDir, "clog"))
	if err != nil {
		h.Close()
		return nil, err
	}
	return &Engine{Heap: h, Tx: t, writes: make(map[uint64]writeSet), snaps: make(map[uint64]uint64)}, nil
}

// Close stops the background GC, then closes the heap and the CLOG.
func (e *Engine) Close() error {
	if e.gcStop != nil {
		close(e.gcStop)
		e.gcStop = nil
		<-e.gcDone
	}
	return errors.Join(e.Heap.Close(), e.Tx.Close())
}

func jsonMarshal(v *Version) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
