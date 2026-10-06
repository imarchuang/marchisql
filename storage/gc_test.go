package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeadVersionsDisappearWhenSnapshotsClose(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	seed, _ := e.BeginTx()
	mustUpdate(t, e, seed, "k", `{"v":1}`)
	if err := e.CommitTx(seed); err != nil {
		t.Fatal(err)
	}
	next, _ := e.BeginTx()
	mustUpdate(t, e, next, "k", `{"v":2}`)
	if err := e.CommitTx(next); err != nil {
		t.Fatal(err)
	}
	if got := e.Heap.Len(); got != 2 {
		t.Fatalf("before gc: %d versions, want 2", got)
	}

	st, err := e.ForceGC()
	if err != nil {
		t.Fatal(err)
	}
	if st.Collected != 1 {
		t.Fatalf("collected: got %d, want 1", st.Collected)
	}
	if e.Heap.Len() != 1 {
		t.Fatalf("after gc: %d versions, want 1", e.Heap.Len())
	}
	v := e.Heap.Versions()[0]
	if string(v.Fields) != `{"v":2}` || v.Xmax != 0 {
		t.Fatalf("survivor: got %+v, want {v:2} still alive", v)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	if e2.Heap.Len() != 1 {
		t.Fatalf("after restart: %d versions, want 1 (manifest must hide the old segment)", e2.Heap.Len())
	}
	if _, err := os.Stat(filepath.Join(dir, "heap", "MANIFEST")); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
}

func TestGCRespectsThreshold(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	seed, _ := e.BeginTx()
	mustUpdate(t, e, seed, "k", `{"v":1}`)
	e.CommitTx(seed)
	next, _ := e.BeginTx()
	mustUpdate(t, e, next, "k", `{"v":2}`)
	e.CommitTx(next)

	// 1 dead of 2 is below a 0.9 threshold.
	st, err := e.GC(0.9)
	if err != nil {
		t.Fatal(err)
	}
	if st.Collected != 0 || e.Heap.Len() != 2 {
		t.Fatalf("below threshold: collected %d, len %d; want 0 collected and both versions kept", st.Collected, e.Heap.Len())
	}
}

// A long-lived transaction pins the horizon at its snapshot xmin. Churn
// behind it cannot be collected until the transaction ends.
func TestLongTransactionPinsHistory(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	seed, _ := e.BeginTx()
	mustUpdate(t, e, seed, "k", `{"v":0}`)
	if err := e.CommitTx(seed); err != nil {
		t.Fatal(err)
	}

	a, _ := e.BeginTx()
	const churn = 1000
	for i := 0; i < churn; i++ {
		tx, err := e.BeginTx()
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Update(tx, "k", raw(`{"v":1}`)); err != nil {
			t.Fatal(err)
		}
		if err := e.CommitTx(tx); err != nil {
			t.Fatal(err)
		}
	}

	st, err := e.ForceGC()
	if err != nil {
		t.Fatal(err)
	}
	if st.Collected != 0 {
		t.Fatalf("gc while A is open: collected %d, want 0 (horizon pinned at %d)", st.Collected, st.Horizon)
	}
	if v, _ := e.GetInTx(a, "k"); v == nil || string(v.Fields) != `{"v":0}` {
		t.Fatalf("A after gc: got %+v, want the version from before the churn", v)
	}
	if got := e.Heap.Len(); got != churn+1 {
		t.Fatalf("versions while A is open: got %d, want %d", got, churn+1)
	}

	if err := e.CommitTx(a); err != nil {
		t.Fatal(err)
	}
	st, err = e.ForceGC()
	if err != nil {
		t.Fatal(err)
	}
	if st.Collected != churn {
		t.Fatalf("gc after A commits: collected %d, want %d", st.Collected, churn)
	}
	if e.Heap.Len() != 1 {
		t.Fatalf("versions after gc: got %d, want 1", e.Heap.Len())
	}
}
