# BasalDB CLI (`basal`)

`basal` is the single-file database utility: initialize `.bsl` containers,
inspect their ASCII header and audit trail, run BasalQL (BQL) statements,
and manage branches.

```
basal init    -file DATA.bsl [-layers H:Name,...] [-author A -summary S -reason R]
basal inspect -file DATA.bsl [-branch NAME] [-header] [-json]
basal query   -file DATA.bsl [-branch NAME] [-author A -summary S -reason R] 'BQL STATEMENT'
basal branch  list -file DATA.bsl
basal branch  create NAME -file DATA.bsl [-from SRC] [-author A -summary S -reason R]
```

Notes that apply everywhere:

* **`-file` is always required** — BasalDB has no default path; every
  command names the container it operates on.
* For `query`, flags may appear before *or* after the statement
  (`basal query 'GET Accounts[*]' -file demo.bsl` works).
* For `branch`, the subcommand (`list` / `create`) must come first, then
  its flags.
* Exit codes: `0` success, `1` runtime or BQL error (printed as
  `basal: <error>` to stderr), `2` usage error.

---

## init

Creates a new `.bsl` container with the given layers and writes the
initial commit.

| Flag | Default | Meaning |
|---|---|---|
| `-file` | *(required)* | path of the container to create |
| `-layers` | `Accounts,Teams,Users` | layer definitions, comma separated |
| `-author` | `basal <basal@localhost>` | audit author of the init commit |
| `-summary` | `Database initialized` | audit summary |
| `-reason` | `manual initialization` | audit reason |

A layer entry is either `Name` (height assigned by position, starting at
0) or `H:Name` (explicit height on the Y-axis).

```console
$ basal init -file demo.bsl -layers 0:Accounts,1:Teams,2:Users
initialized demo.bsl
layers: 0:Accounts,1:Teams,2:Users
initial commit: 1 (basal <basal@localhost> | Database initialized | manual initialization)
```

---

## inspect

Prints the container's header, region map, branches, and commit trail.

| Flag | Default | Meaning |
|---|---|---|
| `-file` | *(required)* | path of the container |
| `-branch` | `main` | branch whose head to show |
| `-header` | off | print the raw ASCII header block instead |
| `-json` | off | print machine-readable JSON |

```console
$ basal inspect -file demo.bsl
file:    demo.bsl
version: 1.0
format:  MatrixSlottedPage
regions: commit log [4096,4291), data [4291,4291), journal [4291,...)
branch:  main @ commit 1
layers:
  0:Accounts     0 record(s)
  1:Teams        0 record(s)
  2:Users        0 record(s)
branches:
  main (fork main@0, head 1)
commits:
  [0001] main | basal <basal@localhost> | Database initialized | manual initialization
```

`-header` dumps the human-readable signature block (also useful for
debugging corrupted files):

```console
$ basal inspect -file demo.bsl -header
Magic:BSL
Version:1.0
Format:MatrixSlottedPage
Layers:[0:Accounts, 1:Teams, 2:Users]
Branches:main,scratch
CommitLogStart:4096
CommitLogEnd:5096
DataStart:5096
DataEnd:6403
JournalStart:6403
```

---

## query

Runs exactly **one** BQL statement against a snapshot of the container.

| Flag | Default | Meaning |
|---|---|---|
| `-file` | *(required)* | path of the container |
| `-branch` | `main` | branch to run against |
| `-author` | *(empty)* | audit author — **required for writes** |
| `-summary` | *(empty)* | audit summary — **required for writes** |
| `-reason` | *(empty)* | audit reason — **required for writes** |
| `-json` | off | print the result as JSON |

Every write (`CREATE`, `UPDATE`, `DELETE`, `BRANCH CREATE`) appends an
immutable commit block carrying the mandatory `author` / `summary` /
`reason` metadata. Reads (`GET`, `SHOW`) need no metadata. A write
without it fails:

```console
$ basal query -file demo.bsl 'CREATE Accounts ("No Meta")'
basal: engine: Author is required
```

### BQL syntax

Keywords are case-insensitive (`show records users` works). Attributes
are quoted strings (double or single quotes; escapes `\n`, `\t`, `\\`,
`\"`, `\'`).

#### Selectors

Every `Layer[...]` reference carries a selector:

| Selector | Meaning |
|---|---|
| `[N]` | record with ID `N` |
| `[#N]` | record in X-axis slot `N` |
| `[*]` | every record in the layer |

`[*]` is allowed in `GET` and `UPDATE`; `DELETE` requires a single
record because a cascade must root at exactly one node.

#### GET — path traversal

```
GET Layer[sel] (-> Layer[sel])*
```

Walks parent→child edges down the matrix with zero joins. Each step must
name the layer at exactly one height below the previous one:

```console
$ basal query -file demo.bsl 'GET Accounts[1] -> Teams -> Users'
layer | id | slot | parent | children | attrs
Users[3#0] parent=2 children=[] "Sam" "sam@dev.io"
1 record(s) on branch "main"
```

Skipping a layer is an error:

```console
$ basal query -file demo.bsl 'GET Accounts[1] -> Users'
basal: bql: path step "Users" must be the child layer of height 0 (got height 2)
```

#### CREATE — root records and children

```
CREATE Layer ("attr", "attr", ...)
CREATE Layer PARENT Layer[sel] ("attr", "attr", ...)
```

Without `PARENT` the record roots its own subtree; with it the parent
must select exactly one existing record:

```console
$ basal query -file demo.bsl \
    -author "Alex Rivera <alex@dev.io>" -summary "seed tenants" -reason "demo data" \
    'CREATE Accounts ("Acme Corp", "2026-01-01")'
layer | id | slot | parent | children | attrs
Accounts[1#0] parent=0 children=[] "Acme Corp" "2026-01-01"
created Accounts#0 (id=1)
commit 2 on main | Alex Rivera <alex@dev.io> | seed tenants | demo data

$ basal query -file demo.bsl \
    -author "Alex Rivera <alex@dev.io>" -summary "seed tenants" -reason "demo data" \
    'CREATE Teams PARENT Accounts[1] ("Eng Team")'
created Teams#0 (id=2)
```

The engine assigns IDs and slots; the CLI output shows both
(`Layer[id#slot]`).

#### UPDATE — replace attributes

```
UPDATE Layer[sel] ("attr", "attr", ...)
```

Replaces the full attribute list of every selected record in one
commit. Zero matches is an error, never a silent no-op:

```console
$ basal query -file demo.bsl \
    -author "Sam <sam@dev.io>" -summary "rename" -reason "demo" \
    'UPDATE Users[#0] ("Sam Rivera", "sam@dev.io")'
layer | id | slot | parent | children | attrs
Users[3#0] parent=2 children=[] "Sam Rivera" "sam@dev.io"
updated 1 record(s) in "Users"

$ basal query -file demo.bsl \
    -author A -summary S -reason R 'UPDATE Users[99] ("Ghost")'
basal: bql: no records in "Users" matched Users[99]
```

#### DELETE — O(1) cascade

```
DELETE Layer[sel]
```

Deletes the selected record and instantly invalidates its entire
subtree via top-down block pruning — no recursive sweep. The selector
must resolve to a single record:

```console
$ basal query -file demo.bsl \
    -author A -summary S -reason R 'DELETE Accounts[1]'
cascade deleted 3 record(s) under Accounts[1]
commit 6 on main | A | S | R

$ basal query -file demo.bsl \
    -author A -summary S -reason R 'DELETE Accounts[*]'
basal: bql: DELETE Accounts[*] must select a single record ([N] or [#N])
```

#### SHOW — catalog queries

```
SHOW LAYERS
SHOW BRANCHES
SHOW COMMITS
SHOW RECORDS [Layer]
```

```console
$ basal query -file demo.bsl 'SHOW RECORDS Users'
layer | id | slot | parent | children | attrs
Users[3#0] parent=2 children=[] "Sam Rivera" "sam@dev.io"
1 record(s) on branch "main"

$ basal query -file demo.bsl 'SHOW BRANCHES'
name | fork | head
main | main@0 | 5
scratch | main@5 | 5
```

`SHOW COMMITS` prints the audit trail — `id | branch | author | summary
| reason | timestamp` (Unix seconds) — and like every write-backed
object it always carries the mandatory metadata.

#### BRANCH — fork isolation

```
BRANCH LIST
BRANCH USE Name
BRANCH CREATE Name [FROM Source]
```

Branches fork at the source's current head and diverge without merging;
writes on one branch are invisible to the others. Because the CLI runs
one statement per invocation, select the target branch with `-branch`
rather than `BRANCH USE`. `BRANCH CREATE` is equivalent to
`basal branch create`, just run through BQL (and it needs audit
metadata).

---

## branch

Dedicated subcommands for branch management (no BQL required):

```console
$ basal branch create -file demo.bsl scratch -author "Alex <alex@dev.io>"
created branch "scratch" from "main"

$ basal branch list -file demo.bsl
main (fork main@0, head 5)
scratch (fork main@5, head 5)
```

| Subcommand flags | Default | Meaning |
|---|---|---|
| `-file` | *(required)* | path of the container |
| `-from` (`create` only) | `main` | source branch to fork at its head |
| `-author` | `basal <basal@localhost>` | audit author |
| `-summary` | `Branch created` | audit summary |
| `-reason` | `feature isolation` | audit reason |

Reads on a forked branch see the forked state; writes stay isolated:

```console
$ basal query -file demo.bsl -branch scratch 'GET Accounts[*]'
layer | id | slot | parent | children | attrs
Accounts[1#0] parent=0 children=[2] "Acme Corp" "2026-01-01"
1 record(s) on branch "scratch"
```

---

## Output format

Every `query` prints a column header, one line per row, an optional
message, and — for writes — the appended commit block:

```
layer | id | slot | parent | children | attrs
Accounts[1#0] parent=0 children=[2] "Acme Corp" "2026-01-01"
created Accounts#0 (id=1)
commit 2 on main | Alex Rivera <alex@dev.io> | seed tenants | demo data
```

Record rows print as `Layer[id#slot]` with `parent`, the `children`
slot array, and the quoted attribute list. Catalog rows (`SHOW`,
`BRANCH`) join their cells with ` | ` instead.

With `-json` the whole `Result` is emitted: `columns`, `rows`, optional
`message`, optional `commit` (`id`, `branch`, `parent`, `author`,
`summary`, `reason`, `timestamp`), and `use_branch` for `BRANCH USE`.
Zero-valued fields are omitted from each row.

---

## The same BQL over HTTP

`basald` (see `cmd/basald`) serves identical BQL over a REST API —
`GET /v1/query?q=...` for reads, `POST /v1/execute` for writes with
`author`/`summary`/`reason` in the JSON body, plus `/v1/health`,
`/v1/meta`, `/v1/branches`, and `/v1/commits`. The read-only endpoint
rejects write statements with `400`.
