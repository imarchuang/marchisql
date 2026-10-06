// Command marchisql is the HTTP shell over the MVCC storage engine.
//
// Milestone 3: real transactions with real snapshots. POST /begin allocates
// a txid and captures a snapshot once; every read carrying ?tx=<id> reuses
// that snapshot, which is the whole of repeatable read. Reads without ?tx=
// keep milestone 2's trivial per-request snapshot (read-committed-ish).
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/marchi/marchisql/storage"
)

type server struct {
	eng *storage.Engine
	txs *storage.TxStore
}

func main() {
	addr := flag.String("addr", envOr("MARCHISQL_ADDR", ":8080"), "listen address")
	dataPath := flag.String("storageDataPath", envOr("MARCHISQL_DATA", "/data"), "storage root")
	flag.Parse()

	eng, err := storage.Open(*dataPath)
	if err != nil {
		log.Fatalf("open engine: %v", err)
	}
	defer eng.Close()

	s := &server{eng: eng, txs: storage.NewTxStore()}
	log.Printf("marchisql listening on %s, data at %s", *addr, *dataPath)
	log.Fatal(http.ListenAndServe(*addr, s.mux()))
}

func (s *server) mux() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// POST /begin → {"txid": N, "snapshot": {...}}
	mux.HandleFunc("POST /begin", func(w http.ResponseWriter, _ *http.Request) {
		tx, err := s.eng.BeginTx()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.txs.Put(tx)
		writeJSON(w, map[string]any{"txid": tx.ID, "snapshot": tx.Snap})
	})

	// POST /commit {"tx": N}
	mux.HandleFunc("POST /commit", func(w http.ResponseWriter, r *http.Request) {
		s.finishTx(w, r, func(tx *storage.Tx) error {
			return s.eng.CommitTx(tx)
		})
	})

	// POST /abort {"tx": N}
	mux.HandleFunc("POST /abort", func(w http.ResponseWriter, r *http.Request) {
		s.finishTx(w, r, func(tx *storage.Tx) error {
			return s.eng.AbortTx(tx)
		})
	})

	// GET /get?key=k[&tx=N] → the newest visible version of k, or 404.
	// With ?tx= the transaction sees its own uncommitted writes.
	mux.HandleFunc("GET /get", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		if key == "" {
			http.Error(w, "missing ?key=", http.StatusBadRequest)
			return
		}
		tx, snap, err := s.snapshotFor(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var v *storage.Version
		var st storage.ScanStats
		if tx != nil {
			v, st = s.eng.GetInTx(tx, key)
		} else {
			v, st = s.eng.Get(snap, key)
		}
		setScanHeaders(w, st)
		w.Header().Set("Content-Type", "application/json")
		if v == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"key not found or not visible"}` + "\n"))
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	})

	// GET /scan[?tx=N] → the newest visible version of every key.
	mux.HandleFunc("GET /scan", func(w http.ResponseWriter, r *http.Request) {
		tx, snap, err := s.snapshotFor(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var vs []*storage.Version
		var st storage.ScanStats
		if tx != nil {
			vs, st = s.eng.ScanInTx(tx)
		} else {
			vs, st = s.eng.Scan(snap)
		}
		setScanHeaders(w, st)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(vs)
	})

	// POST /update {"tx":N, "key":"k", "fields":{...}}
	mux.HandleFunc("POST /update", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tx     uint64          `json:"tx"`
			Key    string          `json:"key"`
			Fields json.RawMessage `json:"fields"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
			return
		}
		tx, err := s.txs.Get(body.Tx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.eng.Update(tx, body.Key, body.Fields); err != nil {
			s.writeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"txid": tx.ID, "key": body.Key, "written": true})
	})

	// POST /delete {"tx":N, "key":"k"}
	mux.HandleFunc("POST /delete", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tx  uint64 `json:"tx"`
			Key string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
			return
		}
		tx, err := s.txs.Get(body.Tx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.eng.Delete(tx, body.Key); err != nil {
			s.writeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"txid": tx.ID, "key": body.Key, "deleted": true})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "marchisql: educational MVCC/SI engine. See PLAN.md. Endpoints land per milestone.")
	})

	return mux
}

// snapshotFor resolves the read boundary: the transaction's pinned snapshot
// when ?tx= is given, otherwise a trivial per-request snapshot. The returned
// tx is nil in the trivial case.
func (s *server) snapshotFor(r *http.Request) (*storage.Tx, storage.Snapshot, error) {
	txq := r.URL.Query().Get("tx")
	if txq == "" {
		return nil, storage.LatestSnapshot(s.eng.Tx.NextTxid()), nil
	}
	txid, err := strconv.ParseUint(txq, 10, 64)
	if err != nil {
		return nil, storage.Snapshot{}, fmt.Errorf("bad ?tx=%q", txq)
	}
	tx, err := s.txs.Get(txid)
	if err != nil {
		return nil, storage.Snapshot{}, err
	}
	return tx, tx.Snap, nil
}

// writeError maps write-path errors to HTTP statuses: conflicts are 409,
// everything else is 400.
func (s *server) writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, storage.ErrSerializationFailure) || errors.Is(err, storage.ErrTxConflict) {
		status = http.StatusConflict
	}
	http.Error(w, err.Error(), status)
}

func (s *server) finishTx(w http.ResponseWriter, r *http.Request, finish func(*storage.Tx) error) {
	var body struct {
		Tx uint64 `json:"tx"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	tx, err := s.txs.Get(body.Tx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := finish(tx); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.txs.Delete(tx.ID)
	writeJSON(w, map[string]any{"txid": tx.ID, "status": s.eng.Tx.Status(tx.ID).String()})
}

func setScanHeaders(w http.ResponseWriter, st storage.ScanStats) {
	w.Header().Set("X-Marchisql-Versions-Scanned", strconv.Itoa(st.Scanned))
	w.Header().Set("X-Marchisql-Versions-Skipped-Invisible", strconv.Itoa(st.SkippedInvisible))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
