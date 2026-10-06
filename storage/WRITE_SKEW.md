# Milestone 5 — Write skew

Snapshot isolation prevents dirty reads, non-repeatable reads, phantoms,
and same-row lost updates. It does not prevent write skew. This note draws
the dependency cycle the engine cannot see, and points at the tests that
reproduce it.

## What the suite shows

| Scenario | Under SI | Where |
|---|---|---|
| Dirty read | prevented | `TestAnomalyDirtyReadPrevented` |
| Non-repeatable read | prevented | `TestAnomalyNonRepeatableReadPrevented` |
| Phantom (`on_call=true` grows) | prevented | `TestAnomalyPhantomPrevented` |
| Same-row lost update | prevented | `TestAnomalyLostUpdatePrevented` |
| Doctors on call | **reproduced** | `TestAnomalyWriteSkewDoctors`, `cmd/demo/doctors.sh` |
| Two-account balance sum ≥ 0 | **reproduced** | `TestAnomalyWriteSkewBalance` |

The prevented cases are the same mechanisms as milestones 2–4, restated as
anomalies so the contrast with write skew is in one file.

## The cycle

```text
A reads {alice, bob}  →  B writes bob
B reads {alice, bob}  →  A writes alice
```

A read of a row followed by the other transaction's write of that row is a
**rw-antidependency**. The two edges form a cycle, so no serial order
matches the execution: A-then-B would have A seeing bob already off call,
B-then-A would have B seeing alice already off call. The execution is not
serializable.

SI never notices. First-writer-wins only fires when two transactions write
the **same** key. Alice and bob are different keys, so each `xmax` is
uncontested, both commits succeed, and the predicate "at least one doctor
is on call" — true in both snapshots — is false afterwards.

That is the asymmetry from milestone 4, harvested: reads used the snapshot,
writes used the latest version of *their own* key, and the predicate that
connects the two keys was never a row.

## The balance variant

Same cycle, different costume. Accounts `a` and `b` start at 100. The
application constraint is `balance(a) + balance(b) >= 0`. Each transaction
reads the sum (200), decides a withdrawal of 200 is legal, and debits a
different account. Both commit. The sum is -200.

The engine has no idea there was a constraint. The constraint lived in the
application, evaluated against a snapshot, and the snapshot was allowed to
be stale with respect to the other writer's key.

## Reproducing it by hand

`cmd/demo/doctors.sh` is the README quick start, including the seed the
README assumes. It starts a server, puts Alice and Bob on call, runs the
two overlapping transactions, and exits non-zero unless the final
`on_call=true` scan is empty.

```text
A reads {alice, bob} → B reads {alice, bob}
B writes bob=false, commits
A writes alice=false, commits
scan where on_call=true → []
```

## Three doors out

Milestone 8 sketches these; none of them is implemented here.

1. **Materialize the conflict.** `SELECT ... FOR UPDATE` on the rows the
   predicate depends on, so the second transaction blocks or aborts on a
   real row lock. The doctors scenario stops skewing because bob's row (or
   the whole on-call set) is locked by the first reader.
2. **Serializable snapshot isolation.** Track the rw-antidependencies and
   abort one side when they form a dangerous cycle. PostgreSQL's SSI does
   this; even a crude over-aborting version teaches why the bookkeeping is
   subtle (the cycle may only become dangerous at commit).
3. **Change the shape.** Store "number of doctors on call" in one row.
   Both transactions then write that row, and first-writer-wins fires.
   The anomaly disappears because the predicate became a key.
