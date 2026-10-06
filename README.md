# marchisql

Educational, PostgreSQL/InnoDB-inspired **single-node MVCC key-value store** in Go.

Multi-version rows, transaction ids, commit-status tracking, per-transaction
snapshots, visibility rules, first-writer-wins conflict detection, and version
GC — built as small, reviewable slices rather than a full database clone.

**Not a database.** No SQL parser, no planner, no network protocol, no WAL-based
recovery, no replication. The goal is to learn **how Snapshot Isolation is
actually implemented** with a running binary and inspectable files under `data/`.

The whole project exists to answer one question from first principles:

> A "snapshot" is just a few bytes of metadata. The real mechanism is that every
> row keeps multiple versions, and reads pick the visible one. What code makes
> that sentence true?

See [PLAN.md](PLAN.md) for the milestone roadmap.

---

## The five things you will understand after building this

1. **A snapshot is O(1) metadata** — a txid boundary plus the list of in-flight
   transactions. Nothing is copied.
2. **UPDATE never overwrites** — it appends a new version and stamps the old
   one's `xmax`. DELETE just sets `xmax`. The row stays until GC.
3. **Visibility is a pure function** — `visible(version, snapshot, clog)` decided
   by `xmin` / `xmax` and the commit log. Reads take no locks.
4. **Reads use the snapshot, writes use the latest version** — that asymmetry is
   the entire semantics of SI, and it is exactly where write skew lives.
5. **Versions must be collected** — GC can only reclaim versions older than the
   oldest active snapshot, so a long-lived transaction pins history.

---

## Milestones at a glance

| # | Milestone | What it teaches |
|---|---|---|
| 1 | Versioned heap + txid counter + CLOG | `xmin`/`xmax`, commit status |
| 2 | Single-statement reads with full-table visibility scan | the visibility function |
| 3 | Transactions + snapshot metadata | snapshot = `{xmin, xmax, active[]}` |
| 4 | Writes: first-writer-wins on the latest version | write-write conflicts |
| 5 | Anomaly test suite | SI prevents dirty/non-repeatable/phantom, **not** write skew |
| 6 | GC (vacuum) + oldest-active-snapshot horizon | long transactions pin versions |
| 7 | Observability: version chains, per-tx stats | seeing the mechanism |
| 8 | `SELECT ... FOR UPDATE` (SSI still later) | materializing a write-skew conflict |

Each milestone is a small PR with its own design note in `storage/`, mirroring
how marchilogs grew (stream index → bloom → tombstones → …).

---

## Quick start (target shape)

**Go (1.22+):**

```bash
go test ./...
go run ./cmd/marchisql -storageDataPath=./data -addr=:8080
```

**Two overlapping transactions, watched live:**

```bash
# tx A begins, reads the on-call count
A=$(curl -s -X POST localhost:8080/begin | jq -r .txid)
curl -s "localhost:8080/scan?tx=$A&where=on_call=true"

# tx B does the same, then takes Bob off call and commits
B=$(curl -s -X POST localhost:8080/begin | jq -r .txid)
curl -s "localhost:8080/scan?tx=$B&where=on_call=true"
curl -s -X POST localhost:8080/update -d "{\"tx\":\"$B\",\"key\":\"bob\",\"on_call\":false}"
curl -s -X POST localhost:8080/commit -d "{\"tx\":\"$B\"}"

# A re-reads: still sees 2 on call (snapshot). A removes Alice and commits too.
curl -s -X POST localhost:8080/update -d "{\"tx\":\"$A\",\"key\":\"alice\",\"on_call\":false}"
curl -s -X POST localhost:8080/commit -d "{\"tx\":\"$A\"}"
# → both committed; nobody is on call. Write skew, reproduced over HTTP.

# The same scan with for_update=true locks both doctors. The second
# transaction gets HTTP 409 and must abort. See storage/BEYOND_SI.md.
```

Every response carries `X-Marchisql-*` headers (versions scanned, versions
skipped-invisible, conflicts) so the mechanism is visible without a debugger.

---

## Project layout

```text
cmd/marchisql/     HTTP server (thin shell over the engine)
storage/           Engine: heap, versions, txmgr, clog, snapshot, gc
PLAN.md            Milestone roadmap (read this first)
```

Module: `github.com/marchi/marchisql`

---

## Non-goals (for now)

- SQL parsing / planning / indexes beyond a primary key map
- WAL, crash recovery, replication, distribution
- Serializable isolation (mini-SSI is a later slice, not a promise)
- Predicate / gap locks (`FOR UPDATE` locks the rows the scan returned)
- Any production use whatsoever

---

## License

Personal / educational project unless otherwise noted.
