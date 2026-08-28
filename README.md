     1|# ProvenGraph Trust
     2|
     3|> The provenance graph for the agent economy — trust scores for MCP servers, computed over a shared provenance graph. Formerly MeshDNS.
     4|
     5|[![Go Version](https://img.shields.io/badge/Go-1.24-00ADD8?logo=go)](https://go.dev)
     6|[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
     7|[![Docker](https://img.shields.io/badge/docker-ready-2496ED?logo=docker)](Dockerfile)
     8|[![Discussions](https://img.shields.io/badge/Discussions-Welcome-6f42c1?logo=github)](https://github.com/trucore-ai/provengraph/discussions)
     9|[![Status](https://img.shields.io/badge/status-live-brightgreen)](https://provengraph.trucore.xyz)
    10|
    11|**ProvenGraph Trust — the trust layer for the agent economy. Three product lines (Trust, Knowledge, Memory) on one provenance graph.**
    12|
    13|ProvenGraph Trust is a provenance-graph registry for the agent economy. Servers register their capabilities and health endpoints; agents query `resolve("capability")` to find the best verified server with trust scores.
    14|
    15|---
    16|
    17|## Install
    18|
    19|```bash
    20|go install github.com/trucore-ai/provengraph/cmd/provengraph@latest
    21|```
    22|
    23|Or grab the pre-built binary from [releases](https://github.com/trucore-ai/provengraph/releases)
    24|(linux-amd64, statically linked — no dependencies).
    25|
    26|## Quickstart
    27|
    28|Start ProvenGraph (defaults to `:8080` with SQLite at `provengraph.db`):
    29|
    30|```bash
    31|provengraph serve
    32|```
    33|
    34|Then copy-paste these three commands in sequence:
    35|
    36|### 1. Register a server
    37|
    38|```bash
    39|curl -s -X POST http://localhost:8080/v0/servers \
    40|  -H "Content-Type: application/json" \
    41|  -d '{
    42|    "name": "weather-agent",
    43|    "description": "Weather data MCP server",
    44|    "server_url": "https://weather.example.com",
    45|    "health_url": "https://weather.example.com/health",
    46|    "capabilities": ["weather", "forecast", "alerts"],
    47|    "owner_contact": "ops@example.com"
    48|  }'
    49|# → {"server_id":"...","write_key":"..."}  — save the write_key!
    50|```
    51|
    52|**POST-only endpoints (MCP streamable-HTTP):** add `"probe_method": "POST"` to
    53|probe with an MCP `initialize` request instead of GET. Leave it out to
    54|auto-detect: if GET answers 405, ProvenGraph tries the POST probe and remembers
    55|the switch automatically.
    56|
    57|### 2. Resolve a capability
    58|
    59|```bash
    60|curl -s "http://localhost:8080/v0/resolve?capability=weather"
    61|# → [{"id":"...","name":"weather-agent","server_url":"...","capabilities":[...],"up":true,...}]
    62|```
    63|
    64|### 3. List all active servers
    65|
    66|```bash
    67|curl -s "http://localhost:8080/v0/servers?status=active"
    68|# → {"servers":[{...}],"next_cursor":"..."}
    69|```
    70|
    71|---
    72|
    73|## SDK Quickstart
    74|
    75|### Python
    76|
    77|```bash
    78|pip install meshdns-client
    79|```
    80|
    81|```python
    82|from meshdns_client import MeshDNSClient
    83|client = MeshDNSClient("http://localhost:8080")
    84|servers = client.resolve("weather")
    85|```
    86|
    87|### TypeScript
    88|
    89|```bash
    90|npm i @meshdns/client
    91|```
    92|
    93|```typescript
    94|import { MeshDNSClient } from "@meshdns/client";
    95|const client = new MeshDNSClient("http://localhost:8080");
    96|const servers = await client.resolve("weather");
    97|```
    98|
    99|---
   100|
   101|## API Reference
   102|
   103|| Method | Path | Description |
   104||--------|------|-------------|
   105|| `POST` | `/v0/servers` | Register a new server. Returns `server_id` + `write_key`. |
   106|| `GET` | `/v0/servers` | List servers. Query params: `status` (active/delisted/all), `query`, `capability`, `cursor`, `limit`. |
   107|| `PUT` | `/v0/servers/{id}` | Update a server. Requires `Authorization: Bearer *** header. |
   108|| `DELETE` | `/v0/servers/{id}` | Delist (soft-delete) a server. Requires `Authorization: Bearer *** header. |
   109|| `GET` | `/v0/resolve` | Resolve servers by capability. Query param: `capability` (required). Returns only healthy servers. |
   110|| `GET` | `/v0/stats` | Registry statistics: active/total servers, up count, resolutions and probes in the last 24h. |
   111|
   112|Additional endpoints: `GET /` (landing page), `GET /v0/export` (full registry export), `GET /llms.txt` (LLM-readable API reference).
   113|
   114|---
   115|
   116|## Catalog Sources
   117|
   118|ProvenGraph ingests from multiple MCP server registries:
   119|
   120|| Source | Servers | Notes |
   121||--------|---------|-------|
   122|| [MCP Official Registry](https://registry.modelcontextprotocol.io) | 5,437 | Probed for health — GET + POST auto-detect |
   123|| [Smithery](https://registry.smithery.ai) | 114 | Deployment URLs resolved via parallel detail API, tools pre-discovered |
   124|| [npm MCP packages](https://npmjs.com) | 240 | Keyword-matched, GitHub repo URLs, declared healthy |
   125|
   126|All sources synced via `catalog_sync.py` in `~/repo/ventures/meshdns/scripts/`. Re-run with `python3 catalog_sync.py all all`.
   127|
   128|---
   129|
   130|## Health Checks
   131|
   132|ProvenGraph probes registered servers every 60s (configurable via `PROVENGRAPH_PROBE_INTERVAL`). The probe logic:
   133|
   134|1. **GET by default.** If the server answers 2xx, it's marked UP.
   135|2. **Auto-detect POST-only.** If GET fails with 405, any 4xx error, or a transport error, ProvenGraph retries with a `POST` MCP `initialize` request. If that succeeds, the server is marked UP and the method switch is persisted — future probes go straight to POST.
   136|3. **Explicit `probe_method: "POST"`** skips the GET attempt entirely. Set this when registering servers known to be POST-only (e.g., streamable-HTTP MCP endpoints).
   137|4. **5s timeout.** Non-2xx after POST retry → DOWN.
   138|5. **`/v0/resolve` never returns DOWN servers.**
   139|
   140|The POST probe sends a standards-compliant MCP initialize:
   141|
   142|```json
   143|{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"provengraph-health","version":"1.0.0"}}}
   144|```
   145|
   146|Servers without a `health_url` are declared healthy by default. 30-day uptime is tracked per server and used for ranking in resolve results.
   147|
   148|---
   149|
   150|## Architecture
   151|
   152|ProvenGraph ships as a **single static binary** with no external dependencies:
   153|
   154|- **Go stdlib HTTP server** — no frameworks, no middleware stacks
   155|- **SQLite** via `modernc.org/sqlite` — pure Go, no CGo, file-based storage
   156|- **Background health pool** — configurable worker pool probes registered health URLs on a schedule and tracks 30-day uptime. Automatic POST probe detection: if GET returns 405, any 4xx, or a transport error, ProvenGraph retries with a `POST` MCP `initialize` request and persists the switch. This discovers streamable-HTTP MCP endpoints that don't answer GET.
   157|- **Bearer token auth** — every registration returns a `write_key` used to authenticate update/delete operations
   158|- **Zero infrastructure** — deploy the binary, point it at a volume for the DB, done
   159|
   160|Config via environment variables: `PROVENGRAPH_PORT`, `PROVENGRAPH_DB`, `PROVENGRAPH_PROBE_INTERVAL`, `PROVENGRAPH_PROBE_TIMEOUT`, `PROVENGRAPH_WORKERS`.
   161|
   162|---
   163|
   164|## Community
   165|
   166|- 💬 [**Discussions**](https://github.com/trucore-ai/provengraph/discussions) — ideas, Q&A, show and tell
   167|- 🐛 [**Issues**](https://github.com/trucore-ai/provengraph/issues) — bug reports
   168|- 📖 [**Contributing**](CONTRIBUTING.md) — setup, tests, PR checklist
   169|- 🔒 [**Security**](SECURITY.md) — vulnerability reporting
   170|
   171|## License
   172|
   173|MIT — see [LICENSE](LICENSE).
   174|
   175|GitHub: [trucore-ai/provengraph](https://github.com/trucore-ai/provengraph)