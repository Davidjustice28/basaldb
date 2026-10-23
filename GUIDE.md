# BasalDB guide

How to use BasalDB past the [introduction](README.md): what works
today, what does not, more examples, the HTTP API, agent traces and
memory, the file format, and a closer look at other stores.

New here? Start with [Install](README.md#install) and
[Getting started](README.md#getting-started-5-minutes). Full command
and query-language reference: **[CLI.md](CLI.md)**.

## Contents

- [Where it stands](#where-it-stands)
- [What you can do today](#what-you-can-do-today)
- [Not supported yet](#not-supported-yet)
- [More examples](#more-examples)
- [Using it from other languages (REST)](#using-it-from-other-languages-rest)
- [AI agents: traces, flows and memory](#ai-agents-traces-flows-and-memory)
- [How the file works](#how-the-file-works)
- [How it compares](#how-it-compares)

---

## Where it stands

**Status: early / experimental.** Create, query, update, cascade delete,
branches, equality indexes and the audit log work. The file format and
query language may still change. Don't use it for data you can't afford
to lose.

**Numbers** (2026-10-16, Apple M3 Pro; details in
[BENCHMARKS.md](BENCHMARKS.md)):

- **Ingest** of the stock schema is about 106k records/s at 1.05M
  records. A hash index on a unique attribute (`email!`, one value per
  user) dropped that to 7.8k records/s. Index attributes you look up
  and that repeat, such as `status` or `role`. A unique index over a
  million values is resident and expensive to build.
- **Open.** A checkpointed 14.04 GB file, 206 million records, 10 GiB
  of pages and a 3.30 GB id index, opens in 12 ms. Heap after that
  open is 7 MB and process RSS is 18 MB. Id pairs stay a mapped sorted
  array and are binary-searched. Page bodies sit in a 64MB cache
  (`-max-memory`). Secondary indexes are still decoded into memory.
  A file built by normal commits also parses its whole commit log:
  the 1.05M bench file (about 47 MB of log) opens in 84 ms with 74 MB
  of heap.
- **Reads.** A cached point read is about 1 µs, whether by id or by an
  indexed attribute. The first touch of a page is about 250 µs at
  1.05M, and 2.8 ms for the first `Users[id=1]` on the 14 GB file. The
  same `email=` lookup with no index is 280 ms at 1.05M. Other filters
  still scan the layer.
- **Writes** stay flat: a create is about 12 µs, and deleting a
  211-record account is under 1 ms.
- **A branch shares unchanged pages.** One branch plus one update grew
  the 1.05M file from 166 MB to 183 MB. The extra bytes are a second
  copy of the id pairs (16 bytes per record) and the one modified page.
  The same step used to reach 283 MB
  ([#36](https://github.com/Davidjustice28/basaldb/issues/36)).

Known risks, fix first:

- **One process holds the file.** Open and Init take an exclusive lock
  on `file.bsl.lock` and keep it until Close. A second process waits
  up to 30 seconds, then fails with `database is locked` and a
  non-zero exit
  ([#4](https://github.com/Davidjustice28/basaldb/issues/4)).
  `basald` holds that lock for its whole run and queues concurrent
  HTTP writes on the same branch.
- A single attribute can still be up to 4 GiB, and reading that record
  pulls the whole value into the cache.
- Open time still grows with the commit log. The 50 ms figure above is
  a checkpointed file whose log is empty.

On the [scalability roadmap (#42)](https://github.com/Davidjustice28/basaldb/issues/42),
a 10 GB file opens in under 50 ms. Still ahead: 10M records through the
normal ingest path, and indexed lookups under 50 µs when the page is
cold. Issues labeled
[`priority: high`](https://github.com/Davidjustice28/basaldb/labels/priority%3A%20high)
are the queue.

---

## What you can do today

| Capability | How | Example |
|---|---|---|
| Define a hierarchy | `basal init -layers` | `-layers "0:Accounts(name,plan),1:Teams(name),2:Users(name,email)"` |
| Create root records | `CREATE Layer [...]` | `CREATE Accounts [name="Acme Corp",plan="pro"]` |
| Create child records | `CREATE Layer PARENT Layer[filter] [...]` | `CREATE Teams PARENT Accounts[name="Acme Corp"] [name="Sales"]` |
| Walk down the tree | `->` between layers | `GET Accounts[id=1] -> Teams -> Users` |
| Filter | `=`, `eq ne gt gte lt lte` (AND only) | `GET Users[age=(gte=30),role=admin]` |
| Equality index | `email!` at init, or `CREATE INDEX` | `GET Users[email="sam@acme.io"]` |
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
| Write from several processes at once | one process holds `file.bsl.lock`; the other waits 30s, then exits with `database is locked` ([#4](https://github.com/Davidjustice28/basaldb/issues/4)) |
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

> `basald` holds the file lock for its whole run. A CLI `Open` of that
> same file waits 30 seconds, then exits with `database is locked`
> ([#4](https://github.com/Davidjustice28/basaldb/issues/4)).

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
  page cache (default 64MB, `-max-memory`) holds recently read pages.
  The id index is a mapped array. A secondary index stays resident.
  Blob attributes, stored out of line, are
  planned in
  [#50](https://github.com/Davidjustice28/basaldb/issues/50).
- **Use branches for speculative memory.** Let an agent write
  "hypotheses" on a branch and keep only what's confirmed.

**Current gaps for agent workloads:** one process holds the file lock,
so give each agent its own file or route writes through `basald`. A
second process gets `database is locked`
([#4](https://github.com/Davidjustice28/basaldb/issues/4)).
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

The [README](README.md#how-it-compares) has the general-purpose table.
These two are the closer calls: other ways to store agent memory, and
measured numbers against SQLite.

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

### Against SQLite, measured

Branches take about 30 µs at any size and store only the pages they
change; with SQLite you'd copy the file (63 ms at 1M records). Reading
a whole subtree in one query is 7–8× faster than the equivalent indexed
SQL join, up to a few hundred thousand records. For storage size,
memory, scans and bulk loading, SQLite is still ahead. See
[BENCHMARKS.md](BENCHMARKS.md#comparison-basaldb-vs-sqlite-2026-10-21).
