package storage

import (
	"fmt"
	"testing"
	"time"
)

func TestSnapshotContents(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	tx1, _ := e.Tx.Begin() // 1, will commit
	tx2, _ := e.Tx.Begin() // 2, stays in flight
	if err := e.Tx.Commit(tx1); err != nil {
		t.Fatal(err)
	}

	snap := e.Tx.TakeSnapshot()
	if snap.Xmin != 2 {
		t.Errorf("xmin: got %d, want 2 (smallest in-flight txid)", snap.Xmin)
	}
	if snap.Xmax != 3 {
		t.Errorf("xmax: got %d, want 3 (next txid)", snap.Xmax)
	}
	if len(snap.Active) != 1 || snap.Active[0] != 2 {
		t.Errorf("active: got %v, want [2]", snap.Active)
	}

	// Nothing in flight: xmin == xmax.
	if err := e.Tx.Abort(tx2); err != nil {
		t.Fatal(err)
	}
	snap = e.Tx.TakeSnapshot()
	if snap.Xmin != 3 || snap.Xmax != 3 || len(snap.Active) != 0 {
		t.Errorf("empty snapshot: got %+v, want {3, 3, []}", snap)
	}
}

// Money test 1: repeatable read. A reads k; B updates k and commits; A reads
// k again and sees the same version, because A's snapshot was taken once at
// begin and never moves.
func TestRepeatableRead(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	// Seed: k = v1, committed.
	seed, _ := e.Tx.Begin()
	if err := e.Heap.Append(Version{Key: "k", Fields: raw(`{"v":1}`), Xmin: seed}); err != nil {
		t.Fatal(err)
	}
	if err := e.Tx.Commit(seed); err != nil {
		t.Fatal(err)
	}

	// A begins and reads.
	a, err := e.BeginTx()
	if err != nil {
		t.Fatal(err)
	}
	v1, _ := e.Get(a.Snap, "k")
	if v1 == nil || string(v1.Fields) != `{"v":1}` {
		t.Fatalf("A's first read: got %+v, want {v:1}", v1)
	}

	// B updates k and commits.
	b, err := e.BeginTx()
	if err != nil {
		t.Fatal(err)
	}
	old := e.Heap.Chain("k")[0]
	stamp := *old
	stamp.Xmax = b.ID
	if err := e.Heap.Append(stamp); err != nil {
		t.Fatal(err)
	}
	if err := e.Heap.Append(Version{Key: "k", Fields: raw(`{"v":2}`), Xmin: b.ID}); err != nil {
		t.Fatal(err)
	}
	if err := e.CommitTx(b); err != nil {
		t.Fatal(err)
	}

	// A reads again: same value, same version pointer.
	v2, _ := e.Get(a.Snap, "k")
	if v2 == nil || string(v2.Fields) != `{"v":1}` {
		t.Fatalf("A's second read: got %+v, want {v:1} (repeatable read)", v2)
	}
	if v2 != v1 {
		t.Errorf("A's two reads returned different versions: %p vs %p", v1, v2)
	}

	// Meanwhile a fresh snapshot sees the new value.
	fresh := e.Tx.TakeSnapshot()
	v3, _ := e.Get(fresh, "k")
	if v3 == nil || string(v3.Fields) != `{"v":2}` {
		t.Fatalf("fresh read: got %+v, want {v:2}", v3)
	}

	if err := e.AbortTx(a); err != nil {
		t.Fatal(err)
	}
}

// Money test 2: taking a snapshot is O(1) — flat cost regardless of heap
// size, because nothing is copied. We assert the snapshot carries no data
// and that begin latency does not grow with the heap.
func TestSnapshotIsO1(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	// 1k committed rows in the heap. (Larger would be better, but every
	// Append fsyncs — 10k rows takes ~40s. 1k is enough to make the point:
	// the snapshot is three integers and an empty list, and begin latency
	// does not touch the heap at all.)
	seed, _ := e.Tx.Begin()
	for i := 0; i < 1_000; i++ {
		if err := e.Heap.Append(Version{
			Key:    fmt.Sprintf("k%06d", i),
			Fields: raw(`{"v":1}`),
			Xmin:   seed,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Tx.Commit(seed); err != nil {
		t.Fatal(err)
	}

	// The snapshot itself is three integers and a one-element list.
	tx, err := e.BeginTx()
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.Snap.Active) != 1 || tx.Snap.Active[0] != tx.ID {
		t.Errorf("active: got %v, want just the new tx itself", tx.Snap.Active)
	}

	// Begin latency over a full heap stays in the same ballpark as over an
	// empty one. Generous bound: this is a sanity check, not a benchmark.
	start := time.Now()
	const n = 100
	for i := 0; i < n; i++ {
		tx2, err := e.BeginTx()
		if err != nil {
			t.Fatal(err)
		}
		if err := e.AbortTx(tx2); err != nil {
			t.Fatal(err)
		}
	}
	if avg := time.Since(start) / n; avg > 10*time.Millisecond {
		t.Errorf("begin over 1k-row heap averaged %v; snapshot should copy nothing", avg)
	}
}

// A snapshot taken inside BeginTx is atomic with the txid allocation: the
// new transaction sees itself in its own active list (its own writes are
// handled separately in milestone 4), and no txid can slip between the
// allocation and the snapshot.
func TestBeginTxSnapshotIncludesSelf(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	a, err := e.BeginTx()
	if err != nil {
		t.Fatal(err)
	}
	if !a.Snap.inFlight(a.ID) {
		t.Errorf("A's snapshot should list A itself as in flight: %+v", a.Snap)
	}
	if a.Snap.Xmax != a.ID+1 {
		t.Errorf("xmax: got %d, want %d (next txid after A)", a.Snap.Xmax, a.ID+1)
	}
}

// Abort leaves the aborted versions invisible to every snapshot, forever.
func TestAbortLeavesVersionsInvisible(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	a, _ := e.BeginTx()
	if err := e.Heap.Append(Version{Key: "k", Fields: raw(`{"v":1}`), Xmin: a.ID}); err != nil {
		t.Fatal(err)
	}
	if err := e.AbortTx(a); err != nil {
		t.Fatal(err)
	}

	snap := e.Tx.TakeSnapshot()
	if v, _ := e.Get(snap, "k"); v != nil {
		t.Fatalf("aborted version %+v is visible", v)
	}
	// The version is still on disk — GC's problem, not the reader's.
	if got := e.Heap.Len(); got != 1 {
		t.Errorf("heap len: got %d, want 1 (abort does not delete)", got)
	}
}
