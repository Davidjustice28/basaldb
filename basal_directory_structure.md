# BasalDB Directory Structure

```text
basaldb/
├── cmd/
│   ├── basal/          # CLI utility (basal init, basal inspect)
│   └── basald/         # BasalDB HTTP Daemon server
├── pkg/
│   ├── engine/         # Slotted page binary engine & .bsl file handler
│   ├── bql/            # BasalQL parser and query executor
│   └── api/            # REST API router and handlers
├── web/                # Embedded web console dashboard
├── docs/               # Architecture docs and spec
├── Dockerfile          # Multi-stage Docker build
├── go.mod              # Go module file
└── README.md           # Main repository documentation
```
