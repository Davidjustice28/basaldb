# sqlite-compare

Runs the `cmd/basal-bench` workload against SQLite so the two can be
compared on the same machine. It uses the same dataset, random seed,
queries and sample counts, and prints in the same format.

```console
$ cd bench/sqlite-compare
$ go run . -accounts 1000 -teams 10 -users 20 -file /tmp/bench.sqlite
$ go run . -accounts 5000 -teams 10 -users 20 -index email -file /tmp/bench.sqlite
```

It's a separate module so BasalDB itself stays dependency-free. It needs
cgo, because `github.com/mattn/go-sqlite3` builds the real C SQLite.

## Setup, chosen to favour SQLite

- WAL mode, foreign keys with `ON DELETE CASCADE`, and an index on every
  parent column (`teams.account_id`, `users.team_id`).
- Each statement is prepared once and reused. BQL parses every query.
- Writes without fsync use `synchronous=OFF`, matching how `basal-bench`
  appends to the journal. The fsync test uses `synchronous=FULL` with
  `fullfsync=ON`, because Go's `File.Sync` issues `F_FULLFSYNC` on macOS.
- A subtree is one indexed join that returns the users under an account.
  It returns the same rows as `GET Accounts[id=N] -> Teams -> Users`.
- SQLite has no branches. The branch test copies the database file, which
  is the closest equivalent.
- `ANALYZE` runs after the load so the planner has statistics.
- Cold open is `sql.Open` plus the first query, because SQLite opens
  lazily.

Results are in [BENCHMARKS.md](../../BENCHMARKS.md).
