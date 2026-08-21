# Contributing to MeshDNS

Thanks for your interest! MeshDNS is an open-source MCP service registry — we welcome
bug reports, feature ideas, and pull requests.

## Getting started

```bash
git clone https://github.com/trucore-ai/meshdns.git
cd meshdns
go mod download
```

## Development

```
meshdns/
├── cmd/meshdns/main.go       # Entrypoint
├── internal/
│   ├── api/                  # HTTP handlers (register, query, auth)
│   ├── config/               # Env-var config
│   ├── events/               # Append-only event log
│   ├── health/               # Background probe worker pool
│   ├── store/                # SQLite schema + data access
│   └── web/                  # Embedded landing page
├── sdk/python/               # Python client
├── sdk/typescript/           # TypeScript client
├── scripts/                  # Load probe + metrics rollup
└── web/                      # Landing page HTML/CSS
```

## Running tests

```bash
# Go server tests (33 tests across 5 packages)
go test ./...

# Python SDK tests
cd sdk/python && pip install -e . && pytest

# TypeScript SDK tests
cd sdk/typescript && npm test
```

## Commit convention

We use [Conventional Commits](https://www.conventionalcommits.org/) with requirement
traceability where applicable:

- `feat(api): capability resolve endpoint [REQ-002]`
- `fix(health): treat timeout as down [REQ-003]`
- `docs(readme): add deploy section`

## Pull requests

- Open an issue or discussion first for anything bigger than a typo fix
- Keep PRs focused — one concern per PR
- Tests must pass (`go test ./...`, `go vet ./...`)
- Update the README if your change affects the API or quickstart
- Squash commits are fine — we squash-merge anyway

## Where things go

| What | Where |
|------|-------|
| Bugs | [Issues](https://github.com/trucore-ai/meshdns/issues) |
| Ideas / feedback | [Discussions](https://github.com/trucore-ai/meshdns/discussions) |
| Security vulns | See [SECURITY.md](SECURITY.md) |

## Code of Conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md). Be respectful.