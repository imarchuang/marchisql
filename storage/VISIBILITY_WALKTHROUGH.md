# A complete snapshot check, step by step

One concrete scenario, from `BEGIN` to the final `visible()` verdict on every
version. Nothing here is implemented yet — this is the worked example that
milestone 2's code and tests must reproduce exactly.

## The world

Seven transactions have been begun over time. The in-memory CLOG currently
says:

```text
txid  status
────  ───────────
1     committed
2     committed
3     committed
4     aborted
5     in_progress     ← still running
6     in_progress     ← still running
```

The heap contains these versions (append order):

```text
#  key    fields        xmin  xmax
─  ─────  ────────────  ────  ────
1  alice  {v: 1}        1     2      ← tx1 inserted, tx2 updated
2  alice  {v: 2}        2     0      ← tx2's version, alive
3  bob    {v: 1}        3     5      ← tx3 inserted, tx5 is deleting
4  carol  {v: 1}        4     0      ← tx4 inserted, then aborted
5  dave   {v: 1}        6     0      ← tx6 inserted, still running
6  eve    {v: 1}        5     0      ← tx5 inserted, still running
```

## Taking the snapshot

`BEGIN` allocates tx7 and captures:

```go
snap := Snapshot{
    xmin:   5,        // smallest txid still in flight
    xmax:   7,        // next txid to be handed out
    active: []{5, 6}, // in flight right now
}
```

Three integers and a two-element list. Nothing is copied. This is the entire
"snapshot".

## The visibility function

```go
func visible(v *Version, snap Snapshot, clog *Clog) bool {
    // Born?
    if clog.Status(v.Xmin) != StatusCommitted { return false }
    if v.Xmin >= snap.Xmax                  { return false }
    if snap.inFlight(v.Xmin)                { return false }

    // Still alive?
    if v.Xmax == 0 { return true }
    switch clog.Status(v.Xmax) {
    case StatusAborted:
        return true // the killer failed; the version lives on
    case StatusInProgress:
        return true // the kill is not committed yet
    }
    // The killer committed — but did my snapshot see the kill?
    if snap.inFlight(v.Xmax) { return true }
    if v.Xmax >= snap.Xmax   { return true }
    return false
}
```

Each check is one map lookup or one integer comparison. No locks, no copies.

## Walking every version

### #1 alice `{xmin:1, xmax:2}` — superseded version

| step | check | result |
|---|---|---|
| born | `clog[1]` → committed ✓; `1 < 7` ✓; `1 ∉ active` ✓ | born |
| alive | `xmax = 2 ≠ 0`; `clog[2]` → committed (not aborted); `2 ∉ active`; `2 < 7` | **killed before my snapshot** |

→ **invisible**. Two map lookups: `clog[1]`, `clog[2]`.

### #2 alice `{xmin:2, xmax:0}` — the visible one

| step | check | result |
|---|---|---|
| born | `clog[2]` → committed ✓; `2 < 7` ✓; `2 ∉ active` ✓ | born |
| alive | `xmax == 0` — nobody has touched it | alive |

→ **visible**. One map lookup: `clog[2]`. `xmax == 0` skips the second
lookup entirely.

### #3 bob `{xmin:3, xmax:5}` — being deleted by an in-flight tx

| step | check | result |
|---|---|---|
| born | `clog[3]` → committed ✓; `3 < 7` ✓; `3 ∉ active` ✓ | born |
| alive | `xmax = 5`; `clog[5]` → in_progress | **the kill isn't committed** |

→ **visible**. tx5 is deleting bob, but from my snapshot that delete hasn't
happened. I still see bob. Two map lookups: `clog[3]`, `clog[5]`.

If tx5 later commits, *new* snapshots stop seeing bob. This snapshot never
changes its answer — that is repeatable read.

### #4 carol `{xmin:4, xmax:0}` — written by an aborted tx

| step | check | result |
|---|---|---|
| born | `clog[4]` → **aborted** | never born |

→ **invisible**. One map lookup. The version sits on disk forever (until GC)
and is invisible to everyone — abort needs no cleanup.

### #5 dave `{xmin:6, xmax:0}` — uncommitted insert

| step | check | result |
|---|---|---|
| born | `clog[6]` → **in_progress** | not born yet |

→ **invisible**. One map lookup. This is the dirty-read prevention clause:
uncommitted data is invisible, full stop.

Even if tx6 commits *after* my snapshot, `6 ∈ active` would still hide it
from me (the second born-check catches that case).

### #6 eve `{xmin:5, xmax:0}` — the other in-flight writer

Same shape as #5: `clog[5]` → in_progress → **invisible**, one lookup.

## Tally

| version | verdict | map lookups |
|---|---|---|
| alice old | invisible (superseded) | 2 |
| alice new | **visible** | 1 |
| bob | **visible** (delete still in flight) | 2 |
| carol | invisible (aborted) | 1 |
| dave | invisible (uncommitted) | 1 |
| eve | invisible (uncommitted) | 1 |

The scan returns `alice → {v:2}` and `bob → {v:1}`, and reports
`X-Marchisql-Versions-Scanned: 6`, `X-Marchisql-Versions-Skipped-Invisible: 4`.

Total work: 8 map lookups and a dozen integer compares for 6 versions. The
snapshot itself was three integers. That is the whole mechanism.
