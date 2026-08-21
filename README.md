# MeshDNS

[![Go Version](https://img.shields.io/badge/Go-1.24-00ADD8?logo=go)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Docker](https://img.shields.io/badge/docker-ready-2496ED?logo=docker)](Dockerfile)
[![Discussions](https://img.shields.io/badge/Discussions-Welcome-6f42c1?logo=github)](https://github.com/trucore-ai/meshdns/discussions)
[![Status](https://img.shields.io/badge/status-live-brightgreen)](https://meshdns.onrender.com)

**MeshDNS — the service registry for AI agents. Never hardcode an MCP server again.**

MeshDNS is a lightweight, zero-dependency service registry that lets AI agents discover each other at runtime. MCP servers register their capabilities and health endpoints; agents query `resolve("capability")` to find the best available server. Think DNS, but for your agent mesh.

---

## Install

```bash
go install github.com/trucore-ai/meshdns/cmd/meshdns@latest
```

Or grab the pre-built binary from [releases](https://github.com/trucore-ai/meshdns/releases)
(linux-amd64, statically linked — no dependencies).

## Quickstart

Start MeshDNS (defaults to `:8080` with SQLite at `meshdns.db`):

```bash
go run . --port=:8080
```

Then copy-paste these three commands in sequence:

### 1. Register a server

```bash
curl -s -X POST http://localhost:8080/v0/servers \
  -H "Content-Type: application/json" \
  -d '{
    "name": "weather-agent",
    "description": "Weather data MCP server",
    "server_url": "https://weather.example.com",
    "health_url": "https://weather.example.com/health",
    "capabilities": ["weather", "forecast", "alerts"],
    "owner_contact": "ops@example.com"
  }'
# → {"server_id":"...","write_key":"..."}  — save the write_key!
```

### 2. Resolve a capability

```bash
curl -s "http://localhost:8080/v0/resolve?capability=weather"
# → [{"id":"...","name":"weather-agent","server_url":"...","capabilities":[...],"up":true,...}]
```

### 3. List all active servers

```bash
curl -s "http://localhost:8080/v0/servers?status=active"
# → {"servers":[{...}],"next_cursor":"..."}
```

---

## SDK Quickstart

### Python

```bash
pip install meshdns-client
```

```python
from meshdns_client import MeshDNSClient
client = MeshDNSClient("http://localhost:8080")
servers = client.resolve("weather")
```

### TypeScript

```bash
npm i @meshdns/client
```

```typescript
import { MeshDNSClient } from "@meshdns/client";
const client = new MeshDNSClient("http://localhost:8080");
const servers = await client.resolve("weather");
```

---

## API Reference

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v0/servers` | Register a new server. Returns `server_id` + `write_key`. |
| `GET` | `/v0/servers` | List servers. Query params: `status` (active/delisted/all), `query`, `capability`, `cursor`, `limit`. |
| `PUT` | `/v0/servers/{id}` | Update a server. Requires `Authorization: Bearer ***` header. |
| `DELETE` | `/v0/servers/{id}` | Delist (soft-delete) a server. Requires `Authorization: Bearer ***` header. |
| `GET` | `/v0/resolve` | Resolve servers by capability. Query param: `capability` (required). Returns only healthy servers. |
| `GET` | `/v0/stats` | Registry statistics: active/total servers, up count, resolutions and probes in the last 24h. |

Additional endpoints: `GET /` (landing page), `GET /v0/export` (full registry export).

---

## Architecture

MeshDNS ships as a **single static binary** with no external dependencies:

- **Go stdlib HTTP server** — no frameworks, no middleware stacks
- **SQLite** via `modernc.org/sqlite` — pure Go, no CGo, file-based storage
- **Background health pool** — configurable worker pool probes registered health URLs on a schedule and tracks 30-day uptime
- **Bearer token auth** — every registration returns a `write_key` used to authenticate update/delete operations
- **Zero infrastructure** — deploy the binary, point it at a volume for the DB, done

Config via environment variables: `MESHDNS_PORT`, `MESHDNS_DB`, `MESHDNS_PROBE_INTERVAL`, `MESHDNS_PROBE_TIMEOUT`, `MESHDNS_WORKERS`.

---

## Community

- 💬 [**Discussions**](https://github.com/trucore-ai/meshdns/discussions) — ideas, Q&A, show and tell
- 🐛 [**Issues**](https://github.com/trucore-ai/meshdns/issues) — bug reports
- 📖 [**Contributing**](CONTRIBUTING.md) — setup, tests, PR checklist
- 🔒 [**Security**](SECURITY.md) — vulnerability reporting

## License

MIT — see [LICENSE](LICENSE).

GitHub: [trucore-ai/meshdns](https://github.com/trucore-ai/meshdns)