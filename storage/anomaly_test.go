package storage

import (
	"encoding/json"
	"errors"
	"testing"
)

func countWhere(t *testing.T, vs []*Version, where string) int {
	t.Helper()
	n := 0
	for _, v := range vs {
		ok, err := MatchWhere(v.Fields, where)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			n++
		}
	}
	return n
}

func mustUpdate(t *testing.T, e *Engine, tx *Tx, key, fields string) {
	t.Helper()
	if err := e.Update(tx, key, raw(fields)); err != nil {
		t.Fatal(err)
	}
}

// Dirty read: a version another transaction has not committed is invisible.
func TestAnomalyDirtyReadPrevented(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	writer, _ := e.BeginTx()
	mustUpdate(t, e, writer, "k", `{"v":1}`)

	reader, _ := e.BeginTx()
	if v, _ := e.GetInTx(reader, "k"); v != nil {
		t.Fatalf("dirty read: uncommitted version %+v is visible", v)
	}
}

// Non-repeatable read: a commit that lands after my snapshot does not change
// a second read inside the same transaction.
func TestAnomalyNonRepeatableReadPrevented(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	seed, _ := e.BeginTx()
	mustUpdate(t, e, seed, "k", `{"v":1}`)
	e.CommitTx(seed)

	a, _ := e.BeginTx()
	first, _ := e.GetInTx(a, "k")
	if first == nil || string(first.Fields) != `{"v":1}` {
		t.Fatalf("first read: got %+v, want {v:1}", first)
	}

	b, _ := e.BeginTx()
	mustUpdate(t, e, b, "k", `{"v":2}`)
	if err := e.CommitTx(b); err != nil {
		t.Fatal(err)
	}

	second, _ := e.GetInTx(a, "k")
	if second == nil || string(second.Fields) != `{"v":1}` {
		t.Fatalf("second read: got %+v, want {v:1} (snapshot did not move)", second)
	}
}

// Phantom: a row inserted and committed after my snapshot does not appear in
// a repeated scan of the same predicate.
func TestAnomalyPhantomPrevented(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	seed, _ := e.BeginTx()
	mustUpdate(t, e, seed, "alice", `{"on_call":true}`)
	mustUpdate(t, e, seed, "bob", `{"on_call":true}`)
	e.CommitTx(seed)

	a, _ := e.BeginTx()
	vs, _ := e.ScanInTx(a)
	if got := countWhere(t, vs, "on_call=true"); got != 2 {
		t.Fatalf("A's first count: got %d, want 2", got)
	}

	b, _ := e.BeginTx()
	mustUpdate(t, e, b, "carol", `{"on_call":true}`)
	if err := e.CommitTx(b); err != nil {
		t.Fatal(err)
	}

	vs, _ = e.ScanInTx(a)
	if got := countWhere(t, vs, "on_call=true"); got != 2 {
		t.Fatalf("A's second count: got %d, want 2 (carol is a phantom)", got)
	}

	fresh := e.Tx.TakeSnapshot()
	vs, _ = e.Scan(fresh)
	if got := countWhere(t, vs, "on_call=true"); got != 3 {
		t.Fatalf("fresh count: got %d, want 3", got)
	}
}

// Same-row lost update: two transactions cannot both commit a write to one key.
func TestAnomalyLostUpdatePrevented(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	seed, _ := e.BeginTx()
	mustUpdate(t, e, seed, "k", `{"balance":100}`)
	e.CommitTx(seed)

	a, _ := e.BeginTx()
	b, _ := e.BeginTx()
	mustUpdate(t, e, a, "k", `{"balance":80}`)
	if err := e.CommitTx(a); err != nil {
		t.Fatal(err)
	}
	err = e.Update(b, "k", raw(`{"balance":60}`))
	if !errors.Is(err, ErrSerializationFailure) {
		t.Fatalf("second writer: got %v, want serialization_failure", err)
	}
}

// Write skew, doctors on call. Each transaction sees two doctors on call,
// each takes a different one off call, both commit. The predicate
// "someone is on call" held in both snapshots and holds in neither final state.
func TestAnomalyWriteSkewDoctors(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	seed, _ := e.BeginTx()
	mustUpdate(t, e, seed, "alice", `{"on_call":true}`)
	mustUpdate(t, e, seed, "bob", `{"on_call":true}`)
	e.CommitTx(seed)

	a, _ := e.BeginTx()
	vs, _ := e.ScanInTx(a)
	if got := countWhere(t, vs, "on_call=true"); got != 2 {
		t.Fatalf("A sees %d on call, want 2", got)
	}

	b, _ := e.BeginTx()
	vs, _ = e.ScanInTx(b)
	if got := countWhere(t, vs, "on_call=true"); got != 2 {
		t.Fatalf("B sees %d on call, want 2", got)
	}

	mustUpdate(t, e, b, "bob", `{"on_call":false}`)
	if err := e.CommitTx(b); err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, e, a, "alice", `{"on_call":false}`)
	if err := e.CommitTx(a); err != nil {
		t.Fatalf("A's commit: %v (different keys must both commit)", err)
	}

	fresh := e.Tx.TakeSnapshot()
	vs, _ = e.Scan(fresh)
	if got := countWhere(t, vs, "on_call=true"); got != 0 {
		t.Fatalf("on call after both commits: got %d, want 0 (write skew)", got)
	}
}

// Write skew, two accounts with the constraint balance(a)+balance(b) >= 0.
// Each transaction reads the sum, finds it sufficient, and withdraws 200
// from a different account. Both commit. The sum is -200.
func TestAnomalyWriteSkewBalance(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	seed, _ := e.BeginTx()
	mustUpdate(t, e, seed, "a", `{"balance":100}`)
	mustUpdate(t, e, seed, "b", `{"balance":100}`)
	e.CommitTx(seed)

	sum := func(tx *Tx) int {
		t.Helper()
		va, _ := e.GetInTx(tx, "a")
		vb, _ := e.GetInTx(tx, "b")
		return balance(t, va) + balance(t, vb)
	}

	txA, _ := e.BeginTx()
	txB, _ := e.BeginTx()
	if got := sum(txA); got != 200 {
		t.Fatalf("A reads sum %d, want 200", got)
	}
	if got := sum(txB); got != 200 {
		t.Fatalf("B reads sum %d, want 200", got)
	}

	// Each withdrawal is legal against its own snapshot: 200 - 200 >= 0.
	mustUpdate(t, e, txA, "a", `{"balance":-100}`)
	if err := e.CommitTx(txA); err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, e, txB, "b", `{"balance":-100}`)
	if err := e.CommitTx(txB); err != nil {
		t.Fatalf("B's commit: %v (different accounts must both commit)", err)
	}

	fresh, _ := e.BeginTx()
	if got := sum(fresh); got != -200 {
		t.Fatalf("sum after both withdrawals: got %d, want -200 (write skew)", got)
	}
}

func balance(t *testing.T, v *Version) int {
	t.Helper()
	if v == nil {
		t.Fatal("missing account")
	}
	var obj struct {
		Balance int `json:"balance"`
	}
	if err := json.Unmarshal(v.Fields, &obj); err != nil {
		t.Fatal(err)
	}
	return obj.Balance
}
