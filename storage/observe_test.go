package storage

import "testing"

func TestVersionChainReportsStatus(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

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

	chain := e.Versions("k")
	if len(chain) != 2 {
		t.Fatalf("chain: got %d, want 2", len(chain))
	}
	if chain[0].XminStatus != "committed" || chain[0].XmaxStatus != "committed" {
		t.Fatalf("old version: xmin %s xmax %s, want committed/committed", chain[0].XminStatus, chain[0].XmaxStatus)
	}
	if chain[1].Xmax != 0 || chain[1].XmaxStatus != "alive" {
		t.Fatalf("new version: xmax %d status %s, want 0 alive", chain[1].Xmax, chain[1].XmaxStatus)
	}
	if chain[0].Segment == "" || chain[0].Segment != chain[1].Segment {
		t.Fatalf("segments: %q and %q, want the same file", chain[0].Segment, chain[1].Segment)
	}

	rep := e.HeapStats()
	if len(rep.Segments) != 1 || rep.Segments[0].Live != 1 || rep.Segments[0].Dead != 1 {
		t.Fatalf("heap stats: %+v, want one segment with 1 live and 1 dead", rep.Segments)
	}
}

func TestTransactionsAndSnapshotAge(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	a, _ := e.BeginTx()
	if got := e.Transactions(); len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("open txs: %+v, want just %d", got, a.ID)
	}
	if e.SnapshotAge(a.Snap) != 0 {
		t.Fatalf("age just after begin: got %d, want 0", e.SnapshotAge(a.Snap))
	}

	b, _ := e.BeginTx()
	if got := e.SnapshotAge(a.Snap); got != 1 {
		t.Fatalf("age after another begin: got %d, want 1", got)
	}
	if got := len(e.Transactions()); got != 2 {
		t.Fatalf("open txs: got %d, want 2", got)
	}

	if err := e.CommitTx(a); err != nil {
		t.Fatal(err)
	}
	if err := e.AbortTx(b); err != nil {
		t.Fatal(err)
	}
	if got := e.Transactions(); len(got) != 0 {
		t.Fatalf("open txs after finish: %+v, want none", got)
	}
}
