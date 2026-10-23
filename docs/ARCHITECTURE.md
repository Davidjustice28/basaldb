# BasalDB architecture

A short map of how BasalDB stores and serves data. For a short
introduction see the [README](../README.md); for capabilities, the file
format and agent walkthroughs see [GUIDE.md](../GUIDE.md); for the query
language see [CLI.md](../CLI.md).

## The data model: a matrix of layers

BasalDB calls itself a matrix-hierarchical database. Every record has a
coordinate `<Y, X, Z>`:

| Axis | Meaning | In the code |
|---|---|---|
| **Y** | layer height: the level of the hierarchy (`0:Users`, `1:Topics`, `2:Facts`) | `Layer.Height` |
| **X** | slot: the record's position within its layer, assigned in insertion order | `Record.Slot` |
| **Z** | attribute: positional index into the record's values, optionally named | `Record.Attrs`, `Layer.Attrs` |

On top of that, each record has a file-wide unique `ID`, a `Parent` id in
the layer above (0 for roots), and the ids of its `Children` in the layer
below. Every child-layer record must have a parent, so a cascade delete
always reaches it. `Users[6#2]` in CLI output means *layer Users, id 6,
slot 2*.

## The `.bsl` file

One file holds everything, in four regions (`pkg/engine/header.go`):

```
[0, 4096)                        ASCII header: Magic:BSL, version, layers, branches, region offsets
[CommitLogStart, CommitLogEnd)   human-readable commit log (author, summary, reason, ops)
[DataStart, DataEnd)             binary slotted pages, per branch and layer
[IndexStart, IndexEnd)           page directory, id index, secondary hash indexes
[JournalStart, EOF)              append-only binary journal of commits since the last checkpoint
```

- **Writes** append a framed record to the journal (`journal.go`). With
  `-sync`/`SetSync(true)` each commit is fsynced.
- **Checkpoint** folds the journal into the commit log and page regions
  and rewrites the file. It runs once the journal passes 8 MiB, or on
  request. A clean close doesn't checkpoint, so read-only use leaves the
  file byte-for-byte unchanged.
- **Open** reads the header and the commit log, maps the file, reads the
  page directory out of the index region, and replays the journal. Id
  pairs stay in that mapping and are binary-searched. Page bodies stay
  on disk until a read misses the cache. Files written before the index
  region existed are scanned once (one page at a time) to rebuild the
  directory and indexes; the next checkpoint stores them.
  `-max-memory` (default 64MB) caps the decoded page cache. Secondary
  indexes are decoded and stay resident. The commit log is parsed in
  full, so a long log still costs open time and RAM.

## Slotted pages

A page (`page.go`) covers 1024 consecutive slots of one layer:
- A 1024-bit validity bitmask; deleting a record clears its bit.
- A slot directory, then encoded record tuples:
  `id | slot | parent | nChildren | children… | nAttrs | (len, bytes)…`.
- Format 1.1 stores those lengths as `uint32` (up to 4 GiB per value,
  up to 2^32−1 children). Format 1.0 used `uint16` and is still readable.
  A checkpoint rewrites a 1.0 file as 1.1. A value that does not fit is
  rejected before commit; it is never truncated.

## In memory: snapshots and copy-on-write overlays

Each branch head is an immutable `Snapshot`. Readers take a snapshot and
never block writers (MVCC). A `WriteTx` (`store.go`) starts with an
empty overlay per layer (`layerState`) on top of the snapshot. The overlay
records only the pages, id-index entries and deletions it changes, so
starting a transaction is O(1). On commit, the overlay becomes the new
head, and deep or dense overlay chains are flattened (amortized O(1) per
commit). If another commit landed on the branch first, `Commit` fails
with `ErrConflict` rather than overwriting it.

Indexes: per-layer `id → slot`, the parent/children pointers in each
record, and optional hash indexes on declared attributes. Declare one
with a trailing `!` in the layer spec (`Pages(url!,status)`) or with
`CREATE INDEX ON Pages(url)` (checkpointed immediately). BQL uses them
for `[id=N]`, `[slot=N]`, `[parent=N]`, and `[attr=value]` when `attr`
is indexed. Other filters scan the layer. A `GET` with `limit` stops
an unindexed scan once enough rows match; `sort=desc` walks slots
backward, which is descending id order within a layer. Indexed
equality collects the matching ids (ascending) and then applies the
remaining filters, sort, and limit. The hash index is copy-on-write,
like the id index, and is maintained on create, update, and cascade
delete.

`basald` holds a per-branch lock across each `/v1/execute`, so
concurrent HTTP writers on one branch queue instead of failing with
`ErrConflict`. A direct `WriteTx` on the same branch can still see
`ErrConflict` if another commit landed first. The daemon listens on
localhost unless told otherwise, and refuses a public bind until a
bearer token is configured.

## Cascade delete

`DELETE` walks the subtree breadth-first from the matched records
(`cascade.go`), clears validity bits, drops id-index entries and removes
the root from its parent's children. Cost is O(size of the subtree), not
O(size of the database).

## Branches

A branch is a named head with its own commit history, forked from another
branch at a commit (`branch.go`). In memory, branches share structure
through the overlays. On disk, a checkpoint writes an unchanged page
once and points every branch that still uses it at that offset. Each
branch still stores its own id pairs (16 bytes per live record) and
any page it has modified
([#36](https://github.com/Davidjustice28/basaldb/issues/36)).
Branches can't be merged yet ([#20](https://github.com/Davidjustice28/basaldb/issues/20)).

## Packages

| Path | Role |
|---|---|
| `pkg/engine` | file format, pages, journal, snapshots, transactions, branches, cascade |
| `pkg/bql` | BasalQL (BQL) lexer, parser and executor |
| `pkg/api` | REST handlers used by `basald` |
| `pkg/config` | `basal config` defaults |
| `cmd/basal` | CLI |
| `cmd/basald` | HTTP server |
| `cmd/basal-bench` | benchmark (see [BENCHMARKS.md](../BENCHMARKS.md)) |
| `deploy/` | Cloud Run deployment script |
