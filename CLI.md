# BasalDB CLI (`basal`)

`basal` is the single-file database utility: initialize `.bsl` containers,
inspect their ASCII header and audit trail, run BasalQL (BQL) statements,
and manage branches.

```
basal init    [-file DATA.bsl] [-layers H:Name,...] [-author A -summary S -reason R]
basal inspect [-file DATA.bsl] [-branch NAME] [-header] [-json]
basal query   [-file DATA.bsl] [-branch NAME] [-author A -summary S -reason R] 'BQL STATEMENT'
basal branch  list [-file DATA.bsl]
basal branch  create NAME [-file DATA.bsl] [-from SRC] [-author A -summary S -reason R]
basal config  list | get KEY | set KEY VALUE | unset KEY
```

## Getting started

Build from the repository root, then create a container, seed it, and
query it:

```console
$ go build -o basal ./cmd/basal

$ basal init -file demo.bsl -layers "0:Accounts(name,date),1:Teams(name),2:Users(name,email)"
initialized demo.bsl
layers: 0:Accounts(name,date),1:Teams(name),2:Users(name,email)
initial commit: 1 (basal <basal@localhost> | Database initialized | manual initialization)

$ basal query -file demo.bsl \
    -author "Alex Rivera <alex@dev.io>" -summary "seed data" -reason "quickstart" \
    'CREATE Accounts [name="Acme Corp",date="2026-01-01"]'
layer | id | slot | parent | children | attrs
Accounts[1#0] parent=0 children=[] "Acme Corp" "2026-01-01"
created Accounts#0 (id=1)
commit 2 on main | Alex Rivera <alex@dev.io> | seed data | quickstart

$ basal query -file demo.bsl \
    -author "Alex Rivera <alex@dev.io>" -summary "seed data" -reason "quickstart" \
    'CREATE Teams PARENT Accounts[id=1] [name="Eng Team"]'
layer | id | slot | parent | children | attrs
Teams[2#0] parent=1 children=[] "Eng Team"
created Teams#0 (id=2)
commit 3 on main | Alex Rivera <alex@dev.io> | seed data | quickstart

$ basal query -file demo.bsl \
    -author "Alex Rivera <alex@dev.io>" -summary "seed data" -reason "quickstart" \
    'CREATE Users PARENT Teams[id=2] [name="Sam",email="sam@dev.io"]'
layer | id | slot | parent | children | attrs
Users[3#0] parent=2 children=[] "Sam" "sam@dev.io"
created Users#0 (id=3)
commit 4 on main | Alex Rivera <alex@dev.io> | seed data | quickstart

$ basal query -file demo.bsl 'GET Accounts[id=1] -> Teams -> Users[Name,Email]'
layer | id | slot | parent | children | attrs
Users[3#0] parent=2 children=[] "Sam" "sam@dev.io"
1 record(s) on branch "main"

$ basal query -file demo.bsl 'GET Users[Name][Email="sam@dev.io"]'
layer | id | slot | parent | children | attrs
Users[3#0] parent=2 children=[] "Sam"
1 record(s) on branch "main"
```

Record writes are metadata-free; pass `-author` / `-summary` /
`-reason` to stamp the commit block (as above), and note that
structural operations like `BRANCH CREATE` require them. See
[BQL syntax](#bql-syntax) for the full language.

## Concepts

* **Container (`.bsl` file)** — the whole database in one portable,
  self-contained file: an ASCII header, a human-readable commit log,
  and binary data pages. A container is what you pass around with
  `-file`; it holds the layer stack, the branches, and the full audit
  trail.
* **Layer stack** — inside a container, the hierarchy is a *stack* of
  named layers, each at a height: `0:Accounts` is the root, and
  `1:Teams`, `2:Users` stack above it (the README's diagram draws
  children above parents). `-layers` declares the stack at `init`;
  queries walk it with `->`.
* **Attributes** — each record stores a positional list of strings, set
  with a bracket of `field=value` pairs (`[name="Acme Corp",date="2026-01-01"]`).
  Optionally declare names per layer
  at init (`0:Accounts(name,date)`); then queries can use `name=`
  filters and `[name,date]` column lists instead of raw indexes. Names
  are case-insensitive.
* **Records** — every record has an `id` (unique across the file), a
  `slot` (position within its layer), an optional `parent`, and a
  `children` array.
* **Branches** — git-style isolation: forks of the whole container
  that diverge without merging. The default branch is `main`.
* **Commits** — every write appends an immutable commit block to the
  audit trail. Record writes stamp it with `author` / `summary` /
  `reason` when you pass them; structural operations (container init,
  branch creation) require them.

## Notes that apply everywhere

* **`-file` and `-branch` have defaults.** Resolution order is: flag →
  value stored by `basal config set` → built-in fallback (`./basal.bsl`
  for the container, `main` for the branch). So a fully configured
  workflow is just `basal config set file demo.bsl` once, then bare
  `basal query 'GET Accounts[*]'`.
* For `query`, flags may appear before *or* after the statement
  (`basal query 'GET Accounts[*]' -file demo.bsl` works).
* For `branch`, the subcommand (`list` / `create`) must come first, then
  its flags.
* Exit codes: `0` success, `1` runtime or BQL error (printed as
  `basal: <error>` to stderr), `2` usage error.

---

## config

Stores per-user defaults so you don't retype `-file` / `-branch` on
every command — the same idea as `gcloud config set`. The defaults are
kept in a small INI file:

| Subcommand | Effect |
|---|---|
| `basal config list` | show the config path and current values |
| `basal config get KEY` | print one stored value |
| `basal config set KEY VALUE` | store one default |
| `basal config unset KEY` | remove one default |

Valid keys are `file` (default container) and `branch` (default branch).

```console
$ basal config set file demo.bsl
file = demo.bsl
$ basal config set branch scratch
branch = scratch
$ basal config list
config: /home/alice/.config/basal/config
file:   demo.bsl
branch: scratch
$ basal config unset branch
branch unset
```

The config file lives at `$BASAL_CONFIG` if that variable is set,
otherwise in the OS user-config directory (`~/.config/basal/config` on
Linux, `~/Library/Application Support/basal/config` on macOS). Its
format:

```ini
[cli]
file = demo.bsl
branch = scratch
```

`$BASAL_CONFIG` also makes it easy to keep a **per-project** config:

```console
$ BASAL_CONFIG=./.basalrc basal query 'SHOW LAYERS'
```

Flags always win over stored defaults, so one-off overrides stay one
command long: `basal query -branch main 'SHOW RECORDS'`.

---

## init

Creates a new `.bsl` container with the given layers and writes the
initial commit.

| Flag | Default | Meaning |
|---|---|---|
| `-file` | config `file`, else `./basal.bsl` | path of the container to create |
| `-layers` | `Accounts,Teams,Users` | layer definitions, comma separated; optionally declare attribute names per layer (`0:Accounts(name,date)`), which enables symbolic BQL like `GET Accounts[Name]` |
| `-author` | `basal <basal@localhost>` | audit author of the init commit |
| `-summary` | `Database initialized` | audit summary |
| `-reason` | `manual initialization` | audit reason |

A layer entry is either `Name` (height assigned by position, starting at
0) or `H:Name` (explicit height on the Y-axis), optionally followed by
the attribute names to declare (`0:Accounts(name,date)`):

```console
$ basal init -file demo.bsl -layers 0:Accounts(name,date),1:Teams(name),2:Users(name,email)
initialized demo.bsl
layers: 0:Accounts(name,date),1:Teams(name),2:Users(name,email)
initial commit: 1 (basal <basal@localhost> | Database initialized | manual initialization)
```

---

## inspect

Prints the container's header, region map, branches, and commit trail.

| Flag | Default | Meaning |
|---|---|---|
| `-file` | config `file`, else `./basal.bsl` | path of the container |
| `-branch` | config `branch`, else `main` | branch whose head to show |
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
| `-file` | config `file`, else `./basal.bsl` | path of the container |
| `-branch` | config `branch`, else `main` | branch to run against |
| `-author` | *(empty)* | audit author stamped onto the commit block |
| `-summary` | *(empty)* | audit summary stamped onto the commit block |
| `-reason` | *(empty)* | audit reason stamped onto the commit block |
| `-json` | off | print the result as JSON |

Every write appends an immutable commit block — the audit trail. Record
writes (`CREATE`, `UPDATE`, `DELETE`) are metadata-free: without the
flags the commit simply carries empty audit fields. Structural
operations require the metadata; `BRANCH CREATE` without it fails:

```console
$ basal query -file demo.bsl 'BRANCH CREATE scratch'
basal: engine: Author is required
```

### BQL syntax

Keywords are case-insensitive (`show records users` works). Attributes
are quoted strings (double or single quotes; escapes `\n`, `\t`, `\\`,
`\"`, `\'`).

#### Picking records

`GET` picks records with filters (`[id=2]`, `[slot=0]`, `[Name=David]`);
`[*]` or no bracket at all means every record. Mutations use the same
filter brackets (`UPDATE Users[id=6]`, `DELETE Accounts[region=emea]`).
The builtin fields `id`, `slot` and `parent` are available everywhere.

#### GET — path traversal, columns, filters, limit/sort

```
GET Layer[columns][filters][limit=N,sort=asc|desc] (-> Step...)
```

Walks parent→child edges down the matrix with zero joins. Each step must
name the layer at exactly one height below the previous one:

```console
$ basal query -file demo.bsl 'GET Accounts[id=1] -> Teams -> Users'
layer | id | slot | parent | children | attrs
Users[3#0] parent=2 children=[] "Sam" "sam@dev.io"
1 record(s) on branch "main"
```

Skipping a layer is an error:

```console
$ basal query -file demo.bsl 'GET Accounts[id=1] -> Users'
basal: bql: path step "Users" must be the child layer of height 0 (got height 2)
```

**Bracket 1 — columns.** Lists the attribute columns to return, in the
order written. Names need the layer to declare them at `init` (see
`-layers` below); indexes work everywhere. `[*]` (or no bracket at all)
returns every attribute:

```
GET Users[*]              every user, every attribute
GET Users                 same thing
GET Users[Name,Age]       every user, two columns (declared names)
GET Users[1,0]            every user, attributes 1 and 0 (positional)
```

Attribute names match case-insensitively (`[Name]` = `[name]`).

**Bracket 2 — filters.** Comma-separated predicates, all of which must
hold (AND). This is also how records are picked: `id=` and `slot=` are
filters like any other. `Field=Value` is equality; `Field=(op=Value)`
compares with `eq`, `ne`, `gt`, `gte`, `lt` or `lte`. Fields are
attribute names, attribute indexes, or the builtins `id`, `slot` and
`parent`. Ordering operators compare numerically when both sides are
numbers, as strings otherwise. Values with spaces must be quoted;
negative numbers are allowed. Repeating the filter bracket just ANDs
it:

```
GET Users[*][id=2]                    one record by id
GET Users[*][slot=0]                  one record by slot
GET Users[*][Name=David]
GET Users[*][Age=(gt=21),Gender=(ne=female)]
GET Users[*][0=David][id=(lt=10)]
GET Users[*][id=3][age=(gte=21)]
```

Filters also work mid-path (they prune the traversal) and may sit in the
first bracket (`GET Users[Name=David]` selects all users, filtered).

**Bracket 3 — limit and sort.** `limit=N` caps the rows (applied last);
`sort=asc|desc` orders rows by record ID. Both only apply to the final
step of a path.

**Putting it all together** — one query combining columns, filters and
limit/sort, against a database created with
`-layers "0:Accounts(name,region),1:Users(name,age,gender)"`:

```console
$ basal query -file demo.bsl \
    'GET Users[Name,Age,Gender][Name=David,Age=(gt=21),Gender=(ne=female)][limit=1,sort=desc]'
layer | id | slot | parent | children | attrs
Users[2#0] parent=1 children=[] "David" "25" "male"
1 record(s) on branch "main"
```

On layers without declared attribute names, name references fail with a
message explaining the `-layers` declaration; positional references
(`[0]`, `[0,1]`, `[1=Globex]`) still work everywhere.

#### CREATE — root records and children

```
CREATE Layer [field=value, ...]
CREATE Layer PARENT Layer[filters] [field=value, ...]
```

The attribute bracket uses the same `field=value` pairs as filters —
by declared name (`name="Sam"`) or positional index (`0="Sam"`) — so
`CREATE`, `UPDATE`, `DELETE`, and `GET` all share one bracket syntax.
Values are stored as strings. Without `PARENT` the record roots its own
subtree; with it the filters must resolve to exactly **one** existing
record:

```console
$ basal query -file demo.bsl \
    -author "Alex Rivera <alex@dev.io>" -summary "seed tenants" -reason "demo data" \
    'CREATE Accounts [name="Acme Corp",date="2026-01-01"]'
layer | id | slot | parent | children | attrs
Accounts[1#0] parent=0 children=[] "Acme Corp" "2026-01-01"
created Accounts#0 (id=1)
commit 2 on main | Alex Rivera <alex@dev.io> | seed tenants | demo data

$ basal query -file demo.bsl \
    -author "Alex Rivera <alex@dev.io>" -summary "seed tenants" -reason "demo data" \
    'CREATE Teams PARENT Accounts[id=1] [name="Eng Team"]'
created Teams#0 (id=2)
```

The engine assigns IDs and slots; the CLI output shows both
(`Layer[id#slot]`).

#### UPDATE — set attributes

```
UPDATE Layer[filters][field=value, ...]
```

The first bracket picks the records, the second sets their attributes —
the same bracket pairs as every other statement. Only the attributes
you list change; the rest keep their values (like SQL's
`UPDATE ... SET`). No filters means every record in the layer. Zero
matches is an error, never a silent no-op:

```console
$ basal query -file demo.bsl \
    -author "Sam <sam@dev.io>" -summary "rename" -reason "demo" \
    'UPDATE Users[slot=0][name="Sam Rivera"]'
layer | id | slot | parent | children | attrs
Users[3#0] parent=2 children=[] "Sam Rivera" "sam@dev.io"
updated 1 record(s) in "Users"

$ basal query -file demo.bsl \
    -author A -summary S -reason R 'UPDATE Users[id=99][name="Ghost"]'
basal: bql: no records in "Users" matched [id=99]
```

#### DELETE — O(1) cascade

```
DELETE Layer[filters]
```

Deletes every record matching the filters and instantly invalidates each
root's entire subtree via top-down block pruning — no recursive sweep.
No filters (or `[*]`) deletes every record in the layer. Zero matches is
an error:

```console
$ basal query -file demo.bsl \
    -author A -summary S -reason R 'DELETE Accounts[id=1]'
cascade deleted 3 record(s) under 1 root(s) in "Accounts"
commit 6 on main | A | S | R

$ basal query -file demo.bsl \
    -author A -summary S -reason R 'DELETE Users[age=(lt=18)]'
cascade deleted 1 record(s) under 1 root(s) in "Users"
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
| reason | timestamp` (Unix seconds). The audit columns are whatever
the write stamped on its commit block (empty for metadata-free record
writes; always filled for initialization and branch events).

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
| `-file` | config `file`, else `./basal.bsl` | path of the container |
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
