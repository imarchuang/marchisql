# Milestone 1 — Versions, txids, and the CLOG

The physical foundation: what a version *is*, who knows whether its creator
committed, and why the on-disk format looks like this.

## The shape of a version

```json
{"key":"alice","fields":{"on_call":true},"xmin":3,"xmax":0}
```

`xmin` = txid that gave birth to this version. `xmax` = txid that killed it
(updated or deleted it); `xmax == 0` means "alive". Every visibility decision
in milestone 2 is a pure function of these two integers, the reader's
snapshot, and the CLOG. Nothing else is ever consulted.

## Why `xmin`/`xmax` and not per-key version vectors

A per-key version vector (each key carries its own counter: alice v1, v2, …)
answers ordering *within one key* but cannot answer the question MVCC reads
actually ask: **"was the writer of this version committed, from my snapshot's
point of view?"** That is a *global* question about a transaction, not a
per-key question. With per-key counters you would still need a global commit
registry to make multi-key writes atomically visible, and every read would
have to reconstruct "which per-key version was current at my snapshot" — the
snapshot could no longer be O(1) metadata.

The PostgreSQL model separates the three concerns cleanly:

- versions carry **birth/death txids** (`xmin`/`xmax`),
- the CLOG carries **outcomes** (committed / aborted),
- the snapshot carries **the boundary** (`xmin`, `xmax`, active list).

Visibility = two integer compares + one CLOG lookup. That separation is the
design; everything else in this project is consequence.

## The CLOG is the source of truth for transaction existence

`clog/clog.jsonl` records every status transition, fsynced on write:

```json
{"txid":3,"status":"in_progress"}
{"txid":3,"status":"committed"}
```

Three ordering rules substitute for a WAL:

1. **Begin before handout.** The `in_progress` record is fsynced before the
   txid is returned, so a version's `xmin` can never reference a transaction
   the CLOG doesn't know. This also makes txid-counter recovery trivial:
   `next = max(txid in CLOG) + 1`.
2. **Heap before commit.** `Heap.Append` fsyncs before returning, so by the
   time a `committed` record is durable, every version it publishes is
   durable. Commit is then literally one CLOG append — the same "commit is
   just a CLOG write" trick as PostgreSQL.
3. **Crash = abort.** On restart, any txid left `in_progress` is marked
   `aborted` (durably). Without a WAL there is nothing to redo: a transaction
   that never reached its commit record never happened, and its versions are
   invisible because the CLOG will never say otherwise.

A crash can tear the last line of any append-only file. Both the heap and the
CLOG treat a final line without a trailing newline as torn, drop it, and
truncate. For the CLOG this is safe by rule 3; for the heap, by rule 2 the
torn version was never published by any commit.

## Append-only heap, last-wins replay

PostgreSQL stamps `xmax` by mutating the dead tuple's header **in place** —
which is why PG has hint bits (caching CLOG lookups onto the tuple) and a
rich folklore around tuple-header corruption. Our format is JSONL, where
in-place update is impractical, so we re-append the full record with the new
`xmax` and replay with **last-wins on `(key, xmin)`** — a transaction writes
at most one version of a key, so that pair is a version's identity. The dead
line left behind is exactly the kind of garbage milestone 6's GC segment
rewrite reclaims. Same logical effect as PG, different physical mechanics,
and it keeps every file append-only.

## 64-bit txids, or: why we dodge freeze

PostgreSQL's txid counter is 32 bits. After ~2 billion transactions it wraps,
and old committed versions would suddenly look like they belong to *future*
transactions — hence `vacuum freeze`, `relfrozenxid`, and anti-wraparound
emergency vacuums. We use a 64-bit counter: at a million tx/s it wraps in
~584,000 years. The teaching point is that wraparound is a *storage-format
budget constraint*, not something MVCC fundamentally requires.

## How InnoDB stores the same three things

InnoDB keeps the **latest** version in the clustered index: each record
carries `DB_TRX_ID` (its `xmin`) and `DB_ROLL_PTR`, a pointer into the **undo
log**, where older versions live as a newest→oldest chain. There is no
`xmax` on the record — to decide visibility you walk the undo chain until you
find a version your ReadView can see, and "old version storage" is the undo
segments, reclaimed by **purge** once no ReadView can reach them (the
"history list length" every MySQL operator watches). Commit status lives in
the trx system, not beside the row. Same three ideas — version ids, an
outcome registry, a snapshot boundary — but history is *moved out* of the
main structure instead of left in place.

## How TiKV stores the same three things

TiKV (Percolator-style) puts the timestamp **in the key**: `key@commit_ts`
in the write column-family, so versions of a key sit next to each other in
the LSM, newest first. In-progress state lives in a separate lock CF; the
commit record is a write CF entry. And because timestamps come from a
centralized TSO (total order), a snapshot is a *single integer*: read at
`ts = T` means "latest `commit_ts ≤ T`". Our snapshot instead needs the
`active[]` list precisely because our txid allocation order ≠ commit order —
a tx can hold a low txid and commit after a higher one. That contrast is the
deepest reason milestone 3's snapshot looks the way it does.

## Deliberate simplifications (and their prices)

- **JSONL, not binary pages.** Inspectable with `tail -f`; pays in density
  and parse cost. Worth it: the files are the teaching aid.
- **fsync per append.** No WAL, no group commit; throughput is awful and we
  don't care. The ordering argument above is what matters.
- **Single writer, single node.** One active segment, one mutex. MVCC
  concurrency semantics are unaffected.
