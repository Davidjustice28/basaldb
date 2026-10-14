# BasalDB

**One file. One database. Built for data that lives in hierarchies.**

BasalDB is a small embedded database for data that nests: accounts contain
teams, teams contain people, canvases contain frames, frames contain shapes.
The whole database is a single portable file (`.bsl`). There's no server to
install, and every write is recorded in an append-only commit log you can
read with `cat`.

```
Account "Acme Corp"
├── Team "Engineering"
│   ├── User "Sam"
│   └── User "Elena"
└── Team "Sales"
    └── User "Ravi"
```

```console
$ basal query 'GET Accounts[name="Acme Corp"] -> Teams[name="Engineering"] -> Users[name,email]'
layer | id | slot | parent | children | attrs
Users[6#0] parent=3 children=[] "Sam" "sam@acme.io"
Users[7#1] parent=3 children=[] "Elena" "elena@acme.io"
2 record(s) on branch "main"
```

> **Status: early / experimental.** The core works (create, query, update,
> cascade delete, branches, audit log), but the format and the query
> language may still change. Don't use it for data you can't afford to
> lose. In particular, **don't let several processes write to the same
> file at once** ([#4](https://github.com/Davidjustice28/basildb/issues/4)).
>
> **Performance:** comfortable up to about 50k records per file today.
> Beyond that, most operations slow down as the file grows. See
> [BENCHMARKS.md](BENCHMARKS.md) and the
> [scalability roadmap (#42)](https://github.com/Davidjustice28/basildb/issues/42).

---

## Contents

- [What you can do today](#what-you-can-do-today)
- [Not supported yet](#not-supported-yet)
- [Install](#install)
- [Getting started (5 minutes)](#getting-started-5-minutes)
- [More examples](#more-examples)
- [Using it from other languages (REST)](#using-it-from-other-languages-rest)
- [AI agents: traces, flows and memory](#ai-agents-traces-flows-and-memory)
- [How the file works](#how-the-file-works)
- [How it compares](#how-it-compares)
- [Where BasalDB fits](#where-basaldb-fits)

Full command and query-language reference: **[CLI.md](CLI.md)**.

---

## What you can do today

| Capability | How | Example |
|---|---|---|
| Define a hierarchy | `basal init -layers` | `-layers "0:Accounts(name,plan),1:Teams(name),2:Users(name,email)"` |
| Create root records | `CREATE Layer [...]` | `CREATE Accounts [name="Acme Corp",plan="pro"]` |
| Create child records | `CREATE Layer PARENT Layer[filter] [...]` | `CREATE Teams PARENT Accounts[name="Acme Corp"] [name="Sales"]` |
| Walk down the tree | `->` between layers | `GET Accounts[id=1] -> Teams -> Users` |
| Filter | `=`, `eq ne gt gte lt lte` (AND only) | `GET Users[age=(gte=30),role=admin]` |
| Pick columns | first bracket | `GET Users[name,email]` |
| Limit and sort by id | last bracket | `GET Users[*][limit=10,sort=desc]` |
| Update attributes | `UPDATE Layer[filter][...]` | `UPDATE Users[email="sam@acme.io"][role="owner"]` |
| Delete a whole subtree | `DELETE Layer[filter]` | `DELETE Accounts[name="Globex"]` (teams and users go too) |
| See the catalog | `SHOW LAYERS / RECORDS / BRANCHES / COMMITS` | `SHOW RECORDS Users` |
| Branch the data | `basal branch create` + `-branch` | `basal query -branch scratch 'DELETE ...'` |
| Record who changed what and why | `-author -summary -reason` | stored in the file's commit log |
| Machine-readable output | `-json` | `basal query -json 'GET Users'` |
| Defaults so you don't repeat `-file` | `basal config set` | `basal config set file acme.bsl` |
| HTTP access | `basald` | `GET /v1/query?q=...`, `POST /v1/execute` |

## Not supported yet

Knowing these up front saves you some surprises. Each one links to its
tracking issue.

| You might want to… | Status |
|---|---|
| Write from several processes at once | ⚠️ unsafe: writes can be lost without an error ([#4](https://github.com/Davidjustice28/basildb/issues/4)) |
| Run several statements in one transaction | one statement per call ([#22](https://github.com/Davidjustice28/basildb/issues/22), [#1](https://github.com/Davidjustice28/basildb/issues/1)) |
| OR / IN filters, text search | AND equality/comparison only ([#9](https://github.com/Davidjustice28/basildb/issues/9), [#10](https://github.com/Davidjustice28/basildb/issues/10)) |
| Sort by an attribute, paginate with offset | sort by id only, `limit` only ([#11](https://github.com/Davidjustice28/basildb/issues/11)) |
| COUNT / aggregates | no ([#12](https://github.com/Davidjustice28/basildb/issues/12)) |
| Walk up the tree, skip layers, get parent columns in the same row | down, one layer at a time, leaf columns only ([#13](https://github.com/Davidjustice28/basildb/issues/13), [#14](https://github.com/Davidjustice28/basildb/issues/14), [#3](https://github.com/Davidjustice28/basildb/issues/3)) |
| Move a record to a new parent | no ([#15](https://github.com/Davidjustice28/basildb/issues/15)) |
| Typed attributes, required/unique | every value is a string ([#16](https://github.com/Davidjustice28/basildb/issues/16)) |
| Change layers/attributes after `init` | schema is fixed at init ([#17](https://github.com/Davidjustice28/basildb/issues/17), [#29](https://github.com/Davidjustice28/basildb/issues/29), [#30](https://github.com/Davidjustice28/basildb/issues/30)) |
| Delete, diff or merge branches | create and read only ([#19](https://github.com/Davidjustice28/basildb/issues/19), [#20](https://github.com/Davidjustice28/basildb/issues/20)) |
| Query the past / undo a commit | no ([#21](https://github.com/Davidjustice28/basildb/issues/21)) |
| Export / import JSON or CSV | no ([#24](https://github.com/Davidjustice28/basildb/issues/24), [#28](https://github.com/Davidjustice28/basildb/issues/28)) |
| Verify or compact the file | no ([#25](https://github.com/Davidjustice28/basildb/issues/25), [#26](https://github.com/Davidjustice28/basildb/issues/26)) |

Also note: a child-layer record can currently be created without a
`PARENT` ([#5](https://github.com/Davidjustice28/basildb/issues/5)). Always
pass `PARENT` for non-root layers.

---

## Install

Requires Go 1.21+.

```console
$ git clone https://github.com/Davidjustice28/basildb.git
$ cd basildb
$ go install ./cmd/basal ./cmd/basald     # installs into $(go env GOPATH)/bin
# or build a local binary:
$ go build -o basal ./cmd/basal
```

Check it's on your `PATH`:

```console
$ basal
basal - BasalDB single-file database utility
...
```

---

## Getting started (5 minutes)

We'll model a small SaaS app: **accounts → teams → users**.

### 1. Create the database

Layers are listed from the root down. Naming the attributes in
parentheses lets you refer to them by name in queries.

```console
$ basal init -file acme.bsl -layers "0:Accounts(name,plan),1:Teams(name),2:Users(name,email,role,age)"
initialized acme.bsl
layers: 0:Accounts(name,plan),1:Teams(name),2:Users(name,email,role,age)
```

Make it the default so you can skip `-file` from now on:

```console
$ basal config set file acme.bsl
file = acme.bsl
```

### 2. Add data

Roots need no parent. Children name their parent with a filter that must
match **exactly one** record.

```console
$ basal query 'CREATE Accounts [name="Acme Corp",plan="pro"]'
$ basal query 'CREATE Accounts [name="Globex",plan="free"]'

$ basal query 'CREATE Teams PARENT Accounts[name="Acme Corp"] [name="Engineering"]'
$ basal query 'CREATE Teams PARENT Accounts[name="Acme Corp"] [name="Sales"]'
$ basal query 'CREATE Teams PARENT Accounts[name="Globex"] [name="Ops"]'

$ basal query 'CREATE Users PARENT Teams[name="Engineering"] [name="Sam",email="sam@acme.io",role="admin",age="34"]'
$ basal query 'CREATE Users PARENT Teams[name="Engineering"] [name="Elena",email="elena@acme.io",role="dev",age="28"]'
$ basal query 'CREATE Users PARENT Teams[name="Sales"] [name="Ravi",email="ravi@acme.io",role="rep",age="41"]'
```

Each write prints the new record and its commit:

```
layer | id | slot | parent | children | attrs
Users[8#2] parent=4 children=[] "Ravi" "ravi@acme.io" "rep" "41"
created Users#2 (id=8)
commit 9 on main |  |  |
```

`Users[8#2]` means *layer Users, id 8, slot 2*. Ids are unique across
the whole file. You can leave out attributes (they become `""`).

> **Quoting tip:** wrap the whole statement in single quotes and the
> values in double quotes. Only plain words and whole numbers can go
> unquoted (`role=admin`, `id=3`). Anything with spaces, dots or symbols
> needs quotes, including decimals (`"0.9"`) and dotted names
> (`"browser.click"`).

### 3. Query

```console
$ basal query 'GET Accounts'
Accounts[1#0] parent=0 children=[3,4] "Acme Corp" "pro"
Accounts[2#1] parent=0 children=[5] "Globex" "free"

# everyone in one account: walk down the tree
$ basal query 'GET Accounts[name="Acme Corp"] -> Teams -> Users'
Users[6#0] parent=3 children=[] "Sam" "sam@acme.io" "admin" "34"
Users[7#1] parent=3 children=[] "Elena" "elena@acme.io" "dev" "28"
Users[8#2] parent=4 children=[] "Ravi" "ravi@acme.io" "rep" "41"

# filter at any step, choose columns at the end
$ basal query 'GET Accounts[name="Acme Corp"] -> Teams[name="Engineering"] -> Users[name,email]'
Users[6#0] parent=3 children=[] "Sam" "sam@acme.io"
Users[7#1] parent=3 children=[] "Elena" "elena@acme.io"

# comparisons, newest first
$ basal query 'GET Users[name,age][age=(gte=30)][sort=desc]'
Users[8#2] parent=4 children=[] "Ravi" "41"
Users[6#0] parent=3 children=[] "Sam" "34"
```

### 4. Update

Only the attributes you list change:

```console
$ basal query 'UPDATE Users[email="elena@acme.io"][role="lead"]'
Users[7#1] parent=3 children=[] "Elena" "elena@acme.io" "lead" "28"
updated 1 record(s) in "Users"
```

### 5. Try a risky change on a branch

Branches are isolated copies inside the same file. Delete a whole
customer on a branch and `main` doesn't change:

```console
$ basal branch create cleanup -author "Alex <alex@acme.io>" -summary "try removal" -reason "offboarding dry run"
created branch "cleanup" from "main"

$ basal query -branch cleanup 'DELETE Accounts[name="Globex"]'
cascade deleted 3 record(s) under 1 root(s) in "Accounts"     # account + team + user

$ basal query -branch cleanup 'GET Accounts'
Accounts[1#0] parent=0 children=[3,4] "Acme Corp" "pro"

$ basal query 'GET Accounts'            # main is untouched
Accounts[1#0] parent=0 children=[3,4] "Acme Corp" "pro"
Accounts[2#1] parent=0 children=[5] "Globex" "free"
```

(Branches can't be merged back yet. See [#20](https://github.com/Davidjustice28/basildb/issues/20).)

### 6. Record who did it and why

Add `-author`, `-summary` and `-reason` to any write and they're stored
in the commit log:

```console
$ basal query -author "Alex <alex@acme.io>" -summary "add ops lead" -reason "ticket #12" \
    'CREATE Users PARENT Teams[name="Ops"] [name="Kim",email="kim@globex.io",role="admin",age="19"]'
...
commit 10 on main | Alex <alex@acme.io> | add ops lead | ticket #12

$ basal query 'SHOW COMMITS'
id | branch | author | summary | reason | timestamp
10 | main | Alex <alex@acme.io> | add ops lead | ticket #12 | 1792006969
9 | main |  |  |  | 1792006969
...
```

### 7. Look inside the file

```console
$ basal inspect
file:    acme.bsl
version: 1.0
format:  MatrixSlottedPage
branch:  main @ commit 11
layers:
  0:Accounts     2 record(s)
  1:Teams        3 record(s)
  2:Users        4 record(s)
branches:
  cleanup (fork main@11, head 12)
  main (fork main@0, head 11)
...
```

Next: the full syntax is in **[CLI.md](CLI.md)**.

---

## More examples

**Design tool: pages → frames → shapes**

```console
$ basal init -file canvas.bsl -layers "0:Pages(title),1:Frames(name,x,y),2:Shapes(kind,color)"
$ basal query -file canvas.bsl 'CREATE Pages [title="Home"]'
$ basal query -file canvas.bsl 'CREATE Frames PARENT Pages[title="Home"] [name="Hero",x=0,y=0]'
$ basal query -file canvas.bsl 'CREATE Shapes PARENT Frames[name="Hero"] [kind=rect,color="#ff0000"]'
$ basal query -file canvas.bsl 'GET Pages[title="Home"] -> Frames -> Shapes[color="#ff0000"]'
$ basal query -file canvas.bsl 'DELETE Frames[name="Hero"]'     # frame + all its shapes
```

**One file per tenant**

```console
$ basal init -file tenants/acme.bsl   -layers "0:Projects(name),1:Tasks(title,status)"
$ basal init -file tenants/globex.bsl -layers "0:Projects(name),1:Tasks(title,status)"
$ cp tenants/acme.bsl exports/acme-2026-10-14.bsl     # export = copy the file
$ rm tenants/globex.bsl                               # offboarding = delete the file
```

**Positional attributes (no names declared)**

```console
$ basal init -file raw.bsl -layers "Orgs,Members"
$ basal query -file raw.bsl 'CREATE Orgs [0="Initech"]'
$ basal query -file raw.bsl 'GET Orgs[0][0=Initech]'
```

**Scripting with JSON**

```console
$ basal query -json 'GET Users[name,email][role=admin]' | jq -r '.rows[].attrs | @tsv'
Sam	sam@acme.io
Kim	kim@globex.io
```

---

## Using it from other languages (REST)

`basald` serves the same BQL over HTTP:

```console
$ basald -file acme.bsl -addr :8080

$ curl -G localhost:8080/v1/query --data-urlencode 'q=GET Accounts[name="Acme Corp"] -> Teams -> Users[name,email]'
{"columns":[...],"rows":[{"layer":"Users","id":6,"parent":3,"attrs":["Sam","sam@acme.io"]}, ...]}

$ curl -X POST localhost:8080/v1/execute -H 'content-type: application/json' -d '{
    "statement": "UPDATE Users[name=\"Sam\"][role=\"owner\"]",
    "author": "Alex <alex@acme.io>", "summary": "promote", "reason": "ticket #14"
  }'
```

Endpoints: `/v1/health`, `/v1/meta`, `/v1/query` (reads only),
`/v1/execute` (writes), `/v1/branches`, `/v1/commits`. Add `?branch=NAME`
to read from a branch. See [CLI.md](CLI.md#rest-api-basald).

> Run **either** `basald` **or** CLI writes against a file, not both at
> once ([#4](https://github.com/Davidjustice28/basildb/issues/4)).

---

## AI agents: traces, flows and memory

Agent data is naturally a tree, and agents need the things BasalDB is
built around: a record of who did what and why, cheap "try it and throw
it away" copies, and clean deletion.

| Agent need | BasalDB feature |
|---|---|
| Runs are trees: session → task → step → tool call | layers + `->` paths, no joins |
| "Which agent wrote this, and why?" | `-author` / `-summary` / `-reason` on every write, kept in an append-only log |
| Try a plan, keep or drop it | branches: retry on a branch while `main` keeps the original run |
| Forget a user or a run completely | `DELETE` removes the whole subtree in one step |
| Portable, per-user or per-agent state | one `.bsl` file per user/agent: copy it, ship it, delete it |
| Inspect a run without tooling | the commit log is plain text (`strings run.bsl`) |

### Agent traces and flows

Record each run as **session → task → step**, stamping each step with the
agent that took it and why:

```console
$ basal init -file agent.bsl -layers "0:Sessions(goal,user),1:Tasks(title,status),2:Steps(tool,input,status)"
$ basal query -file agent.bsl 'CREATE Sessions [goal="Book a flight to NYC",user="u_42"]'
$ basal query -file agent.bsl 'CREATE Tasks PARENT Sessions[id=1] [title="Search flights",status="running"]'

$ basal query -file agent.bsl -author "planner-agent" -summary "open search page" -reason "need options under 400 USD" \
    'CREATE Steps PARENT Tasks[id=2] [tool="browser.navigate",input="kayak.com/flights/SFO-NYC",status="ok"]'
$ basal query -file agent.bsl -author "planner-agent" -summary "sort by price" -reason "cheapest first" \
    'CREATE Steps PARENT Tasks[id=2] [tool="browser.click",input="#sort-price",status="error"]'

# what failed in this session?
$ basal query -file agent.bsl 'GET Sessions[id=1] -> Tasks -> Steps[tool,input,status][status="error"]'
Steps[4#1] parent=2 children=[] "browser.click" "#sort-price" "error"
```

**Retry on a branch.** The failed attempt stays on `main` as evidence,
and the retry happens in isolation:

```console
$ basal branch create attempt-2 -file agent.bsl -author "planner-agent" -summary "retry" -reason "selector failed"
$ basal query -file agent.bsl -branch attempt-2 'UPDATE Steps[status="error"][input="button[aria-label=Sort]",status="ok"]'
$ basal query -file agent.bsl -branch attempt-2 'GET Steps[tool,status]'
Steps[3#0] parent=2 children=[] "browser.navigate" "ok"
Steps[4#1] parent=2 children=[] "browser.click" "ok"
```

The log reads like a run transcript:

```
[Commit:0004]
Branch: main
Author: planner-agent
Reason: need options under 400 USD
Summary: open search page
Ops:
  CREATE Steps#0 id=3 parent=2
```

This works well for browser-automation and tool-calling agents
(run → page → action → result) and for multi-step workflows where you
want to replay or audit what happened.

### Agent memory

Organize long-term memory as **user → project → topic → fact**, with the
provenance of each fact recorded:

```console
$ basal init -file mem.bsl -layers "0:Users(name),1:Projects(name),2:Topics(name),3:Facts(text,source,confidence)"
$ basal query -file mem.bsl 'CREATE Users [name="u_42"]'
$ basal query -file mem.bsl 'CREATE Projects PARENT Users[name="u_42"] [name="travel"]'
$ basal query -file mem.bsl 'CREATE Topics PARENT Projects[name="travel"] [name="preferences"]'
$ basal query -file mem.bsl -author "memory-agent" -summary "learned preference" -reason "user said so in session 1" \
    'CREATE Facts PARENT Topics[name="preferences"] [text="Prefers aisle seats",source="session:1",confidence="0.9"]'

# recall: everything confident about this user's travel
$ basal query -file mem.bsl 'GET Users[name="u_42"] -> Projects[name="travel"] -> Topics -> Facts[text,confidence][confidence=(gte="0.8")]'
Facts[4#0] parent=3 children=[] "Prefers aisle seats" "0.9"

# "forget me": the user's whole memory tree, in one step
$ basal query -file mem.bsl 'DELETE Users[name="u_42"]'
cascade deleted 4 record(s) under 1 root(s) in "Users"
```

**Patterns that work well**
- **One file per user or agent.** Memory is portable and isolated, and
  deleting it for privacy means deleting a file.
- **Pair with a vector index for recall.** BasalDB has no similarity
  search. Store embeddings in a vector store keyed by the BasalDB record
  `id`; BasalDB holds the structure and provenance.
- **Use branches for speculative memory.** Let an agent write
  "hypotheses" on a branch and keep only what's confirmed.

**Current gaps for agent workloads:** only one process can write to a
file at a time, so give each agent its own file or route writes through
`basald` ([#4](https://github.com/Davidjustice28/basildb/issues/4)).
Branches can't be merged back yet
([#20](https://github.com/Davidjustice28/basildb/issues/20)), and there's
no text search ([#10](https://github.com/Davidjustice28/basildb/issues/10)),
moving records ([#15](https://github.com/Davidjustice28/basildb/issues/15))
or reading past states
([#21](https://github.com/Davidjustice28/basildb/issues/21)). Depth is
fixed by the layer stack, so pick it to match your trace or memory
shape.

---

## How the file works

A `.bsl` file has three parts:

1. **A readable header**: the file signature, layer definitions and
   branch list.
2. **A readable commit log**: every write as a plain-text block with its
   branch, parent commit, author, summary, reason, timestamp and the
   operations it performed:
   ```
   [Commit:0010]
   Branch: main
   Parent: 9
   Author: Alex <alex@acme.io>
   Reason: ticket #12
   Summary: add ops lead
   Timestamp: 1792006969
   Ops:
     CREATE Users#3 id=9 parent=5
   ```
3. **Binary data pages**: the records, laid out so that walking down a
   hierarchy is pointer arithmetic instead of a search.

The log is append-only but not yet signed or hash-chained, so it shows
what happened but can't prove nobody edited the file
([#6](https://github.com/Davidjustice28/basildb/issues/6)).

---

## How it compares

BasalDB is deliberately narrow. For most jobs an established database
is the better choice. This table is meant to help you choose honestly.

| Database | Best for | Pros | Cons |
|---|---|---|---|
| **BasalDB** | Deeply nested data where whole subtrees are created, read or deleted together | Hierarchies are built in (no joins); deleting a subtree is one step; commit log and data branches; single file; tiny, no setup | Very new and unproven; no joins across unrelated data; no aggregations or SQL; one writer process at a time; no drivers or ORM |
| **SQLite** | General-purpose embedded storage; the default choice for most apps | Rock solid, everywhere, huge ecosystem, full SQL | Flat tables, so hierarchies need recursive queries; no built-in audit trail or branching |
| **Turso / libSQL** | Hosted SQLite with replicas close to users | SQL you know; managed replication and forking | Hosted service; still flat tables |
| **DuckDB** | Analytics over large tables | Very fast scans and aggregates; reads Parquet/CSV | Not built for per-record app reads/writes; no hierarchy model |
| **PostgreSQL / MySQL** | Client-server apps with many concurrent writers | Full SQL, concurrency, mature tooling | Needs a server; hierarchies are awkward (recursive CTEs); no per-tenant file |
| **MongoDB** | Flexible, schema-light documents | Nested documents feel natural | Server to run; queries across documents are weak; no audit trail by default |
| **Neo4j** | Many-hop relationships in any direction | Arbitrary graph traversal | Heavy; overkill for strict parent-child trees |
| **RocksDB / LevelDB** | Raw key-value speed | Extremely fast get/put | No structure or query language |

Rule of thumb: **hierarchies that get deleted or handed around as a unit
→ BasalDB. Almost everything else → SQLite or PostgreSQL. Number
crunching → DuckDB.**

## Where BasalDB fits

* **AI agent traces and memory.** Sessions → tasks → steps, or users →
  topics → facts, with who/why on every write, branches for retries,
  and one-step "forget". See [AI agents](#ai-agents-traces-flows-and-memory).
* **Multi-tenant SaaS.** One customer, one file. Exporting a customer is
  a file copy; deleting one is deleting a file.
* **Design and canvas tools.** Pages hold frames, frames hold shapes.
  Deleting a frame takes all its children with it in one step.
* **Game state.** Worlds contain zones, zones contain entities. A save
  file is one file.
* **Org charts and group membership.** "Who's under this manager" is a
  path query; removing a manager removes their whole branch. (Moving
  people to a new parent is planned in [#15](https://github.com/Davidjustice28/basildb/issues/15).)
* **Bills of materials and nested config.** A product contains
  assemblies, which contain parts.
* **Audited settings and permission trees.** Every write is in a
  plain-text log with who/why when you pass them.
* **Preview environments for data.** Branch, seed a scenario, throw the
  branch away.

## When not to use BasalDB

* Ad-hoc joins across unrelated entities → **SQLite / PostgreSQL**
* Heavy aggregation or analytics → **DuckDB**
* Many processes writing at once → **PostgreSQL**
* You need drivers, ORMs and GUI tools today → almost anything else

## License

MIT. See [LICENSE](LICENSE).
