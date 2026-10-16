# Benchmarks

Real measurements of BasalDB, produced by [`cmd/basal-bench`](cmd/basal-bench/main.go).
The goal is to make BasalDB scale for trace and agent-memory workloads.
Progress is tracked in the [scalability roadmap (#42)](https://github.com/Davidjustice28/basaldb/issues/42).

## Running it

```console
$ go run ./cmd/basal-bench -accounts 1000 -teams 10 -users 20 -file /tmp/bench.bsl
```

The benchmark creates `Accounts(name,plan,region) -> Teams(name) ->
Users(name,email,role,age)` with the given fan-out. It loads one
transaction per account, then runs reads, writes, deletes and a branch
through BQL in-process.

## Baseline: 2026-10-14

Apple M3 Pro (12 cores, 18 GB), Go 1.27, commit `6d9e518`. Fan-out: 10
teams per account and 20 users per team; the 5.6k run used 5 and 10.

### Size, memory, load

| | 5.6k records | 211k records | 1.05M records |
|---|---|---|---|
| Bulk load | 31 ms (179k rec/s) | 2.6 s (81k rec/s) | 1 m 48 s (9.8k rec/s) |
| File size | 0.7 MB | 26.7 MB | 136.7 MB |
| Bytes per record on disk | 119 | 126 | 130 |
| Heap after load | 2 MB | 63 MB | 332 MB |
| Peak RSS | | 316 MB | 1.8 GB |
| Cold open | 3 ms | 81 ms | 483 ms |

### Reads (BQL, in-process, p50)

| Query | 5.6k | 211k | 1.05M |
|---|---|---|---|
| `GET Accounts[id=N] -> Teams -> Users` (subtree) | 11 µs | 132 µs | 678 µs |
| `GET Accounts[id=N]` | 11 µs | 105 µs | 650 µs |
| `GET Users[id=N]` | 0.8 ms | 43 ms | 308 ms |
| `GET Users[email=X]` | 0.7 ms | 44 ms | 307 ms |
| `GET Users[age=(gt=60),role=admin]` | 0.8 ms | 50 ms | 349 ms |
| `GET Users[*][limit=10,sort=desc]` | 1.3 ms | 80 ms | 491 ms |
| Parallel subtree reads, 12 goroutines | 223k q/s | 54k q/s | 6.7k q/s |

### Writes (BQL, one commit each, in-process, p50)

| Operation | 5.6k | 211k | 1.05M |
|---|---|---|---|
| `CREATE Users PARENT Teams[id=N]` | 0.1 ms | 5 ms | 63 ms |
| `UPDATE Users[id=N][...]` | 0.8 ms | 42 ms | 325 ms |
| `DELETE Accounts[id=N]` (211 records) | 1.7 ms | 196 ms | 1.5 s |
| `DELETE Teams[id=N]` (~21 records) | 0.4 ms | 24 ms | 226 ms |
| CREATE with fsync | | 8 ms | 68 ms |
| Journal bytes per write | 220 | 227 | 229 |

### Branches

| | 211k | 1.05M |
|---|---|---|
| Branch create | 15 µs | 31 µs |
| File size, before → after branch + 1 update | 26.7 → 43.9 MB | 136.7 → 224.7 MB |

### CLI (`basal query`, 211k-record file)

Every call takes **250–450 ms**, including reads, because each invocation
loads the whole file and rewrites it on exit (#32). For comparison,
SQLite with the same data (indexed foreign keys, separate `sqlite3`
process per call) takes 10–23 ms per call, loads in 177 ms and uses 11 MB
on disk.

## Update: 2026-10-14 — #32 and #35 fixed

Same benchmark, Apple M1 (8 cores), Go 1.21.3: old binary built at
`f19c089` vs the fixed tree, measured back-to-back on the same machine,
fan-out 10 x 20 (211k records):

| | old (`f19c089`) | fixed | |
|---|---|---|---|
| `DELETE Accounts[id=N]` (211 records) | 396 ms | 9.2 ms | 43x |
| `DELETE Teams[id=N]` (~21 records) | 49 ms | 11 ms | 4.6x |
| `basal query 'SHOW LAYERS'` (CLI, read-only) | 460-630 ms | ~200 ms | 2.5x |
| CLI read-only call rewrites the file | yes (new inode every call) | no (inode stable) | #32 |
| Bulk load | 45.9k rec/s | 39.5k rec/s | -14% |

- **#35:** cascade delete now drops id-index entries directly from the
  breadth-first walk (O(subtree)) instead of rescanning the layer once
  per deleted record. The remaining ~10 ms is commit-path cost (#34),
  not the cascade.
- **#32:** a clean close no longer checkpoints, so read-only CLI calls
  leave the file byte-for-byte untouched (same inode, no rename). Writes
  still append to the journal; once it passes 8 MiB the next commit
  folds it into the page region, keeping replay on open bounded. The
  bulk-load dip is that periodic checkpointing; ingest throughput is
  tracked in #39.
- The CLI's remaining ~200 ms is loading the whole file into RAM per
  call (#37), not rewriting it.

## Update: 2026-10-14 — #33 and #34 fixed (Phase 1 of #42)

**#34 (copy-on-write overlay layers):** `Begin` no longer clones every
layer's page and id-index maps. A transaction gets an empty overlay that
records only what it changes; commits publish shallow overlays and
flatten them only once they are deep or dense, so the copy work is
amortized O(1) per commit instead of O(total records).

**#33 (indexed point lookups):** BQL resolves `[id=N]`, `[slot=N]` and
`[parent=N]` through the engine indexes instead of scanning and sorting
the whole layer, and `Records` walks pages in slot order instead of
collecting and sorting every record.

Apple M1 (8 cores), Go 1.21.3: old binary at `31d8473` vs fixed tree,
measured back-to-back on the same machine, fan-out 10 x 20:

| 211k records | old (`31d8473`) | fixed | |
|---|---|---|---|
| bulk load | 42.6k rec/s | 146k rec/s | 3.4x |
| `GET Users[id=N]` | 46.8 ms | 2 µs | ~23,000x |
| `GET Accounts[id=N]` | 112 µs | 1 µs | 112x |
| subtree walk | 145 µs | 32 µs | 4.5x |
| parallel subtree reads (8 goroutines) | 30.6k q/s | 88.4k q/s | 2.9x |
| single `CREATE` | 10.7 ms | 18 µs | ~590x |
| `UPDATE [id=N]` | 52 ms | 14 µs | ~3,700x |
| delete 211-record subtree | 10.1 ms | 670 µs | 15x |
| `GET Users[email=X]` (unindexed scan) | 53.2 ms | 12.8 ms | 4.1x |
| cold open | 107 ms | 102 ms | unchanged (#37) |
| heap after load | 68 MB | 73 MB | +7% |

At 1.05M records, fixed tree vs the baseline table above (M3 numbers;
this machine is an M1, so treat cross-machine rows as approximate):

| 1.05M records | baseline | fixed | |
|---|---|---|---|
| bulk load | 9.8k rec/s (1 m 48 s) | 73.9k rec/s (14.3 s) | 7.5x |
| `GET Users[id=N]` | 308 ms | 2 µs | |
| single `CREATE` | 63 ms | 25 µs p50 (318 µs p99) | |
| `UPDATE [id=N]` | 325 ms | 19 µs | |
| delete 211-record subtree | 1.5 s | 1.0 ms | 1,500x |
| subtree walk | 678 µs | 36 µs | 19x |
| `GET Users[email=X]` (unindexed scan) | 307 ms | 60.9 ms | 5x |
| cold open | 483 ms | 653 ms | still O(file size) (#37) |

This meets the #33 target (`[id=N]` under 50 µs at 1M+) and the #34
target direction (25 µs p50 single-record commits at 1M; the 50 µs
@ 10M claim is untested). Still open from Phase 1 of #42: #4
(cross-process file lock) and #41 (benchmark in CI). Next scaling
steps: secondary/ordered indexes for attribute scans (#33 part 3),
page-level lazy loading for open time and RAM (#37), and batched
ingest (#39).

## Update: 2026-10-16 — #37 lazy pages, indexes persisted

Working tree on `f9f8f0c` (not committed). A checkpoint writes the id
index, and any secondary indexes, into an index region. Open reads that
region and does not decode page bodies. Pages load on demand into a
64 MB LRU (`-max-memory`). The stock bench schema is
`Accounts(name,plan,region), Teams(name), Users(name,email,role,age)`
with no secondary indexes, so `email` and `age` are still scans.

Apple M3 Pro (12 cores, 18 GB), Go 1.27.2, `/usr/bin/time -l`. Same
fan-out as the baseline table (5.6k is 100×5×10).

### Size, memory, load

| | 5.6k records | 211k records | 1.05M records |
|---|---|---|---|
| Bulk load | 24 ms (233k rec/s) | 868 ms (243k rec/s) | 9.9 s (106k rec/s) |
| File after checkpoint | 0.8 MB (147 B/rec) | 32.5 MB (154 B/rec) | 166.0 MB (157 B/rec) |
| Heap after load | 5 MB | 29 MB | 120 MB |
| Peak RSS | 22 MB | 202 MB | 922 MB |
| Cold open | <1 ms | 12 ms | 78 ms |

Heap-after-load is measured before checkpoint, while ingest still holds
every page. Peak RSS is that ingest. At 1.05M versus the 2026-10-14 M3
baseline: cold open 483 ms → 78 ms, heap after load 332 MB → 120 MB,
peak RSS 1.8 GB → 922 MB. Open time still grows with record count
because the id index stays resident. A 10 GB file was not measured.

### Reads (BQL, in-process, p50)

| Query | 5.6k | 211k | 1.05M |
|---|---|---|---|
| `GET Accounts[id=N]` | 1 µs | 1 µs | 1 µs |
| `GET Users[id=N]` | 1 µs | 232 µs | 241 µs |
| `GET Users[email=X]` (no index) | 194 µs | 7.4 ms | 280 ms |
| subtree, 200 rows | 6 µs | 22 µs | 47 µs |
| `GET Users[age=(gt=60),role=admin]` | 394 µs | 15.0 ms | 315 ms |
| `GET Users[*][limit=10,sort=desc]` | 2 µs | 15 µs | 91 µs |
| Parallel subtree reads, 12 goroutines | 67k q/s | 21k q/s | 14k q/s |

`GET Users[id=N]` stays near 240 µs from 211k to 1.05M. After a lazy
open that is one page load per random id. It is not a layer scan: the
unindexed email scan is 7.4 ms at 211k and 280 ms at 1.05M. At 5.6k the
file fits in the page cache, so the same lookup stays at 1 µs. The
1.05M email scan is slower than the old in-memory 60.9 ms because the
166 MB file does not fit in the 64 MB cache, so each scan reads pages
again. The harness still prints `cold open (load whole file)` and
`GET Users[id=N] (scans whole layer)`; those labels predate this change.

### Writes (BQL, one commit each, in-process, p50)

| Operation | 5.6k | 211k | 1.05M |
|---|---|---|---|
| `CREATE Users PARENT Teams[id=N]` | 13 µs | 10 µs | 12 µs |
| `UPDATE Users[id=N][role=X]` | 8 µs | 9 µs | 14 µs |
| `DELETE Accounts[id=N]` | 178 µs (63 records) | 351 µs (211 records) | 571 µs (211 records) |
| `DELETE Teams[id=N]` | 35 µs | 59 µs | 57 µs |
| CREATE with fsync | 3.0 ms | 3.0 ms | 1.8 ms |
| Journal bytes per write | 232 | 239 | 241 |

### Branches

| | 5.6k | 211k | 1.05M |
|---|---|---|---|
| Branch create | 33 µs | 23 µs | 19 µs |
| Heap with 2 branches | 1 MB | 20 MB | 114 MB |
| File size, before → after branch + 1 update | 0.9 → 1.5 MB | 32.6 → 55.6 MB | 166.0 → 283.2 MB |

Branch page duplication (#36) is unchanged. These runs do not declare
`email!`, so they do not measure secondary indexes (#45).

## What the numbers say

- **Subtree walks are the strong point**, but their cost still grows
  with database size because each GET sorts the first layer (#33).
- **Almost everything else scales with total DB size**, not with the
  work requested:
  - each transaction copies every layer's index (#34, fixed
    2026-10-14: copy-on-write overlays, amortized O(1) per commit)
  - lookups never use the id index (#33, fixed 2026-10-14: indexed
    point lookups for id/slot/parent)
  - cascade delete rescans the index once per deleted record (#35,
    fixed 2026-10-14: O(subtree), see update above)
  - every CLI call rewrites the whole file (#32, fixed 2026-10-14: a
    clean close no longer checkpoints)
- **Memory and storage:** the 2026-10-16 table is the lazy-load
  measurement. A checkpointed file opens from its index region (id
  index and secondary indexes resident, page bodies cached up to
  `-max-memory`, default 64MB). Cold open at 1.05M is 78 ms and still
  grows with the id index. Each branch still adds a near-full copy of
  its pages on disk (#36). Ingest peak RSS is still the in-memory load
  that checkpoint later releases.

**Today:** a single writer, through `basald` or the Go packages, handles
about 1M records with ~10 µs creates and indexed id lookups that fault
one page (~240 µs when that page is not cached). Unindexed attribute
scans still read the layer, and they re-read it when the file is larger
than the page cache. See
[#42](https://github.com/Davidjustice28/basaldb/issues/42) for the plan
and targets (10M+ records, ≥100k records/s ingest, <50 µs indexed
lookups, bounded RAM).
