// Package storage is the MVCC engine: a versioned heap, a transaction
// manager, and a commit-status log (CLOG). The design notes in this
// directory (VERSIONS.md first) explain the choices; PLAN.md at the repo
// root is the milestone roadmap.
package storage

import (
	"errors"
	"path/filepath"
)

// Engine wires the milestone-1 pieces together over one data directory:
//
//	data/
//	  heap/000001.seg   version records, append-only
//	  clog/clog.jsonl   txid → status transitions
type Engine struct {
	Heap *Heap
	Tx   *TxMgr
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
	return &Engine{Heap: h, Tx: t}, nil
}

// Close closes the heap and the CLOG.
func (e *Engine) Close() error {
	return errors.Join(e.Heap.Close(), e.Tx.Close())
}
