# Milestone 2 — Visibility as a pure function

Reads are `visible(version, snapshot, clog)` and nothing else. This note
walks the function line by line and maps each clause to the anomaly it
kills. For a fully worked numeric example, see VISIBILITY_WALKTHROUGH.md —
its scenario is reproduced verbatim by `TestVisibilityWalkthrough`.

## The function

```go
func visible(v *Version, snap Snapshot, clog *Clog) bool {
    // Born?
    if clog.Status(v.Xmin) != StatusCommitted { return false }  // (1)
    if v.Xmin >= snap.Xmax                  { return false }    // (2)
    if snap.inFlight(v.Xmin)                { return false }    // (3)

    // Still alive?
    if v.Xmax == 0                            { return true }   // (4)
    if clog.Status(v.Xmax) == StatusAborted { return true }     // (5)
    if clog.Status(v.Xmax) == StatusInProgress { return true }  // (6)
    if snap.inFlight(v.Xmax)                { return true }     // (7)
    if v.Xmax >= snap.Xmax                  { return true }     // (8)
    return false                                                // (9)
}
```

(The real code writes (5)/(6) as a `switch`; flattened here for numbering.)

## The born clauses

**(1) `xmin` must be committed → kills the dirty read.** If the creating
transaction aborted or is still running, the version does not exist as far as
anyone else is concerned. One CLOG lookup is the entire enforcement
mechanism: no locks, no coordination with the writer. This clause also makes
aborts free — an aborted transaction's versions are invisible from the
moment the CLOG says so; nobody ever cleans them up (GC does, much later,
for space).

**(2) `xmin < snap.xmax` → the snapshot's time boundary.** Versions born
after the snapshot was taken are invisible, even though their creators have
committed by the time you look. This is half of repeatable read.

**(3) `xmin ∉ snap.active` → the other half.** The active list exists
because txid order is not commit order: a transaction can hold a low txid
and commit *after* a higher-numbered one. Without this clause, a transaction
that was in flight at snapshot time and committed later would leak into the
snapshot. With it, repeatable read is complete: within one snapshot, the
same read always returns the same version.

## The alive clauses

**(4) `xmax == 0` → alive.** Nobody has updated or deleted this version.
One lookup total — the common case is the cheapest.

**(5) `xmax` aborted → alive.** The transaction that killed this version
failed. The kill never happened; the version lives on. Again: abort needs no
cleanup, the CLOG lookup *is* the undo.

**(6) `xmax` in progress → alive.** An uncommitted delete is not a delete.
This clause must come *before* the snapshot-boundary checks (7)/(8): under
milestone 2's trivial snapshot the active list is always empty, so a live
uncommitted killer would otherwise fall through to (9) and the version would
vanish mid-transaction — a dirty read of a delete. `TestInFlightDeleteNotVisible`
locks this ordering in.

**(7) `xmax ∈ snap.active` → alive.** The killer was in flight at snapshot
time. Even though it has since committed, my snapshot predates the kill.
Symmetric to (3).

**(8) `xmax >= snap.xmax` → alive.** The kill was committed, but after my
snapshot. Symmetric to (2).

**(9) otherwise → dead.** The killer committed before my snapshot. The
version is invisible; a newer one (or none) takes its place.

## Why at most one version per key is visible

Suppose two versions of key k: old (xmin=a, xmax=b) and new (xmin=b). For
old to be visible, b must be aborted / in progress / in active / ≥ xmax.
For new to be visible, b must be committed, < snap.xmax, and ∉ active.
The two conditions are disjoint — so a scan can walk newest-first and take
the first visible version per key, and it is exact, not approximate.

## The trivial snapshot, honestly

Milestone 2 reads use `{active: [], xmax: nextTxid}` per request. Two
consequences worth naming:

- **Uncommitted data is still hidden** — clause (1) does that alone, no
  active list needed.
- **Reads are not repeatable.** A transaction that commits between your two
  reads shows up in the second one. That is read-committed behavior, and it
  is exactly what milestone 3 fixes by pinning one snapshot per transaction.

## The PostgreSQL cousin

This function is `HeapTupleSatisfiesMVCC` (src/backend/utils/time/tqual.c)
with the training wheels left on. PG adds two real-world complications we
deliberately skip:

- **Hint bits.** PG caches the outcome of CLOG lookups in flags on the tuple
  header itself, because `pg_xact` lookups are shared-memory page reads. Our
  CLOG is an in-memory map, so every lookup is already O(1) and there is
  nothing to cache. (Hint bits are also why a read-only query can dirty a
  page in PG — a famous surprise.)
- **Own-transaction checks.** PG's function has extra branches for "xmin is
  my own txid" (read-your-own-writes, command ids). Ours arrives with
  milestone 4's per-transaction write sets.

## What this milestone proves

The whole read path of an MVCC database is: walk versions, apply a pure
function, take the first hit. The `/get` and `/scan` endpoints do exactly
that over the full heap, and the `X-Marchisql-Versions-Scanned` /
`X-Marchisql-Versions-Skipped-Invisible` headers make the cost visible:
reads are O(heap size) here, and the skipped count *is* the multi-version
overhead that GC (milestone 6) exists to reclaim.
