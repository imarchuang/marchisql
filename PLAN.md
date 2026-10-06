# marchisql — plan

Core goal: **learn how Snapshot Isolation is actually implemented** by building
the smallest honest MVCC engine — versioned rows, transaction ids, a commit
status log, per-transaction snapshots, visibility rules, first-writer-wins, and
version GC — as a series of small, reviewable slices.

Style follows marchilogs: every milestone is one PR-sized slice with a design
note in `storage/`, an HTTP surface you can drive with `curl`, and tests that
reproduce the anomaly (or prove its absence) instead of just asserting happy
paths.

---

## Guiding decisions (made up front)

| Question | Choice | Why |
|---|---|---|
| Data model | Key → row (opaque JSON fields) | We are learning SI, not SQL |
| Version metadata | PostgreSQL-style `xmin` / `xmax` per version | The classic teaching model; forces you to build a CLOG |
| Old versions stored | In-place in the heap file (append new version, mark old) | PG-style; undo-log (InnoDB) and ts-keyed LSM (TiKV) are documented alternatives |
| Transaction ids | 64-bit counter, monotonic | Skip PG's 32-bit wraparound; note it in a design doc |
| Commit status | `clog.json` / in-memory map txid → status | PG's `pg_xact` in miniature |
| Snapshot content | `{xmin, xmax, activeTxids[]}` | O(1) to take; the whole point |
| Conflict rule | First-writer-wins on the **latest** version | SI semantics; write skew falls out naturally |
| Durability | fsync-on-commit JSONL segments | Enough to restart; no WAL until a later slice |
| Distribution | None, single node | SI is already hard enough |

Out of scope forever: SQL parsing, query planning, secondary indexes, joins.
A primary-key map plus full scans is all the visibility rules need.

---

## Milestone 1 — Versioned heap + txid counter + CLOG

**Teaches:** the physical shape of a version; that commit status is tracked
separately from the row.

Build:

- `storage/heap.go` — append-only segment files of version records:

```text
{key, fields, xmin, xmax}   // xmax = 0 means "alive"
```

- `storage/txmgr.go` — `Begin() txid`, `Commit(txid)`, `Abort(txid)`; 64-bit
  counter; statuses: `in_progress | committed | aborted`.
- `storage/clog.go` — the commit-status map, persisted as JSONL
  (`txid,status` per line), fsynced on commit.

On-disk layout:

```text
data/
  heap/000001.seg      # version records, append-only
  clog/clog.jsonl      # txid → status
```

Tests: insert versions, restart, statuses survive; aborted txid stays aborted.

Design note: `storage/VERSIONS.md` — why `xmin/xmax` and not per-key version
vectors; one paragraph each on how InnoDB (undo chain) and TiKV
(key+timestamp in LSM) store the same information differently.

---

## Milestone 2 — Visibility as a pure function

**Teaches:** reads are `visible(version, snapshot, clog)` and nothing else.

Build:

- `storage/visibility.go`:

```text
visible(v, snap):
  v.xmin committed?            (clog lookup)
  v.xmin not in snap.active?
  v.xmin < snap.xmax?
  AND (v.xmax == 0
       OR v.xmax aborted
       OR v.xmax in snap.active
       OR v.xmax >= snap.xmax)
```

- For milestone 2 only, every read gets a trivial snapshot
  `{active: [], xmax: nextTxid}` — i.e. read-committed-ish behavior.
- `GET /get?key=k` and `GET /scan` doing a full heap walk, returning the first
  visible version per key, with headers:

```text
X-Marchisql-Versions-Scanned
X-Marchisql-Versions-Skipped-Invisible
```

Tests: uncommitted version invisible; aborted version invisible; old committed
version invisible once a newer committed version exists.

Design note: `storage/VISIBILITY.md` — walk the function line by line; map each
clause to the anomaly it prevents (dirty read ← "xmin must be committed").

---

## Milestone 3 — Real transactions and real snapshots

**Teaches:** taking a snapshot is O(1) metadata; repeatable read falls out.

Build:

- `POST /begin` → allocates txid, captures snapshot
  `{xmin: oldestActive, xmax: nextTxid, active: […]}`, stores it on the tx
  handle.
- All reads within the tx reuse that one snapshot.
- `POST /commit`, `POST /abort` (abort marks CLOG; versions written by the tx
  stay on disk and are simply invisible to everyone — GC's problem later).

Tests (the money tests):

- **Repeatable read**: tx A reads key k; tx B updates k and commits; A reads k
  again → same value, same version.
- **Snapshot is O(1)**: begin a tx over a 1M-row heap, assert begin latency is
  flat (no copying).

Design note: `storage/SNAPSHOT.md` — "a snapshot is `{xmin, xmax, active[]}`,
not a copy"; compare with InnoDB ReadView and TiDB's single TSO timestamp;
explain why the active-list is needed at all (in-flight-at-snapshot-time
transactions may commit later, and their versions must stay invisible).

---

## Milestone 4 — Writes: first-writer-wins on the latest version

**Teaches:** the read/write asymmetry. Reads use the snapshot; writes must
confront the **latest** version, snapshot be damned.

Build:

- `POST /update` / `POST /delete` within a tx:
  1. Find the **latest** version of the key (ignoring the snapshot).
  2. If its `xmax` is held by another **in-progress** tx → block or fail
     (start with fail-fast; blocking is a stretch).
  3. If that holder **committed after my snapshot** → abort with
     `serialization_failure` (first-writer-wins).
  4. Otherwise write my new version (`xmin = me`), stamp old `xmax = me`.
- Per-tx write set for commit/abort bookkeeping.

Tests:

- Two txs update the **same** key concurrently → second gets
  `serialization_failure`. (Same-row oversell is impossible under SI.)
- Update-then-read-your-own-writes inside one tx.

Design note: `storage/WRITE_CONFLICT.md` — the asymmetry made explicit:
"read path: snapshot; write path: latest". This one sentence is the seed of
write skew; plant it here, harvest it in milestone 5.

---

## Milestone 5 — The anomaly suite (the centerpiece)

**Teaches:** what SI prevents and what it does not — demonstrated, not asserted.

Build `storage/anomaly_test.go` plus matching `curl` scripts in `cmd/demo/`:

| Scenario | Expected under SI |
|---|---|
| Dirty read | prevented (M2 test) |
| Non-repeatable read | prevented (M3 test) |
| Phantom (`WHERE on_call=true` count changes) | prevented — phantoms can't exist within one snapshot |
| Same-row lost update | prevented (M4 test) |
| **Write skew: doctors on call** | **reproduced** — A and B each see "2 on call", each removes a different doctor, both commit, 0 on call |
| Write skew: cross-account balance sum ≥ 0 | reproduced |

The doctors scenario must be runnable end-to-end over HTTP (the README quick
start), because reproducing it by hand in an interview is the point of the
whole project.

Design note: `storage/WRITE_SKEW.md` — the rw-antidependency cycle drawn out:

```text
A reads {alice, bob} → B writes bob
B reads {alice, bob} → A writes alice
cycle ⇒ not serializable; SI never saw it because no row was written twice
```

---

## Milestone 6 — GC (vacuum) and the oldest-snapshot horizon

**Teaches:** versions are reclaimed only when no active snapshot can see them;
long transactions pin history.

Build:

- `storage/gc.go` — background pass over heap segments:
  - horizon = `min(activeSnapshots.xmin)` (or nextTxid if none);
  - a dead version (`xmax` committed, `xmax < horizon`) is dropped;
  - segment rewrite when dead ratio > threshold; manifest swap for atomic
    replacement (borrow marchilogs' `.publishing-*` + rename pattern).
- `POST /internal/force_gc`; headers/metrics: versions collected, segments
  rewritten, current horizon.

Tests:

- Dead versions disappear after all old snapshots close.
- **Pinning**: open tx A, churn a key 1000 times, force GC → nothing collected;
  close A → GC collects. This is the "long transaction is poison" lesson,
  reproduced.

Design note: `storage/GC.md` — map each piece to its production cousin:
PG VACUUM, InnoDB purge + history list length, TiKV GC safepoint; note PG's
32-bit wraparound/freeze as the thing our 64-bit txid dodges.

---

## Milestone 7 — Observability

**Teaches:** you can't claim to understand a mechanism you can't see.

Build:

- `GET /internal/versions?key=k` — dump the full version chain with
  xmin/xmax/status per version.
- `GET /internal/txs` — active transactions, their snapshots, ages.
- `GET /internal/heap/stats` — live vs dead versions per segment.
- `X-Marchisql-*` headers on every read/write (scanned, skipped, conflicts,
  snapshot age).

---

## Milestone 8 — Stretch: escaping write skew

Pick one or both, each as its own slice:

1. **`SELECT ... FOR UPDATE`** — lock the rows you read; the doctors scenario
   with `FOR UPDATE` on the on-call set must **fail to skew** (second tx
   blocks/aborts). Teaches "materialize the conflict".
2. **Mini-SSI** — track rw-antidependencies (who read what / who wrote what),
   detect the dangerous cycle at commit, abort one side. Even a crude,
   over-aborting version teaches why PostgreSQL's SSI is subtle.

Design note: `storage/BEYOND_SI.md` — three doors out of write skew
(SSI, materialized conflicts, single-row shape transformation) and when each
is worth its cost.

---

## Suggested PR series

```text
1. heap + txmgr + clog            (M1)
2. visibility + read endpoints    (M2)
3. begin/commit/abort + snapshots (M3)
4. update/delete + FWW            (M4)
5. anomaly suite + demo scripts   (M5)
6. gc + horizon + force_gc        (M6)
7. internal introspection APIs    (M7)
8. for-update and/or mini-SSI     (M8, stretch)
```

Each PR: code + one `storage/*.md` note + tests that reproduce the relevant
anomaly or prove its absence. Keep every slice small enough to review in one
sitting — same discipline as marchilogs.

---

## Definition of done

You can whiteboard, without notes:

1. The exact contents of a snapshot and why taking one is O(1).
2. The visibility function, clause by clause, and which anomaly each clause kills.
3. Why writes must look at the latest version while reads look at the snapshot —
   and how that single asymmetry produces write skew.
4. Why GC cannot pass the oldest active snapshot, and what a long transaction
   does to storage.
5. How InnoDB (undo chain) and TiKV (timestamped LSM keys) implement the same
   three ideas with different physical layouts.
