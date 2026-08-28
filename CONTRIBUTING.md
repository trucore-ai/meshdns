     1|# Contributing to MeshDNS
     2|
     3|Thanks for your interest! MeshDNS is an open-source MCP service registry — we welcome
     4|bug reports, feature ideas, and pull requests.
     5|
     6|## Getting started
     7|
     8|```bash
     9|git clone https://github.com/trucore-ai/provengraph.git
    10|cd provengraph
    11|go mod download
    12|```
    13|
    14|## Development
    15|
    16|```
    17|provengraph/
    18|├── cmd/provengraph/main.go       # Entrypoint
    19|├── internal/
    20|│   ├── api/                  # HTTP handlers (register, query, auth)
    21|│   ├── config/               # Env-var config
    22|│   ├── events/               # Append-only event log
    23|│   ├── health/               # Background probe worker pool
    24|│   ├── store/                # SQLite schema + data access
    25|│   └── web/                  # Embedded landing page
    26|├── sdk/python/               # Python client
    27|├── sdk/typescript/           # TypeScript client
    28|├── scripts/                  # Load probe + metrics rollup
    29|└── web/                      # Landing page HTML/CSS
    30|```
    31|
    32|## Running tests
    33|
    34|```bash
    35|# Go server tests (33 tests across 5 packages)
    36|go test ./...
    37|
    38|# Python SDK tests
    39|cd sdk/python && pip install -e . && pytest
    40|
    41|# TypeScript SDK tests
    42|cd sdk/typescript && npm test
    43|```
    44|
    45|## Commit convention
    46|
    47|We use [Conventional Commits](https://www.conventionalcommits.org/) with requirement
    48|traceability where applicable:
    49|
    50|- `feat(api): capability resolve endpoint [REQ-002]`
    51|- `fix(health): treat timeout as down [REQ-003]`
    52|- `docs(readme): add deploy section`
    53|
    54|## Pull requests
    55|
    56|- Open an issue or discussion first for anything bigger than a typo fix
    57|- Keep PRs focused — one concern per PR
    58|- Tests must pass (`go test ./...`, `go vet ./...`)
    59|- Update the README if your change affects the API or quickstart
    60|- Squash commits are fine — we squash-merge anyway
    61|
    62|## Where things go
    63|
    64|| What | Where |
    65||------|-------|
    66|| Bugs | [Issues](https://github.com/trucore-ai/provengraph/issues) |
    67|| Ideas / feedback | [Discussions](https://github.com/trucore-ai/provengraph/discussions) |
    68|| Security vulns | See [SECURITY.md](SECURITY.md) |
    69|
    70|## Code of Conduct
    71|
    72|This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md). Be respectful.