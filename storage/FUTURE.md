# Future — serializability and linearizability

The engine stops at snapshot isolation. This note is what has to be added,
on top of the heap, the CLOG, and `visible`, to go further. Nothing here is
implemented. Milestone 8 is the first slice of the serializability half.

## Where the MVP stops

A transaction reads through one snapshot, `{xmin, xmax, active[]}`, taken at
`BEGIN`. It writes by confronting the latest version of the keys it changes.
First-writer-wins aborts the second writer of a single key.

That is enough to prevent dirty reads, non-repeatable reads, phantoms, and
same-row lost updates. It is not enough for either of the guarantees below.
Write skew is the witness: two transactions can each see "2 doctors on
call", update a different row, and both commit. No serial order produces an
empty on-call set from those reads. See `WRITE_SKEW.md`.

## The two words

**Serializability.** The committed transactions are equivalent to some
serial order. The order does not have to match the wall clock. If A and B
overlap, either A-then-B or B-then-A is acceptable, as long as one of them
matches the reads and writes that actually happened.

**Linearizability.** Each operation appears to take effect at a single
moment between its start and its return, and that moment respects
real time: if operation X finishes before operation Y starts, X is ordered
before Y. For a single key this is the usual meaning. For a multi-key
transaction the same requirement is called **strict serializability**:
serializable, and the serial order agrees with real time.

Serializability does not imply linearizability. A serial order can put B
before A even though A committed and returned before B started. Strict
serializability forbids that.

## What this process already gives you

One node, one CLOG, snapshots taken under the transaction-manager lock.

If A commits and returns, and only then B begins, B's snapshot sees A's
writes. `xmax` is past A's txid, A is committed, and A is not in B's active
list. Non-overlapping transactions are already in real-time order. The
holes are entirely inside the overlap:

- Serializability fails when overlapping transactions form a cycle that
  touches two or more keys (write skew).
- Linearizability of a *single* read fails only when the caller passes an
  old `?tx=`. A `GET` without a transaction takes `LatestSnapshot` at the
  start of the call. A commit that returned before that call is already in
  the heap and the CLOG, and the read locks that take those structures
  observe it. Overlapping a commit, either result is allowed.
- A multi-statement transaction that keeps its snapshot is intentionally
  not linearizable. That is the product.

So the work is not a new storage layout. The heap, `xmin`/`xmax`, and the
visibility function stay. The new code is bookkeeping at read time and a
decision at commit time.

## Serializability

SI validates the write set only: "did someone else commit a write to a key
I wrote, after my snapshot?" Serializability also has to know about keys
the transaction *read* and someone else later wrote.

### Smallest change: validate the read set at commit

Record, per transaction, the keys it read and the version it observed
(`xmin` of the visible version, or "absent"). `GetInTx` and `ScanInTx`
append to that set. Predicates matter: a scan with `where=on_call=true`
read every key that matched, and also the fact that no other visible key
matched. The read set for a predicate scan is the set of keys the snapshot
considered, not only the ones returned. Otherwise a phantom inserted into
the predicate sneaks through. Under SI the phantom is already invisible
inside the snapshot; the read-set check exists so a *later* commit cannot
form a cycle with that read.

At `CommitTx`, before the commit record:

```text
for each key in my read set:
    latest = latest version not written by me
    if latest is missing and I observed a version: abort
    if latest.xmin != the xmin I observed
       and that writer committed after my snapshot: abort
```

Doctors: A read alice and bob. B writes bob and commits. A's read set still
names bob's old xmin. Commit of A aborts. One of the two transactions
survives. The execution is serializable. It aborts more than necessary: a
read of k followed by a write of k from a transaction that committed is
always fatal, even when the dependency does not close a cycle.

This touches `read.go` (record the set), `tx.go` (drop it on commit and
abort), and `CommitTx` (the check). `checkWritable` stays as it is for the
write set. Conflicts stay `serialization_failure`.

### Fewer aborts: serializable snapshot isolation

PostgreSQL's SSI keeps the read-write edges instead of rejecting every one
of them. A read of a version is an SIREAD mark on that key. A later write
of the key by a concurrent transaction is an rw-antidependency from the
reader to the writer. The dangerous shape is two such edges in a row:

```text
T1 --rw--> T2 --rw--> T3
```

with T3 committing first (T2 is the pivot). Abort T2 or T3. A single edge
is harmless: it is just "I read, then you wrote", which still has a serial
order.

On this engine the marks can live in memory next to the write set. A
restart aborts everyone anyway, so they do not need a segment file. The
subtlety, which is the point of milestone 8, is that the pivot may look
safe until the third transaction commits. The check runs at each commit,
not at the original read.

SSI on one node, with snapshots taken at start, is strict: a transaction
that starts after another commits already sees that commit, so the serial
order SSI preserves cannot put the later transaction first. That is strict
serializability for transactions that use this API. It is still not a
linearizable read *inside* an old snapshot. The snapshot is allowed to be
stale; SSI only promises that the commits which do happen have some legal
serial order.

`SELECT ... FOR UPDATE` is not this. It turns the rows you name into write
conflicts so the doctors scenario stops skewing. It does not make an
arbitrary transaction serializable. A reader who forgets a key the
predicate depends on is back in write skew. Worth having as an explicit
lock. Not a substitute for a commit-time check.

### What does not change

Visibility, the CLOG, segment rewrite, and the horizon. A serializable
transaction is still a snapshot reader. Abort still means "the CLOG says
aborted and the versions stay until GC." The read set and the SIREAD marks
are dropped when the transaction ends; they are not versions.

## Linearizability

### One key, one operation

`GET /get?key=k` with no `?tx=` is already a linearizable read of that key
on this process. `POST /update` followed by commit linearizes at the commit
record: the CLOG write is the moment the new version becomes visible to
later latest-snapshots.

Do not add a lock around that read unless a measurement says the race is
real. A commit that has returned happens-before a later `Get`: the version
was published under the heap lock, the status under the CLOG lock, and the
read takes both.

What is missing from the API is a name for the guarantee, and a refusal to
silently weaken it. A `?tx=` on an old snapshot must not be described as
linearizable. A flag such as `GET /get?key=k&linearizable=1` that rejects
`?tx=` makes the choice visible. Internally it is `LatestSnapshot` at the
start of the handler.

A linearizable multi-key read is the same idea applied to one scan: take
one latest snapshot at the start, return, do not hold it open. `GET /scan`
without `?tx=` is that operation today.

### Transactions

A transaction that wants strict serializability cannot keep a snapshot from
`BEGIN` and also claim each of its later reads is linearizable. Those are
different contracts.

The transactional form is: run under SI, then at commit apply read-set
validation or SSI, and keep the rule that already holds here — a
transaction begun after another has committed sees that commit. The
linearization point of the transaction is its commit record, the same
CLOG append as today. Clients observe the order of those commits.

If a transaction must read the latest value of one key and still keep its
snapshot for everything else, that read is a different statement. It takes
a fresh latest snapshot for that key only, and the key joins the read set
under the new xmin. Mixing the two without putting the key in the read set
re-opens write skew.

### What distribution would add

None of the above needs a clock while there is one process. The commit
record is a total order.

Two nodes break both guarantees unless something else creates that order:

- Serializability across nodes needs a shared commit order, or a scheme
  like Spanner's TrueTime, or a serializable protocol that ships the
  read and write sets to one decider.
- Linearizability across nodes needs that same order to respect real time,
  which a pair of local txid counters does not. A timestamp oracle, a
  consensus log, or bounded clock uncertainty is the new mechanism. The
  heap format does not grow a field for it; the snapshot's `xmax` stops
  being a local counter and becomes that external time.

That work is a different system. This file stops at one process.
