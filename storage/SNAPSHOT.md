# Milestone 3 — Real transactions and real snapshots

A snapshot is `{xmin, xmax, active[]}`, not a copy. This note explains why
that is enough, why the active list has to exist, and how the same idea
looks in InnoDB and TiDB.

## What a snapshot is

```go
type Snapshot struct {
    Xmin   uint64   // smallest txid still in flight when taken
    Xmax   uint64   // next txid to be handed out — the "now"
    Active []uint64 // txids in flight at snapshot time, sorted
}
```

Three integers and a short list. Taking one copies no rows, touches no
files, and costs the same whether the heap holds ten versions or ten
million. `TestSnapshotIsO1` begins 100 transactions over a 100k-row heap and
asserts the average begin latency stays flat — the snapshot is O(1) because
there is nothing to copy.

`POST /begin` allocates the txid and captures the snapshot **atomically**
(under one lock), so the active list is exact: no transaction can begin or
end in between. Every read carrying `?tx=N` reuses that one snapshot for the
transaction's whole life.

## Why repeatable read falls out

Milestone 2's reads took a fresh trivial snapshot per request, so a commit
between two reads showed up in the second one — non-repeatable read, i.e.
read-committed behavior. Pinning one snapshot per transaction removes the
movement: the boundary `{xmin, xmax, active[]}` is frozen at begin, and the
visibility function is pure, so the same read against the same snapshot
returns the same version forever. `TestRepeatableRead` and
`TestRepeatableReadOverHTTP` demonstrate it: A reads k, B updates k and
commits, A reads k again and gets the same version *pointer* back.

Nothing was locked, copied, or negotiated. Repeatable read is a consequence
of "the snapshot doesn't move".

## Why the active list has to exist

The boundary alone (`xmin`, `xmax`) cannot answer one question: **was this
transaction in flight when I took my snapshot?**

txid order is not commit order. A transaction can hold a low txid and commit
*after* a higher-numbered one:

```text
tx5 begins ───────────────────── commits at t=10
tx6 begins ── commits at t=5
snapshot taken at t=7: xmax = 7
```

tx5's versions have `xmin = 5 < 7`, so the boundary check passes — but tx5
was in flight at snapshot time and must stay invisible to this snapshot
*even though it has since committed*. The active list is the only place that
fact is recorded. Without it, "committed before or after my snapshot" is
unknowable for txids below xmax.

This is also why `xmin` exists: the active list only needs to cover
`[xmin, xmax)`. Anything older had already ended; anything newer did not
exist. `inFlight` short-circuits outside that range without a search.

## InnoDB's ReadView is the same object

InnoDB calls this a **ReadView**: `low_limit_id` (our `xmax`), `up_limit_id`
(our `xmin`), and `trx_ids` (our `active`). Same three fields, same purpose.
The differences are physical: InnoDB's visibility check walks the **undo
chain** (newest record in the clustered index, older versions reached via
`DB_ROLL_PTR`) until it finds a version the ReadView accepts, whereas we
keep every version in the heap and check them directly. REPEATABLE READ in
InnoDB takes the ReadView at the transaction's first read; READ COMMITTED
takes a fresh one per statement — exactly our `?tx=` vs. no-`?tx=` split.

## TiDB/TiKV get away with one integer

TiKV's timestamps come from a centralized TSO (timestamp oracle), so
allocation order **is** commit order: a snapshot is a single `ts`, and
"visible" means "latest version with `commit_ts ≤ ts`". There is no active
list because there is no ambiguity — a transaction with a smaller timestamp
can never commit after one with a larger timestamp. The price is the TSO
itself: a global, highly-available counter service. Our txid counter is
local and cheap, and the active list is the bookkeeping fee for not having
a TSO.

## What milestone 3 does *not* give you

- **Writes.** Transactions can read, but there is no `/update` yet — that is
  milestone 4, with first-writer-wins on the latest version.
- **Read-your-own-writes.** A transaction's own uncommitted versions are
  invisible to its own snapshot (clause (1) of `visible`: `xmin` must be
  committed). Milestone 4 adds the per-transaction write set that fixes
  this.
- **Phantoms are already impossible** within one snapshot — the snapshot
  cannot move, so a repeated scan returns the same set. The anomaly suite
  (milestone 5) proves it rather than asserts it.
