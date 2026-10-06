package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/marchi/marchisql/storage"
)

// seed builds a small world: alice has a superseded v1 and a committed v2;
// bob has only an uncommitted version.
func seed(t *testing.T) *storage.Engine {
	t.Helper()
	eng, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	tx1, _ := eng.Tx.Begin()
	v1 := storage.Version{Key: "alice", Fields: json.RawMessage(`{"v":1}`), Xmin: tx1}
	if err := eng.Heap.Append(v1); err != nil {
		t.Fatal(err)
	}
	if err := eng.Tx.Commit(tx1); err != nil {
		t.Fatal(err)
	}

	tx2, _ := eng.Tx.Begin()
	stamp := v1
	stamp.Xmax = tx2
	if err := eng.Heap.Append(stamp); err != nil {
		t.Fatal(err)
	}
	if err := eng.Heap.Append(storage.Version{Key: "alice", Fields: json.RawMessage(`{"v":2}`), Xmin: tx2}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Tx.Commit(tx2); err != nil {
		t.Fatal(err)
	}

	tx3, _ := eng.Tx.Begin()
	if err := eng.Heap.Append(storage.Version{Key: "bob", Fields: json.RawMessage(`{"v":1}`), Xmin: tx3}); err != nil {
		t.Fatal(err)
	}
	// tx3 never commits.
	return eng
}

func TestGetEndpoint(t *testing.T) {
	eng := seed(t)
	defer eng.Close()
	srv := httptest.NewServer(newMux(eng))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/get?key=alice")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Marchisql-Versions-Scanned"); got != "1" {
		t.Errorf("scanned header: got %q, want 1 (newest version visible immediately)", got)
	}
	var v storage.Version
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.Key != "alice" || string(v.Fields) != `{"v":2}` {
		t.Errorf("body: got %+v, want alice {v:2}", v)
	}
}

func TestGetHidesUncommitted(t *testing.T) {
	eng := seed(t)
	defer eng.Close()
	srv := httptest.NewServer(newMux(eng))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/get?key=bob")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404 (bob's only version is uncommitted)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Marchisql-Versions-Skipped-Invisible"); got != "1" {
		t.Errorf("skipped header: got %q, want 1", got)
	}
}

func TestGetMissingKeyParam(t *testing.T) {
	eng := seed(t)
	defer eng.Close()
	srv := httptest.NewServer(newMux(eng))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/get")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
}

func TestScanEndpoint(t *testing.T) {
	eng := seed(t)
	defer eng.Close()
	srv := httptest.NewServer(newMux(eng))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/scan")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Marchisql-Versions-Scanned"); got != "3" {
		t.Errorf("scanned header: got %q, want 3 (full heap walk)", got)
	}
	if got := resp.Header.Get("X-Marchisql-Versions-Skipped-Invisible"); got != "2" {
		t.Errorf("skipped header: got %q, want 2 (superseded alice + uncommitted bob)", got)
	}
	var vs []storage.Version
	if err := json.NewDecoder(resp.Body).Decode(&vs); err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 || vs[0].Key != "alice" || string(vs[0].Fields) != `{"v":2}` {
		t.Errorf("body: got %+v, want [alice {v:2}]", vs)
	}
}
