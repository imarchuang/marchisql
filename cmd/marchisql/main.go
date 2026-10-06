// Command marchisql is the HTTP shell over the MVCC storage engine.
//
// Milestone 0 placeholder: only /healthz and / exist. The engine slices land
// per PLAN.md (heap+txmgr+clog first, then visibility, snapshots, writes,
// the anomaly suite, GC, introspection, and stretch goals).
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	addr := flag.String("addr", envOr("MARCHISQL_ADDR", ":8080"), "listen address")
	dataPath := flag.String("storageDataPath", envOr("MARCHISQL_DATA", "/data"), "storage root")
	flag.Parse()

	if err := os.MkdirAll(*dataPath, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "marchisql: educational MVCC/SI engine. See PLAN.md. Endpoints land per milestone.")
	})

	log.Printf("marchisql listening on %s, data at %s", *addr, *dataPath)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
