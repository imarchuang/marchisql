# Milestone 4 — Writes: first-writer-wins on the latest version

The read/write asymmetry made explicit. Reads use the snapshot; writes must
confront the **latest** version, snapshot be damned. That one sentence is
the entire semantics of snapshot isolation, and it is exactly where write
skew lives.

## The asymmetry

| | reads | writes |
|---|---|---|
| boundary | the transaction's pinned snapshot | the latest version, ignoring the snapshot |
| question | "what was true when I began?" | "is anyone else writing this row right now?" |

Reads ask about the past; writes ask about the present. A transaction lives
in two times at once — it reads the world as of its snapshot, but it writes
into the world as of now. SI never reconciles the two. That is not a bug; it
is the definition.

## First-writer-wins

`checkWritable` is the whole conflict rule, applied to the latest version of
the key that was **not** written by me (`latestOther` skips my own
uncommitted versions, which live in my write set). The question is: who
wrote this version, and what happened to them?

```text
xmin in progress    → another tx is writing it now; fail fast
xmin aborted        → that write failed; the version is dead; writable
xmin committed      → did they commit after my snapshot?
                      yes → serialization_failure (I lose)
                      no  → writable (their write is in my snapshot)
```

Two distinct failures, two distinct meanings:

- **`transaction_conflict`** (fail-fast): another transaction is writing the
  same key *right now*. PostgreSQL would block until the holder ends;
  milestone 4 starts with fail-fast (blocking is a stretch). This is the
  same-row lost-update prevention: two transactions cannot both think they
  own the row.
- **`serialization_failure`** (first-writer-wins): another transaction
  *committed* a write to this key after my snapshot. Under SI the only
  correct response is to abort — retrying the write would be a lost update.
  The first writer wins; the second writer gets an error.

`TestSameRowWriteWriteConflict` is the money test: A and B both begin over
the same snapshot, A writes k and commits, B writes k and gets
`serialization_failure`. Same-row oversell is impossible under SI.

## Why writes must look at the latest version

Suppose writes used the snapshot instead. A and B both begin over a snapshot
where `k = 100`. A writes `k = 80`, commits. B writes `k = 60`, commits.
Both appended a version with `xmin` = themselves, `xmax` = 0 on the old
version. Now there are two "latest" versions of k, and the one a reader sees
depends on commit order — the classic lost update.

First-writer-wins closes that hole by making the write path check the
*latest* version's `xmax`: if someone else got there first (and committed
after my snapshot), I lose. The snapshot is irrelevant to the question "is
this row writable?" — only the latest version answers it.

## Read-your-own-writes

A transaction's own uncommitted versions are invisible to its own snapshot
(clause (1) of `visible`: `xmin` must be committed). Without a fix, a
transaction could not read what it just wrote — unusable.

The fix is a per-transaction **write set**: a map from key to the
transaction's own uncommitted version (or a tombstone for deletes). Reads
within a transaction consult the write set first:

- `GetInTx`: own write wins; own delete hides; otherwise fall through to the
  snapshot.
- `ScanInTx`: snapshot supplies the committed view, then the write set is
  overlaid — own updates replace, own inserts appear, own deletes remove.

The write set is in-memory only. A restart aborts everything anyway (crash
recovery), so there is nothing to persist.

## The seed of write skew

First-writer-wins only fires when two transactions write the **same** key.
If A writes alice and B writes bob, no `xmax` is ever contested, both
commit, and neither ever learns the other existed. That is write skew, and
it is not a bug — it is the direct consequence of "reads use the snapshot,
writes use the latest version". Milestone 5 reproduces it end-to-end;
milestone 8 sketches the ways out.

## The PostgreSQL cousin

PG's `heap_update` does the same dance: find the latest tuple, check its
`xmax`, and if another transaction holds it, either block (the default) or
fail (the `NOWAIT` / `SKIP LOCKED` variants). The `serialization_failure`
path is `HeapTupleSelfUpdated` / `HeapTupleUpdated` returned up to
`ExecUpdate`, which maps to SQLSTATE `40001`. Our fail-fast is PG's
`NOWAIT`; our `serialization_failure` is PG's `40001`. The blocking variant
is a stretch goal precisely because it adds lock management, deadlock
detection, and wait queues — none of which change the semantics.
