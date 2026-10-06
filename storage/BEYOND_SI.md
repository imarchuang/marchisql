# Beyond snapshot isolation

Write skew is legal under snapshot isolation. Two transactions each read a
predicate, each write a different row that the predicate depends on, and
both commit. First-writer-wins never fires, because it only looks at the
row being written. `storage/WRITE_SKEW.md` reproduces this with the doctors
on call.

There are three ways out. This milestone builds the first one. The other
two are described so the cost of each is visible before anyone adds it.

## 1. Materialize the conflict (`SELECT … FOR UPDATE`)

Lock the rows the predicate returned. The second transaction then conflicts
on a real row, the same way two updates of one key already do.

```text
A: GET /scan?tx=A&where=on_call=true&for_update=true   → locks alice, bob
B: GET /scan?tx=B&where=on_call=true&for_update=true   → 409 transaction_conflict
B aborts
A writes alice on_call=false and commits
scan where on_call=true → [bob]
```

`for_update` requires `?tx=`. The scan runs without the engine lock. Each
returned key is then locked under `e.mu`:

1. If another in-progress transaction holds the lock → `transaction_conflict`.
2. Otherwise first-writer-wins on the latest version not written by me
   (`checkWritable`). A commit that landed between the scan and the lock is
   `serialization_failure`.
3. Record `locks[key] = me`.

`Update` and `Delete` take the same lock, so a plain write of a locked row
also fails while the holder is in progress. Commit and abort release every
lock the transaction holds. Locks are memory only: a restart aborts
in-progress transactions, and there is nothing to recover.

If a later key in the same scan conflicts, the keys already locked stay
locked until commit or abort.

This is fail-fast, matching milestone 4. PostgreSQL would block, then
re-read the row (EvalPlanQual) under the lock. We do not. A transaction
that swallows the error, waits for the holder to commit, and then writes a
*different* row from its original snapshot can still skew.
`TestForUpdateFailFastDoesNotRecheck` pins that down. The contract is:
on conflict, abort and `POST /begin` again.

What this does not lock: the gap. `FOR UPDATE` locks the rows the predicate
returned, not "any future row that would match". A concurrent insert of a
new on-call doctor is still a phantom. Predicate locks are a different
mechanism and are not in this slice.

## 2. Serializable snapshot isolation

SSI tracks read-write antidependencies instead of asking the caller to name
the rows. A read of a version is an SIREAD mark. A later write of that key
by a concurrent transaction is an edge from the reader to the writer. The
dangerous shape is two such edges in a row:

```text
T1 --rw--> T2 --rw--> T3
```

with T3 committing first (T2 is the pivot). Abort one of them. A single
edge is harmless.

The marks can live in memory next to the write set. They are not versions
and do not belong in a segment. The subtlety is that the pivot can look
safe until the third transaction commits, so the check runs at commit, not
at the original read. A crude version that aborts on any rw-antidependency
is easier and rejects legal histories; PostgreSQL's version exists to abort
less. Neither is implemented here.

`FOR UPDATE` is not a substitute. A reader who forgets a key the predicate
depends on is back in write skew.

## 3. Change the shape

Store "number of doctors on call" in one row. Both transactions update that
row, and first-writer-wins fires. The anomaly disappears because the
predicate became a key. No lock table, no dependency graph. The cost is
that the application has to maintain the aggregate, and every decision that
used to be a scan is now a write to the same hot row.

## Which one

| Door | Pays for | Use it when |
|---|---|---|
| `FOR UPDATE` | The caller knows the rows the predicate depends on | The conflict set is small and explicit, as in the doctors scan |
| SSI | Any transaction, including ones that forget a key | You want serializability without changing the queries |
| One row | Nothing in the engine | The invariant already has a natural counter or flag |

Visibility, the CLOG, and GC do not change for any of the three. A locked
read is still a snapshot read. Abort still means the CLOG says aborted and
the versions stay until GC.
