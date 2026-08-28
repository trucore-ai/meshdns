# @meshdns/client

TypeScript client for [MeshDNS](https://github.com/trucore-ai/provengraph) — resolve mesh DNS servers by capability.

## Quickstart

```bash
npm install @meshdns/client
```

```typescript
import { MeshDNS } from "@meshdns/client";

const client = new MeshDNS("http://localhost:8080");

// Resolve servers for a capability
const servers = await client.resolve("sandbox");

// Use resolveNext to iterate with skip set
const next = await client.resolveNext("sandbox", new Set(["primary"]));

// List servers with filters
const { servers, nextCursor } = await client.listServers({ capability: "dns", limit: 10 });
```

## API

### `new MeshDNS(baseUrl: string)`

Create a client pointed at a MeshDNS server.

### `client.resolve(capability: string): Promise<ServerInfo[]>`

Get active (up) servers that provide the given capability, ordered by uptime.

### `client.resolveNext(capability: string, skip: Set<string>): Promise<ServerInfo[]>`

Like `resolve`, but excludes servers whose name matches an entry in the `skip` set.

### `client.listServers(opts?): Promise<{servers: ServerInfo[], nextCursor: string | null}>`

List servers with optional query, capability, status, cursor, and limit filters. Use `nextCursor` for pagination.

### `ServerInfo`

```typescript
interface ServerInfo {
  name: string;
  server_url: string;
  capabilities: string[];
  uptime_30d: number;
  last_checked_at: string;
}
```

### `MeshDNSError`

Thrown for non-2xx responses. Has `statusCode` and `detail` properties.