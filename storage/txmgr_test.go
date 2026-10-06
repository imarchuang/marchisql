package storage

import "testing"

func TestTxidsStartAtOneAndIncrease(t *testing.T) {
	dir := t.TempDir()
	m, err := OpenTxMgr(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	tx1, err := m.Begin()
	if err != nil {
		t.Fatal(err)
	}
	tx2, err := m.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if tx1 != 1 || tx2 != 2 {
		t.Fatalf("got txids %d, %d; want 1, 2 (0 is reserved: xmax=0 means alive)", tx1, tx2)
	}
	if got := m.NextTxid(); got != 3 {
		t.Fatalf("next: got %d, want 3", got)
	}
	if got := len(m.Active()); got != 2 {
		t.Fatalf("active: got %d, want 2", got)
	}
}

func TestCommitAndAbort(t *testing.T) {
	dir := t.TempDir()
	m, err := OpenTxMgr(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	tx1, _ := m.Begin()
	tx2, _ := m.Begin()
	if err := m.Commit(tx1); err != nil {
		t.Fatal(err)
	}
	if err := m.Abort(tx2); err != nil {
		t.Fatal(err)
	}
	if got := m.Status(tx1); got != StatusCommitted {
		t.Errorf("tx1: got %s, want committed", got)
	}
	if got := m.Status(tx2); got != StatusAborted {
		t.Errorf("tx2: got %s, want aborted", got)
	}
	if got := m.Status(999); got != StatusUnknown {
		t.Errorf("tx999: got %s, want unknown", got)
	}
	if got := len(m.Active()); got != 0 {
		t.Errorf("active after finish: got %d, want 0", got)
	}
}

func TestFinishRejectsNonInProgress(t *testing.T) {
	dir := t.TempDir()
	m, err := OpenTxMgr(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	tx, _ := m.Begin()
	if err := m.Commit(tx); err != nil {
		t.Fatal(err)
	}
	if err := m.Commit(tx); err == nil {
		t.Error("double commit: want error")
	}
	if err := m.Abort(tx); err == nil {
		t.Error("abort after commit: want error")
	}
	if err := m.Commit(999); err == nil {
		t.Error("commit unknown txid: want error")
	}
}

func TestTxidCounterSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	m, err := OpenTxMgr(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		tx, err := m.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Commit(tx); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	m2, err := OpenTxMgr(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	tx, err := m2.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if tx != 4 {
		t.Fatalf("txid after restart: got %d, want 4 (txids must never be reused)", tx)
	}
}

func TestCrashRecoveryAbortsInProgress(t *testing.T) {
	dir := t.TempDir()
	m, err := OpenTxMgr(dir)
	if err != nil {
		t.Fatal(err)
	}
	committed, _ := m.Begin()
	dangling, _ := m.Begin()
	if err := m.Commit(committed); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: close files without finishing the dangling tx.
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	m2, err := OpenTxMgr(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()

	if got := m2.Status(committed); got != StatusCommitted {
		t.Errorf("committed tx: got %s, want committed", got)
	}
	if got := m2.Status(dangling); got != StatusAborted {
		t.Errorf("crash victim: got %s, want aborted (no commit record = never happened)", got)
	}
	if got := len(m2.Active()); got != 0 {
		t.Errorf("active after recovery: got %d, want 0", got)
	}
	// The abort must be durable, not just an in-memory decision: restart again.
	if err := m2.Close(); err != nil {
		t.Fatal(err)
	}
	m3, err := OpenTxMgr(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m3.Close()
	if got := m3.Status(dangling); got != StatusAborted {
		t.Errorf("crash victim after second restart: got %s, want aborted", got)
	}
}
