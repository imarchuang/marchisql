package storage

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrSerializationFailure is the first-writer-wins conflict: another
// transaction committed a write to this key after my snapshot. Under
// snapshot isolation the only correct response is to abort — retrying the
// write would be a lost update.
var ErrSerializationFailure = errors.New("serialization_failure")

// ErrTxConflict is the fail-fast form of a conflict with a transaction that
// is still in progress: it is writing this key, or it holds the row lock
// (FOR UPDATE or its own write). PostgreSQL would block until the holder
// ends; this engine returns the error and the caller aborts.
var ErrTxConflict = errors.New("transaction_conflict")

// writeSet is the transaction's own uncommitted writes, keyed by row key.
// Reads consult it before the heap so a transaction sees its own writes
// (its snapshot hides them: their xmin is its own uncommitted txid).
// Commit and abort also use it for bookkeeping.
type writeSet map[string]*Version

// Update writes a new version of key within tx, first-writer-wins on the
// latest version:
//
//  1. Find the latest version of the key, ignoring the snapshot.
//  2. If its xmax is held by another in-progress tx → fail fast.
//  3. If that holder committed after my snapshot → serialization_failure.
//  4. Otherwise append my new version (xmin = me) and stamp the old xmax.
//
// The asymmetry is the whole point: reads use the snapshot, writes confront
// the latest version. That single sentence is the seed of write skew.
func (e *Engine) Update(tx *Tx, key string, fields json.RawMessage) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// The version to confront is the latest one NOT written by me — my own
	// uncommitted writes are tracked in the write set, and re-writing them
	// is always fine. holdLock also rejects a row another transaction has
	// locked with FOR UPDATE.
	if err := e.holdLock(tx, key); err != nil {
		return err
	}
	latest := e.Heap.latestOther(key, tx.ID)
	if latest != nil {
		// Stamp the old version's xmax = me.
		stamp := *latest
		stamp.Xmax = tx.ID
		if err := e.Heap.Append(stamp); err != nil {
			return err
		}
	}

	v := &Version{Key: key, Fields: fields, Xmin: tx.ID}
	if err := e.Heap.Append(*v); err != nil {
		return err
	}
	e.recordWrite(tx, key, v)
	return nil
}

// Delete removes key within tx: it stamps the latest version's xmax = me
// and appends no new version. The row stays on disk, invisible to new
// snapshots, until GC.
func (e *Engine) Delete(tx *Tx, key string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	latest := e.Heap.latestOther(key, tx.ID)
	if latest == nil {
		return fmt.Errorf("key %q not found", key)
	}
	if err := e.holdLock(tx, key); err != nil {
		return err
	}
	stamp := *latest
	stamp.Xmax = tx.ID
	if err := e.Heap.Append(stamp); err != nil {
		return err
	}
	e.recordWrite(tx, key, nil) // nil = tombstone in the write set
	return nil
}

// checkWritable applies first-writer-wins to the latest version of a key
// that was NOT written by me. The question is: who wrote this version, and
// what happened to them?
//
//	xmin == me          → impossible (latestOther excludes me)
//	xmin in progress    → another tx is writing it now; fail fast
//	xmin aborted        → that write failed; the version is dead; writable
//	xmin committed      → first-writer-wins: did they commit after my snapshot?
//	                      yes → serialization_failure (I lose)
//	                      no  → writable (their write is in my snapshot)
func (e *Engine) checkWritable(tx *Tx, latest *Version) error {
	writer := latest.Xmin
	switch e.Tx.Status(writer) {
	case StatusInProgress:
		return fmt.Errorf("%w: key %q is being written by tx %d", ErrTxConflict, latest.Key, writer)
	case StatusAborted:
		return nil // dead version; writable
	}
	// Committed. If they committed after my snapshot, I lose.
	if writer >= tx.Snap.Xmax || tx.Snap.inFlight(writer) {
		return fmt.Errorf("%w: key %q was written by tx %d after my snapshot", ErrSerializationFailure, latest.Key, writer)
	}
	return nil
}

// recordWrite tracks the transaction's own writes for read-your-own-writes
// and commit/abort bookkeeping.
func (e *Engine) recordWrite(tx *Tx, key string, v *Version) {
	e.writesMu.Lock()
	defer e.writesMu.Unlock()
	ws, ok := e.writes[tx.ID]
	if !ok {
		ws = make(writeSet)
		e.writes[tx.ID] = ws
	}
	ws[key] = v
}

// ownWrite returns the transaction's own uncommitted version of key, if any.
// The second return value is false for a delete (tombstone).
func (e *Engine) ownWrite(tx *Tx, key string) (*Version, bool) {
	e.writesMu.Lock()
	defer e.writesMu.Unlock()
	ws, ok := e.writes[tx.ID]
	if !ok {
		return nil, false
	}
	v, ok := ws[key]
	if !ok || v == nil {
		return nil, false
	}
	return v, true
}

// ownDeleted reports whether the transaction deleted key itself.
func (e *Engine) ownDeleted(tx *Tx, key string) bool {
	e.writesMu.Lock()
	defer e.writesMu.Unlock()
	ws, ok := e.writes[tx.ID]
	if !ok {
		return false
	}
	v, ok := ws[key]
	return ok && v == nil
}

// dropWrites discards the transaction's write set (on commit or abort).
func (e *Engine) dropWrites(txid uint64) {
	e.writesMu.Lock()
	defer e.writesMu.Unlock()
	delete(e.writes, txid)
}
