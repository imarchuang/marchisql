// Command marchisql is the HTTP shell over the MVCC storage engine.
//
// Milestone 2: read-only endpoints (/get, /scan) driven by a trivial
// per-request snapshot — read-committed-ish behavior until milestone 3 pins
// real snapshots to transactions. Every read reports how the visibility
// machinery behaved via X-Marchisql-* headers.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/marchi/marchisql/storage"
)

func main() {
	addr := flag.String("addr", envOr("MARCHISQL_ADDR", ":8080"), "listen address")
	dataPath := flag.String("storageDataPath", envOr("MARCHISQL_DATA", "/data"), "storage root")
	flag.Parse()

	eng, err := storage.Open(*dataPath)
	if err != nil {
		log.Fatalf("open engine: %v", err)
	}
	defer eng.Close()

	log.Printf("marchisql listening on %s, data at %s", *addr, *dataPath)
	log.Fatal(http.ListenAndServe(*addr, newMux(eng)))
}

func newMux(eng *storage.Engine) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// GET /get?key=k → the newest visible version of k, or 404.
	mux.HandleFunc("GET /get", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		if key == "" {
			http.Error(w, "missing ?key=", http.StatusBadRequest)
			return
		}
		snap := storage.LatestSnapshot(eng.Tx.NextTxid())
		v, st := eng.Get(snap, key)
		setScanHeaders(w, st)
		w.Header().Set("Content-Type", "application/json")
		if v == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"key not found or not visible"}` + "\n"))
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	})

	// GET /scan → the newest visible version of every key.
	mux.HandleFunc("GET /scan", func(w http.ResponseWriter, _ *http.Request) {
		snap := storage.LatestSnapshot(eng.Tx.NextTxid())
		vs, st := eng.Scan(snap)
		setScanHeaders(w, st)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(vs)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "marchisql: educational MVCC/SI engine. See PLAN.md. Endpoints land per milestone.")
	})

	return mux
}

func setScanHeaders(w http.ResponseWriter, st storage.ScanStats) {
	w.Header().Set("X-Marchisql-Versions-Scanned", strconv.Itoa(st.Scanned))
	w.Header().Set("X-Marchisql-Versions-Skipped-Invisible", strconv.Itoa(st.SkippedInvisible))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
