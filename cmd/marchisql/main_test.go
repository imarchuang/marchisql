package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func newTestServer(t *testing.T) (*httptest.Server, *storage.Engine) {
	t.Helper()
	eng := seed(t)
	s := &server{eng: eng, txs: storage.NewTxStore()}
	srv := httptest.NewServer(s.mux())
	t.Cleanup(func() {
		srv.Close()
		eng.Close()
	})
	return srv, eng
}

func beginTx(t *testing.T, srv *httptest.Server) uint64 {
	t.Helper()
	resp, err := http.Post(srv.URL+"/begin", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("begin: status %d", resp.StatusCode)
	}
	var body struct {
		Txid uint64 `json:"txid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Txid
}

func finishTx(t *testing.T, srv *httptest.Server, path string, txid uint64) *http.Response {
	t.Helper()
	body := bytes.NewBufferString(fmt.Sprintf(`{"tx":%d}`, txid))
	resp, err := http.Post(srv.URL+path, "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func getVersion(t *testing.T, url string) (*storage.Version, *http.Response) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, resp
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get %s: status %d", url, resp.StatusCode)
	}
	var v storage.Version
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return &v, resp
}

func TestGetEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)

	v, resp := getVersion(t, srv.URL+"/get?key=alice")
	if v == nil || v.Key != "alice" || string(v.Fields) != `{"v":2}` {
		t.Errorf("body: got %+v, want alice {v:2}", v)
	}
	if got := resp.Header.Get("X-Marchisql-Versions-Scanned"); got != "1" {
		t.Errorf("scanned header: got %q, want 1 (newest version visible immediately)", got)
	}
}

func TestGetHidesUncommitted(t *testing.T) {
	srv, _ := newTestServer(t)

	v, resp := getVersion(t, srv.URL+"/get?key=bob")
	if v != nil {
		t.Errorf("got %+v, want nil (bob's only version is uncommitted)", v)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Marchisql-Versions-Skipped-Invisible"); got != "1" {
		t.Errorf("skipped header: got %q, want 1", got)
	}
}

func TestGetMissingKeyParam(t *testing.T) {
	srv, _ := newTestServer(t)

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
	srv, _ := newTestServer(t)

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

// The milestone-3 money test over HTTP: a transaction's two reads return the
// same version even though another transaction committed in between.
func TestRepeatableReadOverHTTP(t *testing.T) {
	srv, eng := newTestServer(t)

	// A begins and reads alice = {v:2}.
	a := beginTx(t, srv)
	v1, _ := getVersion(t, fmt.Sprintf("%s/get?key=alice&tx=%d", srv.URL, a))
	if v1 == nil || string(v1.Fields) != `{"v":2}` {
		t.Fatalf("A's first read: got %+v, want {v:2}", v1)
	}

	// B updates alice to {v:3} and commits.
	b := beginTx(t, srv)
	old := eng.Heap.Chain("alice")[1] // the live {v:2} version
	stamp := *old
	stamp.Xmax = b
	if err := eng.Heap.Append(stamp); err != nil {
		t.Fatal(err)
	}
	if err := eng.Heap.Append(storage.Version{Key: "alice", Fields: json.RawMessage(`{"v":3}`), Xmin: b}); err != nil {
		t.Fatal(err)
	}
	resp := finishTx(t, srv, "/commit", b)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B commit: status %d", resp.StatusCode)
	}

	// A reads again: still {v:2}.
	v2, _ := getVersion(t, fmt.Sprintf("%s/get?key=alice&tx=%d", srv.URL, a))
	if v2 == nil || string(v2.Fields) != `{"v":2}` {
		t.Fatalf("A's second read: got %+v, want {v:2} (repeatable read)", v2)
	}

	// A read without ?tx= sees {v:3}.
	v3, _ := getVersion(t, srv.URL+"/get?key=alice")
	if v3 == nil || string(v3.Fields) != `{"v":3}` {
		t.Fatalf("fresh read: got %+v, want {v:3}", v3)
	}

	resp = finishTx(t, srv, "/abort", a)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("A abort: status %d", resp.StatusCode)
	}

	// The aborted txid is gone from the store.
	resp2, err := http.Get(fmt.Sprintf("%s/get?key=alice&tx=%d", srv.URL, a))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("read with finished tx: got %d, want 400", resp2.StatusCode)
	}
}

// The README quick start, over HTTP: both transactions commit, and nobody
// is left on call.
func TestDoctorsWriteSkewOverHTTP(t *testing.T) {
	eng, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &server{eng: eng, txs: storage.NewTxStore()}
	srv := httptest.NewServer(s.mux())
	t.Cleanup(func() {
		srv.Close()
		eng.Close()
	})

	seed := beginTx(t, srv)
	postOK(t, srv, "/update", fmt.Sprintf(`{"tx":"%d","key":"alice","on_call":true}`, seed))
	postOK(t, srv, "/update", fmt.Sprintf(`{"tx":"%d","key":"bob","on_call":true}`, seed))
	resp := finishTx(t, srv, "/commit", seed)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed commit: %d", resp.StatusCode)
	}

	a := beginTx(t, srv)
	if got := countOnCall(t, srv, a); got != 2 {
		t.Fatalf("A sees %d on call, want 2", got)
	}
	b := beginTx(t, srv)
	if got := countOnCall(t, srv, b); got != 2 {
		t.Fatalf("B sees %d on call, want 2", got)
	}

	postOK(t, srv, "/update", fmt.Sprintf(`{"tx":"%d","key":"bob","on_call":false}`, b))
	resp = finishTx(t, srv, "/commit", b)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B commit: %d", resp.StatusCode)
	}
	postOK(t, srv, "/update", fmt.Sprintf(`{"tx":"%d","key":"alice","on_call":false}`, a))
	resp = finishTx(t, srv, "/commit", a)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("A commit: %d", resp.StatusCode)
	}

	if got := countOnCall(t, srv, 0); got != 0 {
		t.Fatalf("on call after both commits: got %d, want 0", got)
	}
}

func postOK(t *testing.T, srv *httptest.Server, path, body string) {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: status %d", path, resp.StatusCode)
	}
}

func countOnCall(t *testing.T, srv *httptest.Server, txid uint64) int {
	t.Helper()
	url := srv.URL + "/scan?where=on_call=true"
	if txid != 0 {
		url += fmt.Sprintf("&tx=%d", txid)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scan: status %d", resp.StatusCode)
	}
	var vs []storage.Version
	if err := json.NewDecoder(resp.Body).Decode(&vs); err != nil {
		t.Fatal(err)
	}
	return len(vs)
}

func TestForceGCEndpoint(t *testing.T) {
	eng, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &server{eng: eng, txs: storage.NewTxStore()}
	srv := httptest.NewServer(s.mux())
	t.Cleanup(func() {
		srv.Close()
		eng.Close()
	})

	seed := beginTx(t, srv)
	postOK(t, srv, "/update", fmt.Sprintf(`{"tx":%d,"key":"k","fields":{"v":1}}`, seed))
	resp := finishTx(t, srv, "/commit", seed)
	resp.Body.Close()
	next := beginTx(t, srv)
	postOK(t, srv, "/update", fmt.Sprintf(`{"tx":%d,"key":"k","fields":{"v":2}}`, next))
	resp = finishTx(t, srv, "/commit", next)
	resp.Body.Close()

	resp, err = http.Post(srv.URL+"/internal/force_gc", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("force_gc: status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Marchisql-Versions-Collected"); got != "1" {
		t.Errorf("collected: got %q, want 1", got)
	}
	if got := resp.Header.Get("X-Marchisql-Segments-Rewritten"); got != "1" {
		t.Errorf("segments: got %q, want 1", got)
	}
	if resp.Header.Get("X-Marchisql-Horizon") == "" {
		t.Error("missing horizon header")
	}
}

func TestCommitUnknownTx(t *testing.T) {
	srv, _ := newTestServer(t)
	resp := finishTx(t, srv, "/commit", 999)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}
