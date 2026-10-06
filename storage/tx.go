package storage

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Tx is a transaction handle: the txid plus the snapshot captured at begin.
// All reads within the transaction reuse this one snapshot — that reuse, and
// nothing else, is repeatable read.
type Tx struct {
	ID        uint64
	Snap      Snapshot
	StartedAt time.Time
}

// TakeSnapshot captures the read boundary for a transaction beginning now:
// the smallest in-flight txid, the next txid to be handed out, and the list
// of in-flight transactions. Three integers and a short list — nothing is
// copied, which is the entire trick.
//
// Callers that need the snapshot to be atomic with txid allocation use
// Engine.BeginTx, which holds the manager lock across both.
func (m *TxMgr) TakeSnapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// snapshotLocked is the shared body; the caller must hold m.mu.
func (m *TxMgr) snapshotLocked() Snapshot {
	active := make([]uint64, 0, len(m.active))
	for txid := range m.active {
		active = append(active, txid)
	}
	sort.Slice(active, func(i, j int) bool { return active[i] < active[j] })
	snap := Snapshot{Xmax: m.next, Active: active}
	if len(active) > 0 {
		snap.Xmin = active[0]
	} else {
		snap.Xmin = m.next
	}
	return snap
}

// BeginTx begins a transaction and captures its snapshot atomically with the
// txid allocation: no transaction can begin or end between the two, so the
// snapshot's active list is exact.
func (e *Engine) BeginTx() (*Tx, error) {
	m := e.Tx
	m.mu.Lock()
	defer m.mu.Unlock()

	txid := m.next
	if err := m.clog.Record(txid, StatusInProgress); err != nil {
		return nil, err
	}
	m.next++
	m.active[txid] = struct{}{}
	return &Tx{ID: txid, Snap: m.snapshotLocked(), StartedAt: time.Now()}, nil
}

// CommitTx commits the transaction's txid and drops its write set. The
// versions are already durable (Heap.Append fsyncs); the commit record is
// the atomic publish step.
func (e *Engine) CommitTx(tx *Tx) error {
	if err := e.Tx.Commit(tx.ID); err != nil {
		return err
	}
	e.dropWrites(tx.ID)
	return nil
}

// AbortTx aborts the transaction's txid and drops its write set. Its
// versions stay on disk, invisible to everyone, until GC.
func (e *Engine) AbortTx(tx *Tx) error {
	if err := e.Tx.Abort(tx.ID); err != nil {
		return err
	}
	e.dropWrites(tx.ID)
	return nil
}

// TxStore tracks open transaction handles by txid so HTTP requests can
// address them across calls. Milestone 3 keeps it in memory: a restart
// aborts everything anyway (crash recovery), so there is nothing to persist.
type TxStore struct {
	mu sync.Mutex
	m  map[uint64]*Tx
}

func NewTxStore() *TxStore {
	return &TxStore{m: make(map[uint64]*Tx)}
}

func (s *TxStore) Put(tx *Tx) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[tx.ID] = tx
}

func (s *TxStore) Get(txid uint64) (*Tx, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.m[txid]
	if !ok {
		return nil, fmt.Errorf("tx %d not open", txid)
	}
	return tx, nil
}

func (s *TxStore) Delete(txid uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, txid)
}
