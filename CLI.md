# BasalDB CLI reference (`basal`)

`basal` creates and inspects `.bsl` database files, runs BasalQL (BQL)
statements against them, and manages branches. New to BasalDB? Start
with the [README's getting-started guide](README.md#getting-started-5-minutes).

- [Cheat sheet](#cheat-sheet)
- [Concepts](#concepts)
- [Commands](#commands): [config](#config) · [init](#init) · [query](#query) · [inspect](#inspect) · [branch](#branch) · [version and help](#version-and-help)
- [BQL language](#bql-language): [GET](#get) · [CREATE](#create) · [UPDATE](#update) · [DELETE](#delete) · [SHOW](#show) · [BRANCH](#branch-statements)
- [Recipes](#recipes)
- [Output formats](#output-formats)
- [Errors and exit codes](#errors-and-exit-codes)
- [REST API (`basald`)](#rest-api-basald)
- [Limitations](#limitations)

---

## Cheat sheet

```console
# setup
basal init -file app.bsl -layers "0:Accounts(name,plan),1:Teams(name),2:Users(name,email,role)"
basal config set file app.bsl            # skip -file from now on

# write
basal query 'CREATE Accounts [name="Acme",plan="pro"]'
basal query 'CREATE Teams PARENT Accounts[name="Acme"] [name="Eng"]'
basal query 'CREATE Users PARENT Teams[name="Eng"] [name="Sam",email="sam@acme.io",role=admin]'
basal query 'UPDATE Users[name="Sam"][role="owner"]'
basal query 'DELETE Accounts[name="Acme"]'                 # removes its teams and users too

# read
basal query 'GET Accounts'                                 # all accounts
basal query 'GET Accounts[name="Acme"] -> Teams -> Users'  # walk down
basal query 'GET Users[name,email][role=admin]'            # columns + filter
basal query 'GET Users[*][id=(gt=10)][limit=5,sort=desc]'  # compare, limit, sort
basal query 'SHOW RECORDS Users'
basal query 'CREATE INDEX ON Users(email)'                # equality hash index
basal query 'SHOW SCHEMA'                                  # indexed names end with !
basal query -json 'GET Users'
basal query -format csv 'GET Users[name,email]'

# audit
basal query -author "Alex <a@x.io>" -summary "why in 5 words" -reason "ticket #12" 'UPDATE ...'
basal query 'SHOW COMMITS'

# branches
basal branch create scratch -author "Alex <a@x.io>"
basal query -branch scratch 'DELETE Users[role=guest]'
basal branch list

# inspect
basal inspect
basal inspect -header
```

---

## Concepts

| Term | Meaning |
|---|---|
| **Container** | One `.bsl` file. It holds the layers, records, branches and the commit log. |
| **Layer** | One level of the hierarchy, with a height: `0:Accounts` is the root, `1:Teams` sits under it, `2:Users` under that. Fixed at `init`. |
| **Attributes** | Each record stores an ordered list of **string** values. If you declare names at init (`Users(name,email)`), you can use `name=` / `[name,email]`; otherwise use positions (`0=`, `[0,1]`). Names are case-insensitive. A trailing `!` (`email!`) builds a hash index for `email=` equality. `CREATE INDEX ON Users(email)` adds one later. |
| **Record** | Has an `id` (unique across the file), a `slot` (its position within its layer), a `parent` id (`0` for roots) and a list of `children` ids. |
| **Branch** | An isolated fork of the data, starting from another branch's current state. Default: `main`. |
| **Commit** | Every write appends one commit to the log: branch, parent commit, timestamp, the operations, and optional `author` / `summary` / `reason`. |

---

## Commands

### Flags that work everywhere

- **`-file`** defaults to the `file` config value, else `./basal.bsl`.
- **`-branch`** defaults to the `branch` config value, else `main`.
- Order of precedence: **flag → `basal config` → built-in default**.

### config

Stores per-user defaults so you don't retype `-file` / `-branch`.

```console
$ basal config set file acme.bsl
file = acme.bsl
$ basal config set branch scratch
branch = scratch
$ basal config list
config: /Users/you/Library/Application Support/basal/config
file:   acme.bsl
branch: scratch
$ basal config get file
acme.bsl
$ basal config unset branch
branch unset
```

| Action | Effect |
|---|---|
| `list` | show the config path and values |
| `get KEY` | print one value |
| `set KEY VALUE` | store one value |
| `unset KEY` | remove one value |

Keys: `file`, `branch`. The config is an INI file at `$BASAL_CONFIG` if
that's set, otherwise `~/.config/basal/config` (Linux) or
`~/Library/Application Support/basal/config` (macOS):

```ini
[cli]
file = acme.bsl
branch = scratch
```

**Per-project config:** `export BASAL_CONFIG=./.basalrc` in the project
(or use direnv) and each project gets its own defaults.

### init

Creates a new container. It refuses to overwrite an existing file.

```console
$ basal init -file acme.bsl -layers "0:Accounts(name,plan),1:Teams(name),2:Users(name,email,role,age)"
initialized acme.bsl
layers: 0:Accounts(name,plan),1:Teams(name),2:Users(name,email,role,age)
initial commit: 1 (basal <basal@localhost> | Database initialized | manual initialization)
```

| Flag | Default | Meaning |
|---|---|---|
| `-file` | config `file`, else `./basal.bsl` | file to create |
| `-layers` | `Accounts,Teams,Users` | layer list, root first |
| `-author` | `basal <basal@localhost>` | author of the init commit |
| `-summary` | `Database initialized` | summary of the init commit |
| `-reason` | `manual initialization` | reason of the init commit |

**`-layers` syntax.** A comma-separated list. Each entry is
`[height:]Name[(attr,attr,...)]`:

| Entry | Meaning |
|---|---|
| `Accounts` | height assigned by position (first = 0) |
| `0:Accounts` | explicit height |
| `0:Accounts(name,plan)` | explicit height, with attribute names |

Quote the whole value in the shell: the parentheses are special
characters.

> Layers and attribute names **can't be changed after init** yet
> ([#17](https://github.com/Davidjustice28/basaldb/issues/17),
> [#29](https://github.com/Davidjustice28/basaldb/issues/29)). Declare
> every attribute you expect to need.

### query

Runs **one** BQL statement.

```console
basal query [-file F] [-branch B] [-format table|csv|tsv|json] [-author A -summary S -reason R] 'STATEMENT'
```

| Flag | Default | Meaning |
|---|---|---|
| `-file` | config / `./basal.bsl` | container |
| `-branch` | config / `main` | branch to read or write |
| `-format` | `table` | output format: `table`, `csv`, `tsv` or `json` |
| `-json` | off | alias for `-format json` |
| `-author` | *(empty)* | stored on the commit (writes) |
| `-summary` | *(empty)* | stored on the commit (writes) |
| `-reason` | *(empty)* | stored on the commit (writes) |

- Flags can go before or after the statement.
- `CREATE` / `UPDATE` / `DELETE` work without audit flags; the commit
  then has empty audit fields. `BRANCH CREATE` **requires** them.
- Only one statement per call. A `;`-separated script is rejected
  ([#22](https://github.com/Davidjustice28/basaldb/issues/22)).

### inspect

Prints a summary of the file: version, regions, record counts per
layer, branches and commits.

```console
$ basal inspect
file:    acme.bsl
version: 1.0
format:  MatrixSlottedPage
regions: commit log [4096,5882), data [5882,7747), journal [7747,...)
branch:  main @ commit 11
layers:
  0:Accounts     2 record(s)  attrs: name, plan
  1:Teams        3 record(s)  attrs: name
  2:Users        4 record(s)  attrs: name, email, role, age
branches:
  cleanup (fork main@11, head 12)
  main (fork main@0, head 11)
commits:
  [0011] main |  |  |
  [0010] main | Alex <alex@acme.io> | add ops lead | onboarding ticket #12
  ...
```

| Flag | Meaning |
|---|---|
| `-branch` | branch whose head to show |
| `-header` | print the raw ASCII header instead (useful when debugging a damaged file) |
| `-json` | JSON output; the `layers` entries carry `height`, `name`, `records` and `attributes` |

```console
$ basal inspect -header
Magic:BSL
Version:1.1
Format:MatrixSlottedPage
Layers:[0:Accounts(name,plan), 1:Teams(name), 2:Users(name,email,role,age)]
Branches:cleanup,main
CommitLogStart:4096
CommitLogEnd:5882
DataStart:5882
DataEnd:7747
JournalStart:7747
```

To see the operations each commit performed, read the log directly
(it's plain text): `strings acme.bsl | less`, or use REST
`/v1/commits`. The CLI doesn't display ops yet
([#6](https://github.com/Davidjustice28/basaldb/issues/6)).

### branch

```console
basal branch list   [-file F]
basal branch create NAME [-file F] [-from SRC] [-author A -summary S -reason R]
```

The subcommand comes first, then the name and flags.

```console
$ basal branch create cleanup -author "Alex <alex@acme.io>" -summary "try removal" -reason "offboarding dry run"
created branch "cleanup" from "main"

$ basal branch list
cleanup (fork main@11, head 12)
main (fork main@0, head 11)
```

`fork main@11` = created from `main` at commit 11. `head 12` = its latest
commit.

| Flag (`create`) | Default | Meaning |
|---|---|---|
| `-from` | `main` | branch to fork from (at its current head) |
| `-author` | `basal <basal@localhost>` | audit author |
| `-summary` | `Branch created` | audit summary |
| `-reason` | `feature isolation` | audit reason |

Use a branch with `-branch NAME` on `query` / `inspect`, or set it as
the default with `basal config set branch NAME`. Delete, rename, diff
and merge aren't available yet
([#19](https://github.com/Davidjustice28/basaldb/issues/19),
[#20](https://github.com/Davidjustice28/basaldb/issues/20)).

### version and help

```console
$ basal version
basal v1.0.0 (commit 9807408, built 2026-10-14T20:31:02Z)
bsl format: 1.0 (MatrixSlottedPage)

$ basal --version        # same output
$ basal -help            # usage; 'basal help' and '--help' work too
```

The build stamp is empty in a plain `go build` (`basal dev (commit none,
built unknown)`); install or CI builds pass it with `-ldflags`:

```sh
go build -ldflags "-X main.version=v1.0.0 -X main.commit=$(git rev-parse --short HEAD) -X main.date=$(date -u +%FT%TZ)" -o basal ./cmd/basal
```

The `bsl format` line is the file format version the binary reads and
writes — quote it in bug reports.

---

## BQL language

Keywords are case-insensitive (`get users` works). Layer names must match
what you declared.

### Brackets

Every statement is built from the same bracket syntax:

| Bracket | Contains | Example |
|---|---|---|
| **columns** | attribute names/indexes, or `*` | `[name,email]`, `[0,1]`, `[*]` |
| **filters** | `field=value` / `field=(op=value)`, comma = AND | `[role=admin,age=(gte=30)]` |
| **limit / sort** | `limit=N`, `sort=asc\|desc` | `[limit=10,sort=desc]` |
| **values** (CREATE/UPDATE) | `field=value` pairs | `[name="Sam",role=admin]` |

**Fields** you can filter on: declared attribute names, positional
indexes (`0`, `1`, …), and the built-ins `id`, `slot` and `parent`.

**Values**: only plain words and whole numbers can be unquoted
(`role=admin`, `id=3`). Quote anything with spaces, dots or symbols,
**including decimals**: `"Acme Corp"`, `"#ff0000"`, `"0.9"`,
`"browser.click"`. An unquoted `0.9` fails with `unexpected character "."`. Single or double quotes both work; escapes are `\n \t \\ \" \'`.
Negative numbers are allowed.

**Comparison operators** (`field=(op=value)`):

| Op | Meaning |
|---|---|
| `eq` | equal (same as `field=value`) |
| `ne` | not equal |
| `gt` / `gte` | greater than / or equal |
| `lt` / `lte` | less than / or equal |

Comparisons are numeric when both sides look like numbers, and string
comparisons otherwise. There's no OR, IN, LIKE or contains yet
([#9](https://github.com/Davidjustice28/basaldb/issues/9),
[#10](https://github.com/Davidjustice28/basaldb/issues/10)).

### GET

```
GET Layer[columns][filters][limit/sort] -> Layer[...] -> ...
```

```console
$ basal query 'GET Accounts'                                   # every account, every column
$ basal query 'GET Users[name,email]'                          # two columns
$ basal query 'GET Users[*][role=admin]'                       # filter only
$ basal query 'GET Users[role=admin]'                          # same: a filter may sit in the first bracket
$ basal query 'GET Users[name,age][age=(gte=30)][sort=desc]'   # columns + filter + sort
$ basal query 'GET Users[*][id=7]'                             # by id
$ basal query 'GET Users[*][parent=3]'                         # children of record 3
$ basal query 'GET Users[*][role=admin][age=(lt=30)]'          # repeated filter brackets AND together
```

**Paths (`->`)** walk from parents down to their children. Each step must
be the layer **directly below** the previous one, and you can filter at
any step:

```console
$ basal query 'GET Accounts[name="Acme Corp"] -> Teams[name="Engineering"] -> Users[name,email]'
layer | id | slot | parent | children | attrs
Users[6#0] parent=3 children=[] "Sam" "sam@acme.io"
Users[7#1] parent=3 children=[] "Elena" "elena@acme.io"
2 record(s) on branch "main"
```

Things to know:
- Only the **last** layer's records are returned
  ([#3](https://github.com/Davidjustice28/basaldb/issues/3) adds parent
  columns).
- Skipping a layer (`Accounts -> Users`) is an error
  ([#14](https://github.com/Davidjustice28/basaldb/issues/14)).
- You can't walk upward yet. Use the `parent` id with `[id=…]`
  ([#13](https://github.com/Davidjustice28/basaldb/issues/13)).
- `sort` orders by record **id** only; `limit` is applied last
  ([#11](https://github.com/Davidjustice28/basaldb/issues/11)).
- No matches is **not** an error: you get `0 record(s)`.

### CREATE

```
CREATE Layer [field=value, ...]                          # root record
CREATE Layer PARENT Layer[filters] [field=value, ...]    # child record
```

```console
$ basal query 'CREATE Accounts [name="Acme Corp",plan="pro"]'
layer | id | slot | parent | children | attrs
Accounts[1#0] parent=0 children=[] "Acme Corp" "pro"
created Accounts#0 (id=1)
commit 2 on main

$ basal query 'CREATE Teams PARENT Accounts[name="Acme Corp"] [name="Engineering"]'
$ basal query 'CREATE Users PARENT Teams[id=3] [0="Sam",1="sam@acme.io"]'   # positional
```

- The `PARENT` filter must match **exactly one** record in the layer
  directly above. If names can repeat, use `[id=N]`.
- Attributes you leave out are stored as `""`. Unknown names are
  rejected.
- All values are stored as strings (`age="abc"` is accepted;
  [#16](https://github.com/Davidjustice28/basaldb/issues/16)).
- Always use `PARENT` for non-root layers. A child-layer record created
  without one is an orphan
  ([#5](https://github.com/Davidjustice28/basaldb/issues/5)).

### UPDATE

```
UPDATE Layer[filters][field=value, ...]
```

```console
$ basal query 'UPDATE Users[email="elena@acme.io"][role="lead"]'
layer | id | slot | parent | children | attrs
Users[7#1] parent=3 children=[] "Elena" "elena@acme.io" "lead" "28"
updated 1 record(s) in "Users"
```

- Only the attributes you list change.
- Updates **every** record that matches. `UPDATE Users[*][...]` (or no
  filter) updates the whole layer.
- No matches → error (`no records in "Users" matched [...]`).
- `id`, `slot` and `parent` can't be changed (moving records is
  [#15](https://github.com/Davidjustice28/basaldb/issues/15)).

### DELETE

```
DELETE Layer[filters]
```

```console
$ basal query 'DELETE Accounts[name="Globex"]'
cascade deleted 3 record(s) under 1 root(s) in "Accounts"
```

- Deletes every matching record **and everything beneath it**, however
  deep. The count includes descendants.
- No filter (or `[*]`) deletes the whole layer.
- No matches → error.
- Try big deletes on a branch first (see [Recipes](#recipes)).

### SHOW

```
SHOW LAYERS              height | name | records
SHOW SCHEMA [Layer]      height | layer | attributes
SHOW RECORDS [Layer]     all records, or one layer's
SHOW BRANCHES            name | fork | head
SHOW COMMITS             id | branch | author | summary | reason | timestamp
```

```console
$ basal query 'SHOW LAYERS'
height | name | records
0 | Accounts | 2
1 | Teams | 3
2 | Users | 4

$ basal query 'SHOW SCHEMA'
height | layer | attributes
0 | Accounts | name, plan
1 | Teams | name
2 | Users | name, email, role, age
```

`SHOW SCHEMA` reports the attribute names declared at `init`, so you don't
have to provoke an error to find them. Indexed names keep a trailing `!`
(`email!`). A layer that declares none shows an empty attribute list (use
positional `0=`, `[0,1]` instead). `CREATE INDEX ON Layer(attr)` adds an
equality hash index to an existing attribute and checkpoints. `timestamp`
is in Unix seconds.

`basal query` and `basal inspect` take `-max-memory 256MB` (also `basald`)
to cap the decoded page cache. The default is 64MB. The id index is not
part of that cap.

### BRANCH statements

```
BRANCH LIST
BRANCH CREATE Name [FROM Source]     # needs -author/-summary/-reason
BRANCH USE Name                      # only meaningful over REST/embedded; on the CLI use -branch
```

---

## Recipes

**Find a record's id, then use it**
```console
$ basal query 'GET Teams[name="Engineering"]'
Teams[3#0] parent=1 children=[6,7] "Engineering"
$ basal query 'CREATE Users PARENT Teams[id=3] [name="Ana"]'
```

**List a record's children**
```console
$ basal query 'GET Users[*][parent=3]'
```

**Look up a record's parent** (no upward paths yet)
```console
$ basal query 'GET Users[email="sam@acme.io"]'     # → parent=3
$ basal query 'GET Teams[id=3]'
```

**Dry-run a destructive change on a branch**
```console
$ basal branch create dryrun -author "you <you@x.io>" -summary "dry run" -reason "check delete scope"
$ basal query -branch dryrun 'DELETE Teams[name="Sales"]'
cascade deleted 2 record(s) under 1 root(s) in "Teams"
# happy? run the same statement without -branch. Not happy? ignore the branch.
```

**Audit every write in a script**
```sh
B="basal query -author \"$USER\" -summary \"nightly sync\" -reason \"cron\""
eval "$B 'UPDATE Accounts[name=\"Acme Corp\"][plan=\"enterprise\"]'"
```

**Seed from a file** (one statement per line, no transactions yet)
```sh
while IFS= read -r stmt; do basal query "$stmt" || break; done < seed.bql
```

**Get plain values for scripts**
```console
$ basal query -json 'GET Users[name,email][role=admin]' | jq -r '.rows[].attrs | @tsv'
Sam	sam@acme.io
Kim	kim@globex.io
```

**Count records in a layer**
```console
$ basal query 'SHOW LAYERS'                          # counts per layer
$ basal query -json 'GET Users[role=admin]' | jq '.rows | length'
```

---

## Output formats

**Default (table).** Record rows look like this:

```
layer | id | slot | parent | children | attrs
Teams[3#0] parent=1 children=[6,7] "Engineering"
```

`Teams[3#0]` = layer `Teams`, id `3`, slot `0`. `children` lists the
child **ids**. Then come the attribute values, in declared order (or the
columns you asked for). Catalog rows (`SHOW`, `BRANCH`) are joined with
` | `. Writes end with `commit N on BRANCH | author | summary | reason`,
with the trailing empty fields dropped when a write carries no audit
metadata: an unannotated write prints just `commit N on BRANCH`.

**`-format csv` / `-format tsv`.** One header row plus one row per
result, suitable for spreadsheets, `xsv` or `duckdb`:

```console
$ basal query -format csv 'GET Users[name,email][role=admin]'
layer,id,slot,parent,children,name,email
Users,9,5,3,,Kim,kim@globex.io
```

`layer`, `id`, `slot`, `parent` and `children` always get their own
columns. The attribute columns are named by the fields you selected, or
by the layer's declared attribute names when you select none — extended
with positional indices (`0`, `1`, …) past the declaration, which is how
a layer that declares no names shows all of its attributes. `SHOW
RECORDS` over several layers at once keeps every attribute in a single
`attrs` column instead, since the layers don't share names. The human
`message` and `commit` lines go to **stderr** in these modes, so stdout
stays parseable.

**`-json`** (alias: `-format json`). The full result:

```json
{
  "columns": ["layer","id","slot","parent","children","attrs"],
  "fields": ["name","email"],
  "rows": [{"layer":"Users","id":9,"slot":3,"parent":5,"children":[],"attrs":["Kim","kim@globex.io"]}],
  "message": "1 record(s) on branch \"main\"",
  "commit": {"id":13,"branch":"main","parent":11,"author":"…","summary":"…","reason":"…","timestamp":1792006982}
}
```

`commit` only appears for writes. Record rows always carry `layer`,
`id`, `slot`, `parent`, `children` and `attrs` — zeros included, empty
lists as `[]` — so `"slot": 0` is distinguishable from a missing field
([#7](https://github.com/Davidjustice28/basaldb/issues/7)). `fields`
names the values in each row's `attrs`, in order; it is absent when the
layer declares no names.

---

## Errors and exit codes

| Exit | Meaning |
|---|---|
| `0` | success |
| `1` | runtime or BQL error, printed to stderr as `basal: …` |
| `2` | usage error (bad flags or subcommand) |

Common errors:

| Message | Fix |
|---|---|
| `PARENT Accounts[name=Acme] matched 2 records (need exactly 1)` | use `PARENT Accounts[id=N]` |
| `layer "Users" is not the root layer; CREATE Users needs PARENT …` | add `PARENT Teams[filter]` — only the root layer creates without one |
| `layer "Users" has no attribute "nickname" (declared: …)` | use a declared name or a positional index |
| `path step "Users" must be the child layer of height 0 (got height 2)` | include every layer in the path: `Accounts -> Teams -> Users` |
| `no records in "Users" matched [id=99]` | UPDATE/DELETE matched nothing |
| `sort must be asc or desc` | sorting is by id only |
| `unknown comparison operator "in"` | supported: `eq ne gt gte lt lte` |
| `unexpected GET after statement` | one statement per call |
| `engine: unknown branch "x"` | `basal branch list` |
| `engine: Author is required` | pass `-author -summary -reason` (BRANCH CREATE) |
| `engine: x.bsl already exists` | `init` won't overwrite; choose another `-file` |

---

## REST API (`basald`)

`basald` serves the same BQL over HTTP and creates the file if it
doesn't exist.

```console
$ basald -file acme.bsl [-layers "…"]
# 127.0.0.1:8080, fsync on. A public bind needs a token:

$ BASAL_TOKEN="$(openssl rand -hex 32)" basald -file acme.bsl -addr 0.0.0.0:8080
```

| Flag | Default | Meaning |
|---|---|---|
| `-file` | `basal.bsl` | container |
| `-addr` | `127.0.0.1:8080` | listen address. `0.0.0.0:8080` or `:8080` binds every interface. If you leave `-addr` unset and `PORT` is set (Cloud Run), basald binds `0.0.0.0:$PORT` |
| `-layers` | `Accounts,Teams,Users` | used only when creating a new file |
| `-sync` | on | fsync every commit before the response. `-sync=false` acknowledges the write before it hits disk |
| `-token-env` | `BASAL_TOKEN` | name of the env var that holds the bearer token. The token is not a flag, so it stays out of `ps` |
| `-read-token-env` | unset | env var for a token that may only call GET routes. Writes get `403` |
| `-insecure-no-auth` | off | allow a non-loopback listen with no token |
| `-tls-cert`, `-tls-key` | unset | serve HTTPS. On a public bind without these, put TLS in front of basald |

`Authorization: Bearer <token>` is required on every route except
`/v1/health` once a token is configured. A non-loopback listen with no
token refuses to start. Concurrent `POST /v1/execute` calls on the same
branch are queued in the server, so clients do not see `409 Conflict`
for overlapping single statements.

| Endpoint | Method | Use |
|---|---|---|
| `/v1/health` | GET | `{"status":"ok"}` |
| `/v1/meta` | GET | path, version, layers with counts, branches, head commit |
| `/v1/query?q=…[&branch=…]` | GET | read-only BQL. Writes return `400`. |
| `/v1/execute` | POST | any BQL: `{"statement","branch","author","summary","reason"}` |
| `/v1/branches` | GET / POST | list; create with `{"name","fork_from","author","summary","reason"}` |
| `/v1/commits[?branch=…]` | GET | commit log, **including the `ops` of each commit** |

```console
$ curl -G localhost:8080/v1/query --data-urlencode 'q=GET Accounts[name="Acme Corp"] -> Teams -> Users[name,email]'

$ curl -X POST localhost:8080/v1/execute -H 'content-type: application/json' -d '{
    "statement": "UPDATE Users[name=\"Sam\"][role=\"owner\"]",
    "author": "Alex <alex@acme.io>", "summary": "promote", "reason": "ticket #14"}'

$ curl 'localhost:8080/v1/commits?branch=main'
[{"kind":"commit","id":13,"branch":"main","author":"Alex <alex@acme.io>", … ,"ops":["UPDATE Users#0 id=6"]}, …]
```

Errors come back as `{"error":"…"}` with status `400` / `401` / `403` / `405` / `409` / `413`. Don't run
CLI writes against a file while `basald` is serving it
([#4](https://github.com/Davidjustice28/basaldb/issues/4)).

---

## Limitations

Current gaps, with tracking issues:

- **Concurrency:** only one writing process per file (#4)
- **Statements:** one per call, no transactions (#22, #1)
- **Filters:** AND only, no OR/IN/LIKE (#9, #10)
- **Sort/paging:** id only, no offset (#11); no COUNT (#12)
- **Paths:** downward only, no skipping layers, leaf columns only (#13, #14, #3)
- **Records:** can't change parent (#15); orphans possible (#5)
- **Schema:** strings only, fixed at init except `CREATE INDEX` (#16, #17, #29, #30, #18). `-max-memory` (query, inspect, basald) caps the page cache, not the id index.
- **Branches:** no delete/diff/merge (#19, #20); no time travel (#21)
- **Tooling:** no export/import, verify, compact, version, CSV (#24, #25, #26, #27, #28), no interactive shell (#23)
