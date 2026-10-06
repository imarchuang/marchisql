package storage

import (
	"errors"
	"testing"
)

// Money test 1: two transactions update the SAME key concurrently → the
// second gets serialization_failure. Same-row oversell is impossible under
// snapshot isolation.
func TestSameRowWriteWriteConflict(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	// Seed: k = 100, committed.
	seed, _ := e.BeginTx()
	if err := e.Update(seed, "k", raw(`{"balance":100}`)); err != nil {
		t.Fatal(err)
	}
	if err := e.CommitTx(seed); err != nil {
		t.Fatal(err)
	}

	// A and B both begin over the same snapshot.
	a, _ := e.BeginTx()
	b, _ := e.BeginTx()

	// A writes and commits.
	if err := e.Update(a, "k", raw(`{"balance":80}`)); err != nil {
		t.Fatal(err)
	}
	if err := e.CommitTx(a); err != nil {
		t.Fatal(err)
	}

	// B writes the same key: first-writer-wins → serialization_failure.
	err = e.Update(b, "k", raw(`{"balance":60}`))
	if !errors.Is(err, ErrSerializationFailure) {
		t.Fatalf("B's update: got %v, want serialization_failure", err)
	}
}

// Money test 2: update-then-read-your-own-writes inside one transaction.
func TestReadYourOwnWrites(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	tx, _ := e.BeginTx()
	if err := e.Update(tx, "k", raw(`{"v":42}`)); err != nil {
		t.Fatal(err)
	}

	// The transaction sees its own uncommitted write.
	v, _ := e.GetInTx(tx, "k")
	if v == nil || string(v.Fields) != `{"v":42}` {
		t.Fatalf("read-your-own-writes: got %+v, want {v:42}", v)
	}

	// A fresh snapshot does NOT see it (uncommitted).
	fresh := e.Tx.TakeSnapshot()
	if v, _ := e.Get(fresh, "k"); v != nil {
		t.Fatalf("uncommitted write leaked to fresh snapshot: %+v", v)
	}

	// After commit, everyone sees it.
	if err := e.CommitTx(tx); err != nil {
		t.Fatal(err)
	}
	fresh = e.Tx.TakeSnapshot()
	v, _ = e.Get(fresh, "k")
	if v == nil || string(v.Fields) != `{"v":42}` {
		t.Fatalf("post-commit read: got %+v, want {v:42}", v)
	}
}

// A transaction's own delete is visible to it, and the row reappears for
// everyone if the transaction aborts.
func TestReadYourOwnDelete(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	seed, _ := e.BeginTx()
	e.Update(seed, "k", raw(`{"v":1}`))
	e.CommitTx(seed)

	tx, _ := e.BeginTx()
	if err := e.Delete(tx, "k"); err != nil {
		t.Fatal(err)
	}

	// Own delete: the key is gone for this transaction.
	if v, _ := e.GetInTx(tx, "k"); v != nil {
		t.Fatalf("own delete: got %+v, want nil", v)
	}

	// Others still see it (uncommitted delete is not a delete).
	fresh := e.Tx.TakeSnapshot()
	if v, _ := e.Get(fresh, "k"); v == nil {
		t.Fatal("uncommitted delete hid the row from a fresh snapshot")
	}

	// Abort: the row lives on for everyone.
	if err := e.AbortTx(tx); err != nil {
		t.Fatal(err)
	}
	fresh = e.Tx.TakeSnapshot()
	if v, _ := e.Get(fresh, "k"); v == nil || string(v.Fields) != `{"v":1}` {
		t.Fatalf("post-abort read: got %+v, want {v:1}", v)
	}
}

// Fail-fast conflict: two live transactions write the same key; the second
// gets transaction_conflict (not serialization_failure — nobody committed).
func TestInProgressWriteConflict(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	seed, _ := e.BeginTx()
	e.Update(seed, "k", raw(`{"v":1}`))
	e.CommitTx(seed)

	a, _ := e.BeginTx()
	b, _ := e.BeginTx()

	if err := e.Update(a, "k", raw(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	// A hasn't committed; B's write to the same key fails fast.
	err = e.Update(b, "k", raw(`{"v":3}`))
	if !errors.Is(err, ErrTxConflict) {
		t.Fatalf("B's update: got %v, want transaction_conflict", err)
	}
}

// An aborted writer does not block: after A aborts, B can write the key.
func TestAbortedWriterDoesNotBlock(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	seed, _ := e.BeginTx()
	e.Update(seed, "k", raw(`{"v":1}`))
	e.CommitTx(seed)

	a, _ := e.BeginTx()
	if err := e.Update(a, "k", raw(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := e.AbortTx(a); err != nil {
		t.Fatal(err)
	}

	b, _ := e.BeginTx()
	if err := e.Update(b, "k", raw(`{"v":3}`)); err != nil {
		t.Fatalf("B's update after A aborted: got %v, want nil", err)
	}
	if err := e.CommitTx(b); err != nil {
		t.Fatal(err)
	}

	fresh := e.Tx.TakeSnapshot()
	v, _ := e.Get(fresh, "k")
	if v == nil || string(v.Fields) != `{"v":3}` {
		t.Fatalf("final read: got %+v, want {v:3}", v)
	}
}

// Insert-then-update within one transaction: the write set tracks both.
func TestInsertThenUpdateSameTx(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	tx, _ := e.BeginTx()
	if err := e.Update(tx, "k", raw(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := e.Update(tx, "k", raw(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}

	v, _ := e.GetInTx(tx, "k")
	if v == nil || string(v.Fields) != `{"v":2}` {
		t.Fatalf("read-your-own-writes after two updates: got %+v, want {v:2}", v)
	}
	if err := e.CommitTx(tx); err != nil {
		t.Fatal(err)
	}

	fresh := e.Tx.TakeSnapshot()
	v, _ = e.Get(fresh, "k")
	if v == nil || string(v.Fields) != `{"v":2}` {
		t.Fatalf("post-commit read: got %+v, want {v:2}", v)
	}
}

// Delete of a nonexistent key is an error.
func TestDeleteNonexistent(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	tx, _ := e.BeginTx()
	if err := e.Delete(tx, "nobody"); err == nil {
		t.Fatal("delete of nonexistent key: want error")
	}
}

// ScanInTx overlays the write set on the snapshot: own updates replace,
// own inserts appear, own deletes remove.
func TestScanInTxOverlaysWriteSet(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	seed, _ := e.BeginTx()
	e.Update(seed, "a", raw(`{"v":1}`))
	e.Update(seed, "b", raw(`{"v":1}`))
	e.Update(seed, "c", raw(`{"v":1}`))
	e.CommitTx(seed)

	tx, _ := e.BeginTx()
	e.Update(tx, "a", raw(`{"v":2}`)) // update
	e.Update(tx, "d", raw(`{"v":1}`)) // insert
	e.Delete(tx, "c")                 // delete

	vs, _ := e.ScanInTx(tx)
	got := make(map[string]string)
	for _, v := range vs {
		got[v.Key] = string(v.Fields)
	}
	want := map[string]string{
		"a": `{"v":2}`, // updated
		"b": `{"v":1}`, // untouched
		"d": `{"v":1}`, // inserted
		// c deleted
	}
	if len(got) != len(want) {
		t.Fatalf("scan-in-tx: got %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("key %s: got %s, want %s", k, got[k], w)
		}
	}
}
