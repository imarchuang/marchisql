package storage

import (
	"fmt"
	"sort"
	"sync"
)

// TxMgr owns the transaction-id counter and the begin/commit/abort state
// machine. All durable status truth lives in the CLOG; TxMgr adds the
// in-memory active set and crash recovery.
//
// Transaction ids are 64-bit, monotonic, and start at 1 — 0 is reserved
// because Version.Xmax == 0 means "alive". PostgreSQL's 32-bit counter can
// wrap around, which is why it needs freezing and anti-wraparound vacuums;
// a 64-bit counter cannot wrap in any practical lifetime. See VERSIONS.md.
type TxMgr struct {
	mu     sync.Mutex
	clog   *Clog
	next   uint64
	active map[uint64]struct{}
}

// OpenTxMgr replays the CLOG under dir, recovers the txid counter, and
// aborts any transaction a crash left in_progress.
func OpenTxMgr(dir string) (*TxMgr, error) {
	c, err := OpenClog(dir)
	if err != nil {
		return nil, err
	}
	m := &TxMgr{clog: c, next: c.Max() + 1, active: make(map[uint64]struct{})}
	if err := m.recover(); err != nil {
		c.Close()
		return nil, err
	}
	return m, nil
}

// recover aborts every transaction whose last durable status is in_progress.
// Without a WAL there is nothing to redo: a transaction that never reached
// its commit record simply never happened, and its versions are invisible
// because the CLOG will never say "committed" for its txid.
func (m *TxMgr) recover() error {
	for _, txid := range m.clog.InProgress() {
		if err := m.clog.Record(txid, StatusAborted); err != nil {
			return err
		}
	}
	return nil
}

// Begin allocates a durable transaction id. The in_progress record is fsynced
// before the txid is handed out, so a version's xmin always refers to a txid
// the CLOG knows about.
func (m *TxMgr) Begin() (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	txid := m.next
	if err := m.clog.Record(txid, StatusInProgress); err != nil {
		return 0, err
	}
	m.next++
	m.active[txid] = struct{}{}
	return txid, nil
}

// Commit marks txid committed. The caller's heap writes are already durable
// (Heap.Append fsyncs), so this single CLOG record is the atomic publish
// step: before it the transaction never happened, after it its versions are
// visible to new snapshots.
func (m *TxMgr) Commit(txid uint64) error {
	return m.finish(txid, StatusCommitted)
}

// Abort marks txid aborted. Its versions stay on disk, invisible to everyone,
// until GC reclaims them (milestone 6).
func (m *TxMgr) Abort(txid uint64) error {
	return m.finish(txid, StatusAborted)
}

func (m *TxMgr) finish(txid uint64, s TxStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.active[txid]; !ok {
		return fmt.Errorf("tx %d not in progress (status: %s)", txid, m.clog.Status(txid))
	}
	if err := m.clog.Record(txid, s); err != nil {
		return err
	}
	delete(m.active, txid)
	return nil
}

// Status reports the durable status of txid.
func (m *TxMgr) Status(txid uint64) TxStatus {
	return m.clog.Status(txid)
}

// NextTxid is the id the next Begin will hand out — the "xmax" boundary of a
// snapshot taken right now (milestone 3).
func (m *TxMgr) NextTxid() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.next
}

// Active returns the currently in-progress txids in ascending order —
// milestone 3's snapshot active list.
func (m *TxMgr) Active() []uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]uint64, 0, len(m.active))
	for txid := range m.active {
		out = append(out, txid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Close closes the underlying CLOG.
func (m *TxMgr) Close() error {
	return m.clog.Close()
}
