# BasalDB

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/basaldb_logo_white.png">
  <img alt="BasalDB" src="docs/assets/basaldb_logo_black.png" width="320">
</picture>

**Agent memory in a single file.**

Store what your agent did, what it learned and how it does things, as
trees with a record of who wrote each entry and why. Recall by path,
branch to experiment, and forget a user in one step. BasalDB is
embedded: it runs inside your agent's process with no database server
to set up, and keeps one `.bsl` file per agent or per user that you can
copy, ship or delete. When you do want a server, `basald` serves the
same file over HTTP ([REST](GUIDE.md#using-it-from-other-languages-rest)).

```console
$ basal init -file mem.bsl -layers "0:Users(name),1:Topics(name),2:Facts(text,source,confidence)"
$ basal config set file mem.bsl
$ basal query 'CREATE Users [name="u_42"]'
$ basal query 'CREATE Topics PARENT Users[name="u_42"] [name="travel"]'

# an agent stores a fact, with where it came from and why
$ basal query -author memory-agent -summary "learned preference" -reason "user said so in session 12" \
    'CREATE Facts PARENT Topics[name="travel"] [text="Prefers aisle seats",source="session:12",confidence="0.9"]'

# recall by walking the tree: no joins
$ basal query 'GET Users[name="u_42"] -> Topics[name="travel"] -> Facts[text,confidence]'
Facts[3#0] parent=2 children=[] "Prefers aisle seats" "0.9"

# why does the agent believe that?
$ basal query 'SHOW COMMITS'
id | branch | author | summary | reason | timestamp
4 | main | memory-agent | learned preference | user said so in session 12 | 1792092841

# forget the user completely, in one step
$ basal query 'DELETE Users[name="u_42"]'
cascade deleted 3 record(s) under 1 root(s) in "Users"
```

### One file for everything an agent knows

| Kind of memory | Example | What BasalDB gives you | Status |
|---|---|---|---|
| **Episodic**: what happened | session → task → step traces, crawl runs | trees and `->` paths; who/why on every write; branches for retries | ✅ today |
| **Semantic**: what it knows | user → topic → fact, page → findings | path recall, one-step forget, provenance per fact | ✅ today |
| | ask a question, get the relevant facts for *this* user only | RAG search scoped to a subtree, `[near="…",k=5]` | 🛣️ planned ([#49](https://github.com/Davidjustice28/basaldb/issues/49)) |
| | raw page HTML, screenshots, long transcripts | blob attributes stored out-of-line | 🛣️ planned ([#50](https://github.com/Davidjustice28/basaldb/issues/50)) |
| **Procedural**: how to do things | namespace → skill → files | versioned via the commit log; whole skill loads in ~6 µs | ✅ today |

See [AI agents: traces, flows and memory](GUIDE.md#ai-agents-traces-flows-and-memory)
for full walkthroughs. BasalDB also works for any other data that nests
(SaaS tenants, design canvases, game state); see [Where BasalDB fits](#where-basaldb-fits).

> **Status: early / experimental.** Create, query, update, cascade delete,
> branches, equality indexes and the audit log work. The file format and
> query language may still change. Don't use it for data you can't afford
> to lose. Numbers, known risks and what is not supported yet are in
> [GUIDE.md](GUIDE.md).

Capabilities, more examples, REST, the file format:
**[GUIDE.md](GUIDE.md)**. Command and query-language reference:
**[CLI.md](CLI.md)**.

---

## Where BasalDB fits

* **AI agent traces and memory.** Sessions → tasks → steps, or users →
  topics → facts, with who/why on every write, branches for retries,
  and one-step "forget". See
  [AI agents](GUIDE.md#ai-agents-traces-flows-and-memory).
* **Multi-tenant SaaS.** One customer, one file. Exporting a customer is
  a file copy; deleting one is deleting a file.
* **Design and canvas tools.** Pages hold frames, frames hold shapes.
  Deleting a frame takes all its children with it in one step.
* **Game state.** Worlds contain zones, zones contain entities. A save
  file is one file.
* **Org charts and group membership.** "Who's under this manager" is a
  path query; removing a manager removes their whole branch. (Moving
  people to a new parent is planned in [#15](https://github.com/Davidjustice28/basaldb/issues/15).)
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

---

## Install

Requires Go 1.21+.

**Names at a glance:** the project is **BasalDB**; the repo and Go
module are `basaldb`; the CLI is `basal`, the HTTP server is `basald`
and the benchmark is `basal-bench`; database files end in `.bsl`; the
query language is BasalQL (**BQL**). "Branch" in these docs means a
BasalDB data branch inside a `.bsl` file (`basal branch create`), not a
git branch.

```console
$ git clone https://github.com/Davidjustice28/basaldb.git
$ cd basaldb
$ go install ./cmd/basal ./cmd/basald     # installs into $(go env GOPATH)/bin
# or build a local binary:
$ go build -o basal ./cmd/basal
# stamp the version banner (basal version) with release info:
$ go build -ldflags "-X main.version=v1.0.0 -X main.commit=$(git rev-parse --short HEAD) -X main.date=$(date -u +%FT%TZ)" -o basal ./cmd/basal
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
commit 9 on main
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

(Branches can't be merged back yet. See [#20](https://github.com/Davidjustice28/basaldb/issues/20).)

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

Next: the full syntax is in **[CLI.md](CLI.md)**. More examples, the
HTTP API and how the file is laid out are in **[GUIDE.md](GUIDE.md)**.

---

## How it compares

BasalDB is deliberately narrow: **embedded, tree-shaped memory for an
agent, in one file.** For most jobs an established database is the
better choice.

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

Rule of thumb: **memory, traces or skills that live with one agent or
user and get read, branched or forgotten as a unit → BasalDB.**
Relational app data → SQLite or PostgreSQL. Large-scale semantic search
→ a vector database. Number crunching → DuckDB.

Agent-memory products (SQLite, Dolt, Mem0, vector databases) and
measured numbers against SQLite are in
[GUIDE.md](GUIDE.md#how-it-compares).

## License

MIT. See [LICENSE](LICENSE).
