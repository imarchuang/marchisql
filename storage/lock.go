package storage

import "fmt"

// holdLock takes the row lock for key and applies first-writer-wins to the
// latest version not written by tx. Caller holds e.mu.
//
// A lock held by another in-progress transaction is ErrTxConflict (fail-fast,
// same choice as milestone 4: we do not block). A version committed after
// tx's snapshot is ErrSerializationFailure. Locks are memory-only and are
// dropped when the holder commits or aborts.
func (e *Engine) holdLock(tx *Tx, key string) error {
	if holder, ok := e.locks[key]; ok && holder != tx.ID {
		if e.Tx.Status(holder) == StatusInProgress {
			return fmt.Errorf("%w: key %q is locked by tx %d", ErrTxConflict, key, holder)
		}
		delete(e.locks, key)
	}
	if latest := e.Heap.latestOther(key, tx.ID); latest != nil {
		if err := e.checkWritable(tx, latest); err != nil {
			return err
		}
	}
	e.locks[key] = tx.ID
	return nil
}

// releaseLocks drops every row lock held by txid. Caller holds e.mu.
func (e *Engine) releaseLocks(txid uint64) {
	for key, holder := range e.locks {
		if holder == txid {
			delete(e.locks, key)
		}
	}
}

// ScanForUpdate returns the rows visible to tx that match where, and locks
// each of them. where is the same field=value predicate as GET /scan.
// An empty where locks every visible row.
//
// The scan itself takes no engine lock. Locks are acquired afterwards, and
// holdLock re-checks the latest version, so a commit that landed between
// the scan and the lock is still a conflict. This locks the rows the
// predicate returned, not the gap: a concurrent insert of a new matching
// key is still a phantom.
//
// If a later key conflicts, locks already taken in this call stay until
// the transaction commits or aborts.
func (e *Engine) ScanForUpdate(tx *Tx, where string) ([]*Version, ScanStats, error) {
	vs, st := e.ScanInTx(tx)
	matched := make([]*Version, 0)
	for _, v := range vs {
		if where == "" {
			matched = append(matched, v)
			continue
		}
		ok, err := MatchWhere(v.Fields, where)
		if err != nil {
			return nil, st, err
		}
		if ok {
			matched = append(matched, v)
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, v := range matched {
		if err := e.holdLock(tx, v.Key); err != nil {
			return nil, st, err
		}
	}
	return matched, st, nil
}
