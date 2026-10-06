package storage

import (
	"errors"
	"testing"
)

func seedOnCall(t *testing.T, e *Engine, keys ...string) {
	t.Helper()
	seed, err := e.BeginTx()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		mustUpdate(t, e, seed, key, `{"on_call":true}`)
	}
	if err := e.CommitTx(seed); err != nil {
		t.Fatal(err)
	}
}

// The doctors write skew from WRITE_SKEW.md, with both transactions locking
// the on-call set. The second lock fails, the second transaction aborts,
// and one doctor stays on call.
func TestForUpdateStopsDoctorsSkew(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	seedOnCall(t, e, "alice", "bob")

	a, err := e.BeginTx()
	if err != nil {
		t.Fatal(err)
	}
	vs, _, err := e.ScanForUpdate(a, "on_call=true")
	if err != nil {
		t.Fatal(err)
	}
	if got := countWhere(t, vs, "on_call=true"); got != 2 {
		t.Fatalf("A locked %d on-call rows, want 2", got)
	}

	b, err := e.BeginTx()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.ScanForUpdate(b, "on_call=true"); !errors.Is(err, ErrTxConflict) {
		t.Fatalf("B scan for update: got %v, want ErrTxConflict", err)
	}
	if err := e.Update(b, "bob", raw(`{"on_call":false}`)); !errors.Is(err, ErrTxConflict) {
		t.Fatalf("B update of locked bob: got %v, want ErrTxConflict", err)
	}
	if err := e.AbortTx(b); err != nil {
		t.Fatal(err)
	}

	mustUpdate(t, e, a, "alice", `{"on_call":false}`)
	if err := e.CommitTx(a); err != nil {
		t.Fatal(err)
	}

	fresh, err := e.BeginTx()
	if err != nil {
		t.Fatal(err)
	}
	vs, _ = e.ScanInTx(fresh)
	if got := countWhere(t, vs, "on_call=true"); got != 1 {
		t.Fatalf("on call after A commits and B aborts: got %d, want 1", got)
	}
}

// Fail-fast does not re-read the row after the holder commits. A transaction
// that keeps its old snapshot and writes a different locked row once the
// lock is gone can still skew. The caller has to abort and begin again.
func TestForUpdateFailFastDoesNotRecheck(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	seedOnCall(t, e, "alice", "bob")

	a, _ := e.BeginTx()
	if _, _, err := e.ScanForUpdate(a, "on_call=true"); err != nil {
		t.Fatal(err)
	}
	b, _ := e.BeginTx()
	if err := e.Update(b, "bob", raw(`{"on_call":false}`)); !errors.Is(err, ErrTxConflict) {
		t.Fatalf("while A holds the lock: got %v, want ErrTxConflict", err)
	}

	mustUpdate(t, e, a, "alice", `{"on_call":false}`)
	if err := e.CommitTx(a); err != nil {
		t.Fatal(err)
	}
	// B never aborted. bob was not the row A wrote, so first-writer-wins
	// lets this commit, and the on-call set is empty.
	mustUpdate(t, e, b, "bob", `{"on_call":false}`)
	if err := e.CommitTx(b); err != nil {
		t.Fatal(err)
	}
	vs, _ := e.Scan(e.Tx.TakeSnapshot())
	if got := countWhere(t, vs, "on_call=true"); got != 0 {
		t.Fatalf("same-transaction retry after release: got %d on call, want 0", got)
	}
}

// A conflict on a later key leaves the earlier locks held until abort.
func TestForUpdateKeepsEarlierLocks(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	seedOnCall(t, e, "alice", "bob", "carol")

	holder, _ := e.BeginTx()
	mustUpdate(t, e, holder, "bob", `{"on_call":true,"note":"editing"}`)

	scanner, _ := e.BeginTx()
	if _, _, err := e.ScanForUpdate(scanner, "on_call=true"); !errors.Is(err, ErrTxConflict) {
		t.Fatalf("scan: got %v, want ErrTxConflict", err)
	}

	other, _ := e.BeginTx()
	if err := e.Update(other, "alice", raw(`{"on_call":false}`)); !errors.Is(err, ErrTxConflict) {
		t.Fatalf("alice should stay locked after the partial scan: got %v", err)
	}
	if err := e.AbortTx(scanner); err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, e, other, "alice", `{"on_call":false}`)
}

// Without FOR UPDATE the original write skew still commits.
func TestWriteSkewStillCommitsWithoutForUpdate(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	seedOnCall(t, e, "alice", "bob")

	a, _ := e.BeginTx()
	b, _ := e.BeginTx()
	if vs, _ := e.ScanInTx(a); countWhere(t, vs, "on_call=true") != 2 {
		t.Fatal("A should see both doctors")
	}
	if vs, _ := e.ScanInTx(b); countWhere(t, vs, "on_call=true") != 2 {
		t.Fatal("B should see both doctors")
	}
	mustUpdate(t, e, b, "bob", `{"on_call":false}`)
	mustUpdate(t, e, a, "alice", `{"on_call":false}`)
	if err := e.CommitTx(b); err != nil {
		t.Fatal(err)
	}
	if err := e.CommitTx(a); err != nil {
		t.Fatal(err)
	}
	vs, _ := e.Scan(e.Tx.TakeSnapshot())
	if got := countWhere(t, vs, "on_call=true"); got != 0 {
		t.Fatalf("skew without FOR UPDATE: got %d, want 0", got)
	}
}
