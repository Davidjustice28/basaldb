# BasalDB

**One file. One database. Built for data that lives in hierarchies.**

BasalDB is a small embedded database for data that nests: accounts contain
teams, teams contain people, canvases contain frames, frames contain shapes.
It ships as a single portable file (`.bsl`), runs inside your process with no
server to install, and keeps a signed log of who changed what and why — like
git for your data.

```
Account "Acme"
├── Team "Engineering"
│   ├── User "Sam"
│   └── User "Elena"
└── Team "Sales"
    └── User "Ravi"
```

* **Delete once, gone completely.** Delete an account and every team, user,
  and setting underneath it disappears in one step — no matter how deep it
  goes. Nothing is left dangling.
* **Ask for a path, get an answer.** "Sam's account" is a direct walk down the
  tree: `GET Accounts[name="Acme"] -> Teams -> Users[name=Sam]`. No joins, no
  index tuning.
* **Every change is signed.** Each write appends a commit to an unreadable-by-
  accident log: what changed, who did it, and why. You can read the log with
  `cat`.
* **Branch your data like git.** Fork a database, try things, keep it or throw
  it away. Branches never interfere with each other.
* **It's just a file.** Copy it, email it, check it into git, hand one to a
  customer, keep one per tenant. No server, no driver, no setup.

## 60-second demo

```console
$ go build -o basal ./cmd/basal

$ basal init -file demo.bsl -layers "0:Accounts(name,date),1:Teams(name),2:Users(name,email)"
initialized demo.bsl

$ basal query -file demo.bsl 'CREATE Accounts [name="Acme Corp",date="2026-01-01"]'
created Accounts#0 (id=1)

$ basal query -file demo.bsl 'CREATE Teams PARENT Accounts[id=1] [name="Eng Team"]'
created Teams#0 (id=2)

$ basal query -file demo.bsl 'CREATE Users PARENT Teams[id=2] [name="Sam",email="sam@dev.io"]'
created Users#0 (id=3)

$ basal query -file demo.bsl 'GET Accounts[name="Acme Corp"] -> Teams -> Users'
layer | id | slot | parent | children | attrs
Users[3#0] parent=2 children=[] "Sam" "sam@dev.io"
1 record(s) on branch "main"

$ basal query -file demo.bsl 'DELETE Accounts[id=1]'
cascade deleted 3 record(s) under 1 root(s) in "Accounts"
```

Record writes don't need any ceremony. If you *want* to sign them, pass
`-author`, `-summary` and `-reason` and they land in the audit log.

The full command reference lives in [CLI.md](CLI.md) — quickstart, concepts,
the query language, branching, output formats, and the REST API served by
`basald`.

## How it compares

BasalDB is deliberately narrow. For most jobs, an established database is the
right call — this table is meant to help you pick honestly.

| Database | Best for | Pros | Cons |
|---|---|---|---|
| **BasalDB** | Deeply nested data where whole subtrees are created, read, or deleted together | Hierarchies are native (no joins); deleting a subtree is one step; built-in audit log and data branches; single readable file; tiny, zero-setup | Very new and unproven; no joins across unrelated data; no aggregations or SQL; writes to one file serialize (reads run in parallel); no drivers/ORM ecosystem |
| **SQLite** | General-purpose embedded storage; the default choice for most apps | Rock solid, everywhere, huge ecosystem, full SQL, battle-tested for decades | Flat tables — hierarchies need recursive queries; no built-in audit trail or branching |
| **Turso / libSQL** | Edge-hosted SQLite: replicas close to your users | SQL you know; managed replication and database forking; no server to operate | Hosted service (network + vendor dependency); still flat tables for hierarchies; small/young ecosystem |
| **DuckDB** | Analytics: scanning and aggregating large tables (OLAP) | Blazing fast analytical queries; embedded; reads Parquet/CSV directly; SQL | Built for analytics, not per-record app reads/writes; no hierarchy model; no audit trail |
| **PostgreSQL / MySQL** | Client-server apps with many concurrent writers | Full SQL, concurrent access over the network, mature tooling | Needs a server and operations; hierarchies are awkward (recursive CTEs, closure tables); no per-tenant file |
| **MongoDB / document stores** | Flexible, schema-light documents | Schemaless flexibility; nested documents feel natural | Whole-document rewrites; queries cut across documents poorly; server to run; no audit trail by default |
| **Neo4j / graph databases** | Many-hop relationships in any direction (social graphs, recommendations) | Relationships are first-class; arbitrary-depth traversal | Heavier footprint and a server; overkill for strict parent-child trees; smaller ecosystem |
| **RocksDB / LevelDB** | Raw embedded key-value speed | Extremely fast simple get/put | No structure — you design keys and rebuild meaning yourself; no query language |

Rule of thumb: **hierarchies that get deleted or handed around as a unit →
BasalDB. Almost everything else → SQLite or PostgreSQL; number crunching →
DuckDB.**

## Where BasalDB fits

* **Multi-tenant SaaS** — one customer, one file. Exporting a customer's data
  is a file copy. Deleting one is deleting one file. Offboarding is exact:
  nothing hides in shared tables.
* **Design and canvas tools** — pages hold frames, frames hold shapes.
  Deleting a frame takes its fifty children with it, instantly, and the undo
  history is the commit log.
* **Game state and servers** — worlds contain zones, zones contain entities.
  Save files are single files; per-player data can live in its own file.
* **Org charts, family trees, group membership** — any "reports to" data
  reads as a path, and removing a manager removes their whole branch of the
  org (or rehomes it) in one statement.
* **Bills of materials and nested configurations** — a product contains
  assemblies contains parts; change or delete at any level with the subtree
  rules applied consistently.
* **Audited state and compliance-sensitive records** — every change carries
  who/what/why, append-only, human-readable with `cat`. Great for settings,
  entitlements, and permission trees where "who changed this?" must always
  have an answer.
* **Offline-first and edge apps** — no server process, no port, no
  credentials; the whole database syncs as one artifact and opens instantly.
* **Preview and staging environments for data** — branch production, seed a
  test scenario, throw the branch away. Same idea as a git worktree, for data.

## When not to use BasalDB

* You need ad-hoc joins across unrelated entities → **SQLite/PostgreSQL**
* You need heavy aggregations or analytics → **DuckDB**
* Many processes writing to the same database at once → **PostgreSQL** (Basal
  serializes writes to one file; reads still run in parallel)
* You need a mature ecosystem of drivers, ORMs, and GUI tools today →
  almost anything else

BasalDB is young. It does one thing — nested data, stored plainly, deleted
cleanly, with a paper trail — and it is not trying to be a general SQL
database.

## How the file works

A `.bsl` file has three parts:

1. **A readable header** — the file signature and your layer definitions.
   Open it in any text editor.
2. **A readable commit log** — every change ever made, in plain text, with
   its author, summary, and reason.
3. **Binary data pages** — the records themselves, laid out so that walking a
   hierarchy is pointer arithmetic instead of searching.
