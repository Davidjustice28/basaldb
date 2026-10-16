# BasalDB

**Agent memory in a single file.**

Store what your agent did, what it learned and how it does things, as
trees with a record of who wrote each entry and why. Recall by path,
branch to experiment, and forget a user in one step. BasalDB is
embedded: it runs inside your agent's process with no database server
to set up, and keeps one `.bsl` file per agent or per user that you can
copy, ship or delete. When you do want a server, `basald` serves the
same file over HTTP ([REST](#using-it-from-other-languages-rest)).

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

See [AI agents: traces, flows and memory](#ai-agents-traces-flows-and-memory)
for full walkthroughs. BasalDB also works for any other data that nests
(SaaS tenants, design canvases, game state); see [Where BasalDB fits](#where-basaldb-fits).

> **Status: early / experimental.** Create, query, update, cascade delete,
> branches and the audit log work, but the file format and query language
> may still change. Don't use it for data you can't afford to lose. Known
> risks, fix first:
> - **Don't let several processes write to the same file at once**: writes
>   can be lost silently ([#4](https://github.com/Davidjustice28/basaldb/issues/4)).
>   Use one writer, or route writes through `basald`. `basald` queues
>   concurrent requests on the same branch, so those writers don't have
>   to retry 409s themselves.
> - Slotted pages load on demand into a bounded cache (default 64MB,
>   `-max-memory` on `basal query` / `inspect` and `basald`). The id
>   index, and any secondary indexes, stay in memory. A single attribute
>   can still be up to 4 GiB, and reading that record pulls the whole
>   value into the cache.
>
> **Performance:** path recall, id lookups and subtree deletes run in
> microseconds to about a millisecond at 250k–500k records in-process.
> Equality on an indexed attribute (`url!` at init, or
> `CREATE INDEX ON Pages(url)`) is a hash lookup. Other attribute
> filters still scan the layer. See
> [BENCHMARKS.md](BENCHMARKS.md), the issues labeled
> [`priority: high`](https://github.com/Davidjustice28/basaldb/labels/priority%3A%20high),
> and the [scalability roadmap (#42)](https://github.com/Davidjustice28/basaldb/issues/42).

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
| See the catalog | `SHOW LAYERS / SCHEMA / RECORDS / BRANCHES / COMMITS` | `SHOW SCHEMA Users` |
| Branch the data | `basal branch create` + `-branch` | `basal query -branch scratch 'DELETE ...'` |
| Record who changed what and why | `-author -summary -reason` | stored in the file's commit log |
| Machine-readable output | `-json` or `-format csv\|tsv\|json` | `basal query -format csv 'GET Users[name,email]'` |
| Defaults so you don't repeat `-file` | `basal config set` | `basal config set file acme.bsl` |
| HTTP access | `basald` | `GET /v1/query?q=...`, `POST /v1/execute` |

## Not supported yet

Knowing these up front saves you some surprises. Each one links to its
tracking issue.

| You might want to… | Status |
|---|---|
| Write from several processes at once | ⚠️ unsafe: writes can be lost without an error ([#4](https://github.com/Davidjustice28/basaldb/issues/4)) |
| Run several statements in one transaction | one statement per call ([#22](https://github.com/Davidjustice28/basaldb/issues/22), [#1](https://github.com/Davidjustice28/basaldb/issues/1)) |
| OR / IN filters, text search | AND equality/comparison only ([#9](https://github.com/Davidjustice28/basaldb/issues/9), [#10](https://github.com/Davidjustice28/basaldb/issues/10)) |
| Sort by an attribute, paginate with offset | sort by id only, `limit` only ([#11](https://github.com/Davidjustice28/basaldb/issues/11)) |
| COUNT / aggregates | no ([#12](https://github.com/Davidjustice28/basaldb/issues/12)) |
| Walk up the tree, skip layers, get parent columns in the same row | down, one layer at a time, leaf columns only ([#13](https://github.com/Davidjustice28/basaldb/issues/13), [#14](https://github.com/Davidjustice28/basaldb/issues/14), [#3](https://github.com/Davidjustice28/basaldb/issues/3)) |
| Move a record to a new parent | no ([#15](https://github.com/Davidjustice28/basaldb/issues/15)) |
| Typed attributes, required/unique | every value is a string ([#16](https://github.com/Davidjustice28/basaldb/issues/16)) |
| Change layers/attributes after `init` | schema is fixed at init ([#17](https://github.com/Davidjustice28/basaldb/issues/17), [#29](https://github.com/Davidjustice28/basaldb/issues/29), [#30](https://github.com/Davidjustice28/basaldb/issues/30)) |
| Delete, diff or merge branches | create and read only ([#19](https://github.com/Davidjustice28/basaldb/issues/19), [#20](https://github.com/Davidjustice28/basaldb/issues/20)) |
| Query the past / undo a commit | no ([#21](https://github.com/Davidjustice28/basaldb/issues/21)) |
| Export / import a whole database as JSON or CSV | CSV/TSV query output works ([#28](https://github.com/Davidjustice28/basaldb/issues/28)); no file export/import yet ([#24](https://github.com/Davidjustice28/basaldb/issues/24)) |
| Verify or compact the file | no ([#25](https://github.com/Davidjustice28/basaldb/issues/25), [#26](https://github.com/Davidjustice28/basaldb/issues/26)) |

Child layers always need `PARENT`: a `CREATE` outside the root layer
without one is an error, so no record can end up unreachable by a
cascade delete.

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
$ basald -file acme.bsl
# listens on 127.0.0.1:8080, fsyncs each commit

$ curl -G localhost:8080/v1/query --data-urlencode 'q=GET Accounts[name="Acme Corp"] -> Teams -> Users[name,email]'
{"columns":[...],"rows":[{"layer":"Users","id":6,"parent":3,"attrs":["Sam","sam@acme.io"]}, ...]}

$ curl -X POST localhost:8080/v1/execute -H 'content-type: application/json' -d '{
    "statement": "UPDATE Users[name=\"Sam\"][role=\"owner\"]",
    "author": "Alex <alex@acme.io>", "summary": "promote", "reason": "ticket #14"
  }'
```

A bind outside localhost (`-addr 0.0.0.0:8080`, or `PORT` on Cloud Run)
refuses to start until `BASAL_TOKEN` is set. Send it as
`Authorization: Bearer …`. `/v1/health` stays open. See
[CLI.md](CLI.md#rest-api-basald).

Endpoints: `/v1/health`, `/v1/meta`, `/v1/query` (reads only),
`/v1/execute` (writes), `/v1/branches`, `/v1/commits`. Add `?branch=NAME`
or `"branch"` in the JSON body to use a branch. Concurrent
`/v1/execute` calls on one branch are queued.

> Run **either** `basald` **or** CLI writes against a file, not both at
> once ([#4](https://github.com/Davidjustice28/basaldb/issues/4)).

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
- **Pair with a vector index for recall (for now).** BasalDB has no
  similarity search yet. Store embeddings in a vector store keyed by the
  BasalDB record `id`; BasalDB holds the structure and provenance. Native
  RAG search scoped to a subtree is planned in
  [#49](https://github.com/Davidjustice28/basaldb/issues/49).
- **Large bodies fit, and reading one loads that page.** An attribute
  can be up to 4 GiB and a record can have up to 2^32−1 children. The
  page cache (default 64MB, `-max-memory`) holds recently read pages;
  the id index stays resident. Blob attributes, stored out of line, are
  planned in
  [#50](https://github.com/Davidjustice28/basaldb/issues/50).
- **Use branches for speculative memory.** Let an agent write
  "hypotheses" on a branch and keep only what's confirmed.

**Current gaps for agent workloads:** only one process can write to a
file at a time, so give each agent its own file or route writes through
`basald` ([#4](https://github.com/Davidjustice28/basaldb/issues/4)).
Branches can't be merged back yet
([#20](https://github.com/Davidjustice28/basaldb/issues/20)), and there's
no text search ([#10](https://github.com/Davidjustice28/basaldb/issues/10)),
moving records ([#15](https://github.com/Davidjustice28/basaldb/issues/15))
or reading past states
([#21](https://github.com/Davidjustice28/basaldb/issues/21)). Depth is
fixed by the layer stack, so pick it to match your trace or memory
shape.

---

## How the file works

(Deeper dive: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).)

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
([#6](https://github.com/Davidjustice28/basaldb/issues/6)).

---

## How it compares

BasalDB is deliberately narrow: **embedded, tree-shaped memory for an
agent, in one file.** For most jobs an established database is the
better choice. These tables are meant to help you choose honestly.

### For agent memory

| Option | Best for | Pick it instead of BasalDB when… |
|---|---|---|
| **BasalDB** | Per-agent or per-user memory, traces and skills that are naturally trees; read by subtree, forgotten as a unit, with who/why on every write | — |
| **SQLite (+ sqlite-vec)** | The default embedded store; DIY memory tables with vector search via an extension | you need full SQL, joins across unrelated data, or a mature ecosystem today. This is BasalDB's closest alternative: it's embedded too, but trees need recursive queries and there's no built-in provenance, branching or one-step forget |
| **Dolt** | A versioned SQL database (branch, diff, merge) run as a server | you want git-style versioning on relational tables, with merge working today (#20 is still open here) |
| **Memory frameworks** (Mem0, Zep, Letta, …) | LLM-driven extraction and retrieval of memories from conversations, as a library or service | you want memory managed for you, end to end. They can sit on top of a store; BasalDB is the store |
| **Vector databases** (Pinecone, Qdrant, Weaviate, Milvus, pgvector) | Semantic search over large embedding collections | retrieval by similarity is the main job. BasalDB has no similarity search yet ([#49](https://github.com/Davidjustice28/basaldb/issues/49)) |
| **Forkable Postgres** (Neon, Tiger Data, …) | Giving each agent task its own disposable copy of a production Postgres | the agent works against your app's existing relational data |
| **Multi-model / graph** (SurrealDB, Neo4j) | Memory with many cross-links between entities | facts relate to each other across the tree. BasalDB records have one parent and no links yet ([#54](https://github.com/Davidjustice28/basaldb/issues/54)) |

### General-purpose

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

## License

MIT. See [LICENSE](LICENSE).
