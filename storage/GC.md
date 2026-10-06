# Milestone 6 — GC and the oldest-snapshot horizon

Versions are reclaimed only when no open snapshot can still see them. A
transaction that stays open pins that line, and every update behind it
becomes garbage that cannot be thrown away.

## The horizon

```text
horizon = min(open snapshot.xmin)    or nextTxid, if nothing is open
```

A version is dead when its `xmax` is committed and `xmax < horizon`. Every
open snapshot has `xmin >= horizon`, so that `xmax` is below every one of
them: the killer was not in flight when the snapshot was taken, and it has
committed. No current snapshot can treat the version as alive, and a future
snapshot's xmin only moves forward. The version is garbage.

`xmax == 0` is alive. `xmax` aborted or still in progress is not dead — the
kill did not stick, or it has not stuck yet.

## What a long transaction does

`TestLongTransactionPinsHistory` opens transaction A, then updates the same
key 1000 times and commits each update. A's snapshot xmin is A's own txid
(nothing older was in flight). Every superseded version has an `xmax` from
a later transaction, so `xmax < horizon` is false. `ForceGC` collects
nothing. Commit A, and the horizon jumps to the next txid; the same call
collects all 1000.

That is the operational lesson: a forgotten transaction does not just hold
a few bytes of snapshot metadata. It holds the history of every key written
since it began.

## The swap

The heap is append-only between collections, so reclaiming space means
rewriting. A pass writes the surviving versions to `.publishing-NNNNNN.seg`,
fsyncs them, then renames `.publishing-MANIFEST` onto `MANIFEST`. The rename
is the commit. Open trusts `MANIFEST` when it exists and ignores every other
segment file. A crash before the rename leaves the old manifest (or no
manifest) in place, and the next open deletes the unpublished files.

The background pass rewrites only when the dead ratio is above
`DefaultGCThreshold` (0.25). `POST /internal/force_gc` uses a threshold of
0, so any dead version is collected. The response carries

```text
X-Marchisql-Versions-Collected
X-Marchisql-Segments-Rewritten
X-Marchisql-Horizon
```

## Production cousins

| Here | PostgreSQL | InnoDB | TiKV |
|---|---|---|---|
| horizon | oldest `xmin` among snapshots, including replication slots | purge view / history list | GC safepoint |
| dead version | a tuple whose `xmax` is committed and below that xmin | an undo record no ReadView can reach | a version older than the safepoint |
| rewrite | VACUUM rewrites pages and truncates | purge threads trim the undo history list | compaction drops old MVCC keys |
| the pin | a long transaction, or a slot, blocks VACUUM | history list length grows | safepoint cannot advance |

PostgreSQL's 32-bit txid is why VACUUM also has to **freeze**: once a
committed xid is older than the wraparound horizon it must be replaced with
a special frozen xid, or the next wrap would make it look like the future.
Our txid is 64 bits, so the horizon only decides what is garbage. It never
has to rewrite a live version just to keep the counter from lapping it.
