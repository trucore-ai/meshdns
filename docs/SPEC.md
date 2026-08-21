# MeshDNS — Public Specification

**Version:** 0.1.0
**Repository:** [github.com/trucore-ai/meshdns](https://github.com/trucore-ai/meshdns)
**License:** MIT

---

## Summary

MeshDNS is the service registry for AI agents. It provides runtime discovery so agents never need to hardcode MCP server URLs. Servers register their capabilities and health endpoints; agents query `resolve("capability")` to get the best available server. Think DNS, but for your agent mesh.

---

## MVP Features

- **Server registration** — MCP servers register with a name, URL, health URL, capabilities, and owner contact. Each registration returns a unique `server_id` and a `write_key` for authenticated updates.
- **Capability resolution** — Agents resolve a capability string (e.g., `"weather"`, `"chat"`) to a list of healthy servers advertising that capability.
- **Server listing** — Paginated listing with filtering by name, capability, and status (active, delisted, or all).
- **Authenticated updates** — Bearer token auth protects update and delete operations; the `write_key` returned at registration is required.
- **Background health checking** — A configurable worker pool probes registered health URLs. Results feed into server status (`up`/`down`) and 30-day uptime tracking. Servers without a health URL are treated as healthy.
- **Registry statistics** — `/v0/stats` exposes active/total server counts, up counts, and 24h resolution/probe event counts.
- **Export** — `/v0/export` provides a full JSON dump of the registry for backups or offline analysis.

---

## API Reference

Base URL: `http://localhost:8080`

All request and response bodies are JSON (`Content-Type: application/json`). Errors follow the format:

```json
{"error": {"code": "validation_failed", "detail": {"name": "must match ..."}}}
```

### 1. Register Server

```
POST /v0/servers
```

**Request body:**

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `name` | string | yes | `^[a-z0-9][a-z0-9-]{1,63}$` |
| `description` | string | no | Free-text description |
| `server_url` | string | yes | Valid `http` or `https` URL |
| `health_url` | string | no | Valid `http` or `https` URL |
| `capabilities` | string[] | yes | 1–20 items, max 128 chars each |
| `owner_contact` | string | no | Max 200 characters |

**Response (201 Created):**

```json
{"server_id": "...", "write_key": "..."}
```

---

### 2. List Servers

```
GET /v0/servers
```

**Query parameters:**

| Param | Type | Default | Notes |
|-------|------|---------|-------|
| `status` | string | `active` | `active`, `delisted`, or `all` |
| `query` | string | — | Free-text search across name |
| `capability` | string | — | Filter by exact capability |
| `cursor` | string | — | Pagination cursor |
| `limit` | integer | `20` | Max `100` |

**Response (200 OK):**

```json
{
  "servers": [
    {
      "id": "...",
      "name": "...",
      "description": "...",
      "server_url": "...",
      "health_url": "...",
      "capabilities": ["..."],
      "status": "active",
      "up": true,
      "last_checked_at": "2026-01-01T00:00:00Z",
      "uptime_30d": 0.995,
      "created_at": "2026-01-01T00:00:00Z",
      "updated_at": "2026-01-01T00:00:00Z"
    }
  ],
  "next_cursor": "..."
}
```

---

### 3. Update Server

```
PUT /v0/servers/{id}
Authorization: Bearer ***
```

**Request body (all fields optional):**

| Field | Type |
|-------|------|
| `description` | string |
| `server_url` | string |
| `health_url` | string |
| `owner_contact` | string |
| `capabilities` | string[] |

**Response (200 OK):** Full server object (same shape as list item).

---

### 4. Delete (Delist) Server

```
DELETE /v0/servers/{id}
Authorization: Bearer ***
```

Soft-deletes the server (status → `delisted`). It no longer appears in default listings or resolution results.

**Response (200 OK):**

```json
{"ok": true}
```

---

### 5. Resolve Capability

```
GET /v0/resolve?capability=***
```

Returns all **active and healthy** servers advertising the exact capability. Results are sorted by `name`.

**Response (200 OK):** Array of server objects (same shape as list item).

**Events:** Each resolution is logged for metrics (visible in `/v0/stats`).

---

### 6. Registry Statistics

```
GET /v0/stats
```

**Response (200 OK):**

```json
{
  "servers_active": 42,
  "servers_total": 50,
  "up_count": 38,
  "resolutions_24h": 1523,
  "probes_24h": 4200
}
```

---

### Additional Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/` | Landing page (HTML) |
| `GET` | `/v0/export` | Full registry export (all servers) |

---

## SDKs

### Python — `meshdns-client`

```bash
pip install meshdns-client
```

```python
from meshdns_client import MeshDNSClient
client = MeshDNSClient("http://localhost:8080")
servers = client.resolve("weather")
# Each server: .id, .name, .server_url, .capabilities, .up, .uptime_30d, ...
```

Built on `httpx`. Python ≥ 3.10.

### TypeScript — `@meshdns/client`

```bash
npm i @meshdns/client
```

```typescript
import { MeshDNSClient } from "@meshdns/client";
const client = new MeshDNSClient("http://localhost:8080");
const servers = await client.resolve("weather");
```

Zero runtime dependencies (native `fetch`). ESM only.

---

## Architecture

MeshDNS is a **single static binary** with zero infrastructure requirements:

| Layer | Technology |
|-------|------------|
| HTTP server | Go stdlib `net/http` (Go 1.24+) |
| Database | SQLite via `modernc.org/sqlite` (pure Go, no CGo) |
| Health pool | Configurable background worker pool with jittered scheduling |
| Auth | SHA-256 hashed Bearer tokens for write operations |
| Storage | Single-file `meshdns.db` (WAL mode) |

**Configuration** via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `MESHDNS_PORT` | `:8080` | Listen address |
| `MESHDNS_DB` | `meshdns.db` | SQLite database path |
| `MESHDNS_PROBE_INTERVAL` | `60s` | Health check schedule interval |
| `MESHDNS_PROBE_TIMEOUT` | `5s` | Per-probe HTTP timeout |
| `MESHDNS_WORKERS` | `8` | Concurrent health probe workers |

---

## Metrics & Observability

Each API event (register, update, delist, resolve, probe) is logged to an internal events table. The `/v0/stats` endpoint surfaces:

- Active and total server counts
- Healthy (up) server count
- Resolution count in the last 24 hours
- Health probe count in the last 24 hours

Server-level health metrics:
- `up` — boolean, reflects most recent probe result
- `last_checked_at` — RFC 3339 timestamp of last probe
- `uptime_30d` — rolling 30-day uptime ratio (0.0–1.0)

---

## License

MIT. See [LICENSE](../LICENSE) for full terms.