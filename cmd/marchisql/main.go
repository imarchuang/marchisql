// Command marchisql is the HTTP shell over the MVCC storage engine.
//
// Reads with ?tx= reuse the transaction's pinned snapshot. Reads without
// ?tx= take a trivial per-request snapshot. GET /scan accepts
// where=field=value. POST /update accepts either {"fields":{...}} or the
// README shape {"key":"...","on_call":false}, and tx may be a number or a
// string so the quick-start curls work as written.
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
	"time"

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
	eng.StartGC(30*time.Second, storage.DefaultGCThreshold)
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
		setObsHeaders(w, st, 0, snapshotAge(s.eng, tx, snap))
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
		if where := r.URL.Query().Get("where"); where != "" {
			filtered := make([]*storage.Version, 0, len(vs))
			for _, v := range vs {
				ok, err := storage.MatchWhere(v.Fields, where)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if ok {
					filtered = append(filtered, v)
				}
			}
			vs = filtered
		}
		setObsHeaders(w, st, 0, snapshotAge(s.eng, tx, snap))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(vs)
	})

	// POST /update {"tx":N, "key":"k", "fields":{...}}
	// or the README shape {"tx":"N","key":"bob","on_call":false}, where every
	// key other than tx and key becomes the new fields object.
	mux.HandleFunc("POST /update", func(w http.ResponseWriter, r *http.Request) {
		txid, key, fields, err := decodeUpdate(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tx, err := s.txs.Get(txid)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.eng.Update(tx, key, fields); err != nil {
			s.writeError(w, tx, err)
			return
		}
		setObsHeaders(w, storage.ScanStats{}, 0, s.eng.SnapshotAge(tx.Snap))
		writeJSON(w, map[string]any{"txid": tx.ID, "key": key, "written": true})
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
			s.writeError(w, tx, err)
			return
		}
		setObsHeaders(w, storage.ScanStats{}, 0, s.eng.SnapshotAge(tx.Snap))
		writeJSON(w, map[string]any{"txid": tx.ID, "key": body.Key, "deleted": true})
	})

	// GET /internal/versions?key=k → the full chain, oldest first.
	mux.HandleFunc("GET /internal/versions", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		if key == "" {
			http.Error(w, "missing ?key=", http.StatusBadRequest)
			return
		}
		writeJSON(w, s.eng.Versions(key))
	})

	// GET /internal/txs → open transactions, their snapshots, and ages.
	mux.HandleFunc("GET /internal/txs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.eng.Transactions())
	})

	// GET /internal/heap/stats → live vs dead versions per segment.
	mux.HandleFunc("GET /internal/heap/stats", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.eng.HeapStats())
	})

	// POST /internal/force_gc → collect every dead version, ignoring the ratio.
	mux.HandleFunc("POST /internal/force_gc", func(w http.ResponseWriter, _ *http.Request) {
		st, err := s.eng.ForceGC()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-Marchisql-Versions-Collected", strconv.Itoa(st.Collected))
		w.Header().Set("X-Marchisql-Segments-Rewritten", strconv.Itoa(st.Segments))
		w.Header().Set("X-Marchisql-Horizon", strconv.FormatUint(st.Horizon, 10))
		writeJSON(w, st)
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
func (s *server) writeError(w http.ResponseWriter, tx *storage.Tx, err error) {
	status := http.StatusBadRequest
	conflicts := 0
	if errors.Is(err, storage.ErrSerializationFailure) || errors.Is(err, storage.ErrTxConflict) {
		status = http.StatusConflict
		conflicts = 1
	}
	setObsHeaders(w, storage.ScanStats{}, conflicts, s.eng.SnapshotAge(tx.Snap))
	http.Error(w, err.Error(), status)
}

func snapshotAge(eng *storage.Engine, tx *storage.Tx, snap storage.Snapshot) uint64 {
	if tx != nil {
		return eng.SnapshotAge(tx.Snap)
	}
	return eng.SnapshotAge(snap)
}

func (s *server) finishTx(w http.ResponseWriter, r *http.Request, finish func(*storage.Tx) error) {
	raw, err := decodeObject(r)
	if err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	txid, err := parseTxID(raw["tx"])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tx, err := s.txs.Get(txid)
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

func setObsHeaders(w http.ResponseWriter, st storage.ScanStats, conflicts int, snapAge uint64) {
	w.Header().Set("X-Marchisql-Versions-Scanned", strconv.Itoa(st.Scanned))
	w.Header().Set("X-Marchisql-Versions-Skipped-Invisible", strconv.Itoa(st.SkippedInvisible))
	w.Header().Set("X-Marchisql-Conflicts", strconv.Itoa(conflicts))
	w.Header().Set("X-Marchisql-Snapshot-Age", strconv.FormatUint(snapAge, 10))
}

func decodeUpdate(r *http.Request) (txid uint64, key string, fields json.RawMessage, err error) {
	raw, err := decodeObject(r)
	if err != nil {
		return 0, "", nil, err
	}
	txid, err = parseTxID(raw["tx"])
	if err != nil {
		return 0, "", nil, err
	}
	if err := json.Unmarshal(raw["key"], &key); err != nil || key == "" {
		return 0, "", nil, fmt.Errorf("missing key")
	}
	if f, ok := raw["fields"]; ok {
		return txid, key, f, nil
	}
	obj := make(map[string]json.RawMessage, len(raw))
	for k, v := range raw {
		if k == "tx" || k == "key" {
			continue
		}
		obj[k] = v
	}
	if len(obj) == 0 {
		return 0, "", nil, fmt.Errorf("missing fields")
	}
	fields, err = json.Marshal(obj)
	return txid, key, fields, err
}

func decodeObject(r *http.Request) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// parseTxID accepts a JSON number or a JSON string, so {"tx":1} and {"tx":"1"}
// both work. The README quick start quotes the txid.
func parseTxID(raw json.RawMessage) (uint64, error) {
	if len(raw) == 0 {
		return 0, fmt.Errorf("missing tx")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, err
		}
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("bad tx %q", s)
		}
		return n, nil
	}
	var n uint64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("bad tx")
	}
	return n, nil
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
