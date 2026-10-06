package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func fields(t *testing.T, s string) json.RawMessage {
	t.Helper()
	return json.RawMessage(s)
}

func TestHeapAppendAndScan(t *testing.T) {
	dir := t.TempDir()
	h, err := OpenHeap(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	recs := []Version{
		{Key: "alice", Fields: fields(t, `{"on_call":true}`), Xmin: 1},
		{Key: "bob", Fields: fields(t, `{"on_call":true}`), Xmin: 1},
		{Key: "alice", Fields: fields(t, `{"on_call":false}`), Xmin: 2},
	}
	for _, v := range recs {
		if err := h.Append(v); err != nil {
			t.Fatal(err)
		}
	}

	if got := h.Len(); got != 3 {
		t.Fatalf("len: got %d, want 3", got)
	}
	vs := h.Versions()
	for i, want := range recs {
		if vs[i].Key != want.Key || vs[i].Xmin != want.Xmin {
			t.Errorf("versions[%d]: got {%s xmin=%d}, want {%s xmin=%d}",
				i, vs[i].Key, vs[i].Xmin, want.Key, want.Xmin)
		}
	}
	chain := h.Chain("alice")
	if len(chain) != 2 || chain[0].Xmin != 1 || chain[1].Xmin != 2 {
		t.Fatalf("alice chain: got %+v, want xmin 1 then 2 (oldest first)", chain)
	}
}

func TestHeapSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	h, err := OpenHeap(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Append(Version{Key: "alice", Fields: fields(t, `{"on_call":true}`), Xmin: 1}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	h2, err := OpenHeap(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	if got := h2.Len(); got != 1 {
		t.Fatalf("len after restart: got %d, want 1", got)
	}
	v := h2.Versions()[0]
	if v.Key != "alice" || v.Xmin != 1 || v.Xmax != 0 {
		t.Errorf("got %+v, want alice xmin=1 xmax=0", v)
	}
}

func TestHeapStampSupersedes(t *testing.T) {
	dir := t.TempDir()
	h, err := OpenHeap(dir, 0)
	if err != nil {
		t.Fatal(err)
	}

	v := Version{Key: "alice", Fields: fields(t, `{"on_call":true}`), Xmin: 1}
	if err := h.Append(v); err != nil {
		t.Fatal(err)
	}
	// Milestone 4's UPDATE stamps the old version's xmax by re-appending the
	// full record; (key, xmin) identity makes the later line supersede.
	stamp := v
	stamp.Xmax = 7
	if err := h.Append(stamp); err != nil {
		t.Fatal(err)
	}

	if got := h.Len(); got != 1 {
		t.Fatalf("len after stamp: got %d, want 1 (stamp supersedes, not appends)", got)
	}
	if got := h.Versions()[0].Xmax; got != 7 {
		t.Fatalf("xmax after stamp: got %d, want 7", got)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	h2, err := OpenHeap(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	if got := h2.Len(); got != 1 {
		t.Fatalf("len after restart: got %d, want 1", got)
	}
	if got := h2.Versions()[0].Xmax; got != 7 {
		t.Fatalf("xmax after restart: got %d, want 7 (stamp must survive replay)", got)
	}
}

func TestHeapSegmentRolling(t *testing.T) {
	dir := t.TempDir()
	h, err := OpenHeap(dir, 100) // 100-byte segments force rolling
	if err != nil {
		t.Fatal(err)
	}
	const n = 10
	for i := 0; i < n; i++ {
		v := Version{
			Key:    fmt.Sprintf("k%02d", i),
			Fields: fields(t, `{"v":1}`),
			Xmin:   uint64(i + 1),
		}
		if err := h.Append(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	segs := 0
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".seg" {
			segs++
		}
	}
	if segs < 2 {
		t.Fatalf("got %d segment, want rolling to produce several", segs)
	}

	h2, err := OpenHeap(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	if got := h2.Len(); got != n {
		t.Fatalf("len across segments: got %d, want %d", got, n)
	}
	for i, v := range h2.Versions() {
		if want := fmt.Sprintf("k%02d", i); v.Key != want {
			t.Errorf("versions[%d]: got key %s, want %s (append order must span segments)", i, v.Key, want)
		}
	}
}

func TestHeapTornTail(t *testing.T) {
	dir := t.TempDir()
	h, err := OpenHeap(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := h.Append(Version{Key: fmt.Sprintf("k%d", i), Fields: fields(t, `{}`), Xmin: uint64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "000001.seg")
	good, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"key":"torn","fie`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	h2, err := OpenHeap(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	if got := h2.Len(); got != 2 {
		t.Fatalf("len after torn tail: got %d, want 2", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != good.Size() {
		t.Errorf("torn tail not truncated: size %d, want %d", fi.Size(), good.Size())
	}
}
