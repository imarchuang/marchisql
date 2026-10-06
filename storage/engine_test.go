package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The milestone-1 money test: insert versions, restart, statuses survive;
// an aborted (here: crash-aborted) txid stays aborted.
func TestEngineEndToEnd(t *testing.T) {
	dir := t.TempDir()

	e, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := e.Tx.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Heap.Append(Version{Key: "alice", Fields: json.RawMessage(`{"on_call":true}`), Xmin: tx}); err != nil {
		t.Fatal(err)
	}
	if err := e.Tx.Commit(tx); err != nil {
		t.Fatal(err)
	}
	dangling, err := e.Tx.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	// On-disk layout promised by PLAN.md milestone 1.
	for _, p := range []string{"heap/000001.seg", "clog/clog.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("layout: %s missing: %v", p, err)
		}
	}

	e2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()

	if got := e2.Tx.Status(tx); got != StatusCommitted {
		t.Errorf("committed tx after restart: got %s, want committed", got)
	}
	if got := e2.Tx.Status(dangling); got != StatusAborted {
		t.Errorf("dangling tx after restart: got %s, want aborted", got)
	}
	vs := e2.Heap.Versions()
	if len(vs) != 1 || vs[0].Key != "alice" || vs[0].Xmin != tx || vs[0].Xmax != 0 {
		t.Fatalf("versions after restart: got %+v", vs)
	}
}
