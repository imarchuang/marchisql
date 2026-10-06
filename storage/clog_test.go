package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClogStatusesSurviveRestart(t *testing.T) {
	dir := t.TempDir()

	c, err := OpenClog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []clogRecord{
		{Txid: 1, Status: StatusInProgress},
		{Txid: 1, Status: StatusCommitted},
		{Txid: 2, Status: StatusInProgress},
		{Txid: 2, Status: StatusAborted},
		{Txid: 3, Status: StatusInProgress},
	} {
		if err := c.Record(rec.Txid, rec.Status); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2, err := OpenClog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	if got := c2.Status(1); got != StatusCommitted {
		t.Errorf("tx 1: got %s, want committed", got)
	}
	if got := c2.Status(2); got != StatusAborted {
		t.Errorf("tx 2: got %s, want aborted", got)
	}
	if got := c2.Status(3); got != StatusInProgress {
		t.Errorf("tx 3: got %s, want in_progress", got)
	}
	if got := c2.Status(999); got != StatusUnknown {
		t.Errorf("tx 999: got %s, want unknown", got)
	}
	if got := c2.Max(); got != 3 {
		t.Errorf("max: got %d, want 3", got)
	}
}

func TestClogTornTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clog.jsonl")

	c, err := OpenClog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Record(1, StatusCommitted); err != nil {
		t.Fatal(err)
	}
	good, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash mid-append: a partial record with no trailing newline.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"txid":2,"status":"committ`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	c2, err := OpenClog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	if got := c2.Status(1); got != StatusCommitted {
		t.Errorf("tx 1: got %s, want committed", got)
	}
	if got := c2.Status(2); got != StatusUnknown {
		t.Errorf("tx 2 (torn): got %s, want unknown", got)
	}
	// The torn tail must be truncated so future appends stay clean.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != good.Size() {
		t.Errorf("torn tail not truncated: size %d, want %d", fi.Size(), good.Size())
	}
}
