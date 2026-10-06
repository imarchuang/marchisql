package storage

import (
	"encoding/json"
	"testing"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

// buildWalkthrough reproduces storage/VISIBILITY_WALKTHROUGH.md exactly:
// tx1–3 committed, tx4 aborted, tx5 and tx6 still in progress, and six
// versions covering every branch of the visibility function.
func buildWalkthrough(t *testing.T) (*Engine, Snapshot) {
	t.Helper()
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// CLOG: 1,2,3 committed; 4 aborted; 5,6 in progress.
	for i := 0; i < 3; i++ {
		tx, err := e.Tx.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Tx.Commit(tx); err != nil {
			t.Fatal(err)
		}
	}
	tx4, _ := e.Tx.Begin()
	if err := e.Tx.Abort(tx4); err != nil {
		t.Fatal(err)
	}
	tx5, _ := e.Tx.Begin()
	tx6, _ := e.Tx.Begin()
	if tx4 != 4 || tx5 != 5 || tx6 != 6 {
		t.Fatalf("scenario txids off: tx4=%d tx5=%d tx6=%d, want 4,5,6", tx4, tx5, tx6)
	}

	// The heap, in the doc's append order.
	vs := []Version{
		{Key: "alice", Fields: raw(`{"v":1}`), Xmin: 1, Xmax: 2}, // superseded by tx2
		{Key: "alice", Fields: raw(`{"v":2}`), Xmin: 2, Xmax: 0}, // alive
		{Key: "bob", Fields: raw(`{"v":1}`), Xmin: 3, Xmax: 5},   // tx5 is deleting it
		{Key: "carol", Fields: raw(`{"v":1}`), Xmin: 4, Xmax: 0}, // aborted insert
		{Key: "dave", Fields: raw(`{"v":1}`), Xmin: 6, Xmax: 0},  // uncommitted insert
		{Key: "eve", Fields: raw(`{"v":1}`), Xmin: 5, Xmax: 0},   // uncommitted insert
	}
	for _, v := range vs {
		if err := e.Heap.Append(v); err != nil {
			t.Fatal(err)
		}
	}

	snap := Snapshot{Xmin: 5, Xmax: 7, Active: []uint64{5, 6}}
	return e, snap
}

// The acceptance test: every verdict in VISIBILITY_WALKTHROUGH.md's tally
// table, reproduced clause by clause.
func TestVisibilityWalkthrough(t *testing.T) {
	e, snap := buildWalkthrough(t)
	defer e.Close()

	want := []struct {
		key     string
		xmin    uint64
		visible bool
	}{
		{"alice", 1, false}, // superseded by committed tx2
		{"alice", 2, true},  // the visible one
		{"bob", 3, true},    // delete by tx5 still in flight
		{"carol", 4, false}, // aborted insert
		{"dave", 6, false},  // uncommitted insert
		{"eve", 5, false},   // uncommitted insert
	}
	vs := e.Heap.Versions()
	if len(vs) != len(want) {
		t.Fatalf("heap has %d versions, scenario wants %d", len(vs), len(want))
	}
	for i, w := range want {
		if vs[i].Key != w.key || vs[i].Xmin != w.xmin {
			t.Fatalf("version %d: got {%s xmin=%d}, want {%s xmin=%d}",
				i, vs[i].Key, vs[i].Xmin, w.key, w.xmin)
		}
		if got := visible(vs[i], snap, e.Tx.clog); got != w.visible {
			t.Errorf("visible({%s xmin=%d xmax=%d}) = %v, want %v",
				vs[i].Key, vs[i].Xmin, vs[i].Xmax, got, w.visible)
		}
	}
}

// The doc's final tally: scan returns alice→{v:2} and bob→{v:1},
// X-Marchisql-Versions-Scanned: 6, X-Marchisql-Versions-Skipped-Invisible: 4.
func TestScanWalkthrough(t *testing.T) {
	e, snap := buildWalkthrough(t)
	defer e.Close()

	got, st := e.Scan(snap)
	if st.Scanned != 6 || st.SkippedInvisible != 4 {
		t.Errorf("stats: got scanned=%d skipped=%d, want 6/4 (the doc's tally)",
			st.Scanned, st.SkippedInvisible)
	}
	if len(got) != 2 || got[0].Key != "alice" || got[1].Key != "bob" {
		t.Fatalf("scan: got %v, want [alice bob]", keys(got))
	}
	if string(got[0].Fields) != `{"v":2}` {
		t.Errorf("alice: got fields %s, want v:2 (the newest committed version)", got[0].Fields)
	}
}

func TestGetWalkthrough(t *testing.T) {
	e, snap := buildWalkthrough(t)
	defer e.Close()

	v, st := e.Get(snap, "alice")
	if v == nil || string(v.Fields) != `{"v":2}` {
		t.Fatalf("get alice: got %+v, want {v:2}", v)
	}
	if st.Scanned != 1 || st.SkippedInvisible != 0 {
		t.Errorf("get alice stats: got %d/%d, want 1/0 (newest version visible immediately)",
			st.Scanned, st.SkippedInvisible)
	}

	v, st = e.Get(snap, "carol")
	if v != nil {
		t.Errorf("get carol: got %+v, want nil (aborted insert)", v)
	}
	if st.Scanned != 1 || st.SkippedInvisible != 1 {
		t.Errorf("get carol stats: got %d/%d, want 1/1", st.Scanned, st.SkippedInvisible)
	}

	if v, _ := e.Get(snap, "nobody"); v != nil {
		t.Errorf("get nobody: got %+v, want nil", v)
	}
}

// PLAN milestone 2, test 1: an uncommitted version is invisible.
func TestTrivialSnapshotHidesUncommitted(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	tx, _ := e.Tx.Begin()
	if err := e.Heap.Append(Version{Key: "k", Fields: raw(`{"v":1}`), Xmin: tx}); err != nil {
		t.Fatal(err)
	}

	snap := LatestSnapshot(e.Tx.NextTxid())
	if v, _ := e.Get(snap, "k"); v != nil {
		t.Fatalf("dirty read: uncommitted version %+v is visible", v)
	}
}

// PLAN milestone 2, test 2: an aborted version is invisible.
func TestTrivialSnapshotHidesAborted(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	tx, _ := e.Tx.Begin()
	if err := e.Heap.Append(Version{Key: "k", Fields: raw(`{"v":1}`), Xmin: tx}); err != nil {
		t.Fatal(err)
	}
	if err := e.Tx.Abort(tx); err != nil {
		t.Fatal(err)
	}

	snap := LatestSnapshot(e.Tx.NextTxid())
	if v, _ := e.Get(snap, "k"); v != nil {
		t.Fatalf("aborted version %+v is visible", v)
	}
}

// PLAN milestone 2, test 3: once a newer committed version exists, the old
// committed version is invisible.
func TestSupersededVersionInvisible(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	tx1, _ := e.Tx.Begin()
	v1 := Version{Key: "k", Fields: raw(`{"v":1}`), Xmin: tx1}
	if err := e.Heap.Append(v1); err != nil {
		t.Fatal(err)
	}
	if err := e.Tx.Commit(tx1); err != nil {
		t.Fatal(err)
	}

	tx2, _ := e.Tx.Begin()
	stamp := v1
	stamp.Xmax = tx2
	if err := e.Heap.Append(stamp); err != nil {
		t.Fatal(err)
	}
	if err := e.Heap.Append(Version{Key: "k", Fields: raw(`{"v":2}`), Xmin: tx2}); err != nil {
		t.Fatal(err)
	}
	if err := e.Tx.Commit(tx2); err != nil {
		t.Fatal(err)
	}

	snap := LatestSnapshot(e.Tx.NextTxid())
	v, st := e.Get(snap, "k")
	if v == nil || string(v.Fields) != `{"v":2}` {
		t.Fatalf("get: got %+v, want {v:2}", v)
	}
	if st.Scanned != 1 {
		t.Errorf("get stats: scanned %d versions, want 1 (newest first, first hit wins)", st.Scanned)
	}
	vs, st := e.Scan(snap)
	if len(vs) != 1 || st.Scanned != 2 || st.SkippedInvisible != 1 {
		t.Errorf("scan: got %d versions, stats %d/%d; want 1 version, 2/1",
			len(vs), st.Scanned, st.SkippedInvisible)
	}
}

// An aborted delete is not a delete: the old version lives on.
func TestAbortedDeleteLeavesVersionVisible(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	tx1, _ := e.Tx.Begin()
	v1 := Version{Key: "k", Fields: raw(`{"v":1}`), Xmin: tx1}
	e.Heap.Append(v1)
	e.Tx.Commit(tx1)

	tx2, _ := e.Tx.Begin()
	stamp := v1
	stamp.Xmax = tx2
	e.Heap.Append(stamp)
	if err := e.Tx.Abort(tx2); err != nil {
		t.Fatal(err)
	}

	snap := LatestSnapshot(e.Tx.NextTxid())
	v, _ := e.Get(snap, "k")
	if v == nil || string(v.Fields) != `{"v":1}` {
		t.Fatalf("aborted delete: got %+v, want {v:1} still visible", v)
	}
}

// Regression test for clause ordering: a delete by a LIVE, uncommitted
// transaction must not take effect — even under the trivial snapshot, whose
// empty active list cannot catch the killer. The in_progress clause must
// fire before the snapshot-boundary checks.
func TestInFlightDeleteNotVisible(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	tx1, _ := e.Tx.Begin()
	v1 := Version{Key: "k", Fields: raw(`{"v":1}`), Xmin: tx1}
	e.Heap.Append(v1)
	e.Tx.Commit(tx1)

	tx2, _ := e.Tx.Begin()
	stamp := v1
	stamp.Xmax = tx2
	e.Heap.Append(stamp)
	// tx2 never commits.

	snap := LatestSnapshot(e.Tx.NextTxid()) // active list is empty!
	v, _ := e.Get(snap, "k")
	if v == nil || string(v.Fields) != `{"v":1}` {
		t.Fatalf("in-flight delete took effect: got %+v, want {v:1} still visible", v)
	}
}

func keys(vs []*Version) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Key
	}
	return out
}
