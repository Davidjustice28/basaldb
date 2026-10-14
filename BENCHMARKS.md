# Benchmarks

Real measurements of BasalDB, produced by [`cmd/basal-bench`](cmd/basal-bench/main.go).
The goal is to make BasalDB scale for trace and agent-memory workloads.
Progress is tracked in the [scalability roadmap (#42)](https://github.com/Davidjustice28/basildb/issues/42).

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

## What the numbers say

- **Subtree walks are the strong point**, but their cost still grows
  with database size because each GET sorts the first layer (#33).
- **Almost everything else scales with total DB size**, not with the
  work requested:
  - each transaction copies every layer's index (#34)
  - lookups never use the id index (#33)
  - cascade delete rescans the index once per deleted record (#35)
  - every CLI call rewrites the whole file (#32)
- **Memory and storage:** the whole database lives in RAM (#37), and
  each branch adds a near-full copy on disk (#36).

**Today:** good for internal testing up to about 50k records per file,
with a single writer, ideally through `basald` or the Go packages
rather than the CLI. See [#42](https://github.com/Davidjustice28/basildb/issues/42)
for the plan and targets (10M+ records, ≥100k records/s ingest, <50 µs
indexed lookups, bounded RAM).
