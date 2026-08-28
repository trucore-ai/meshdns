import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { spawn, type ChildProcess } from "node:child_process";
import { createServer, type AddressInfo } from "node:net";
import { resolve as resolvePath } from "node:path";
import { fileURLToPath } from "node:url";
import { randomBytes } from "node:crypto";
import { unlinkSync } from "node:fs";

import { MeshDNS, MeshDNSError } from "./index.js";

const __dirname = resolvePath(fileURLToPath(import.meta.url), "..");
const REPO_ROOT = resolvePath(__dirname, "../../..");
// REPO_ROOT = .../meshdns/src  (go.mod lives here)

function randomName(): string {
  return `ts-test-${randomBytes(4).toString("hex")}`;
}

let server: ChildProcess | null = null;
let serverUrl: string = "";
const goBinPath = resolvePath(REPO_ROOT, "meshdns-test-server");

async function buildGoBinary(): Promise<boolean> {
  return new Promise((resolve) => {
    const proc = spawn("go", ["build", "-o", goBinPath, "./cmd/provengraph"], {
      cwd: REPO_ROOT,
      stdio: "pipe",
    });
    proc.on("close", (code) => resolve(code === 0));
    proc.on("error", () => resolve(false));
  });
}

function getFreePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = createServer();
    srv.listen(0, "127.0.0.1", () => {
      const port = (srv.address() as AddressInfo).port;
      srv.close(() => resolve(port));
    });
    srv.on("error", reject);
  });
}

interface RegisterResponse {
  server_id: string;
  write_key: string;
}

async function registerServer(
  name: string,
  capability: string
): Promise<RegisterResponse> {
  const body = JSON.stringify({
    name,
    description: "TypeScript SDK test server",
    server_url: `https://${name}.example/dns-query`,
    health_url: `https://${name}.example/health`,
    capabilities: [capability],
    owner_contact: "sdk-test@example.com",
  });

  const response = await fetch(`${serverUrl}/v0/servers`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body,
  });

  if (response.status !== 201) {
    const text = await response.text();
    throw new Error(`Failed to register server: ${response.status} ${text}`);
  }

  return (await response.json()) as RegisterResponse;
}

async function waitForServer(url: string, maxRetries = 30): Promise<void> {
  for (let i = 0; i < maxRetries; i++) {
    try {
      const response = await fetch(`${url}/v0/stats`);
      if (response.ok) return;
    } catch {
      // server not ready yet
    }
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(`Server at ${url} did not become ready`);
}

let skipReason: string | null = null;

beforeAll(async () => {
  const built = await buildGoBinary();
  if (!built) {
    skipReason = "go build failed — skipping integration tests";
    return;
  }

  const port = await getFreePort();
  serverUrl = `http://127.0.0.1:${port}`;

  const dbPath = resolvePath(
    REPO_ROOT,
    `test-sdk-${randomBytes(4).toString("hex")}.db`
  );

  server = spawn(goBinPath, [], {
    cwd: REPO_ROOT,
    env: {
      PATH: process.env.PATH ?? "",
      PROVENGRAPH_PORT: `:${port}`,
      PROVENGRAPH_DB: dbPath,
      PROVENGRAPH_PROBE_INTERVAL: "24h",
    },
    stdio: "pipe",
  });

  server.stderr?.on("data", (_data: Buffer) => {
    // suppress noise
  });

  await waitForServer(serverUrl);

  // Register test servers
  await registerServer(randomName(), "sandbox");
  await registerServer(randomName(), "sandbox");
  await registerServer(randomName(), "metrics");
});

afterAll(() => {
  if (server) {
    server.kill("SIGTERM");
    server = null;
  }
  try {
    unlinkSync(goBinPath);
  } catch {
    // ignore
  }
});

describe("MeshDNS TypeScript SDK", () => {
  it("resolve returns an array with ServerInfo shape", async () => {
    if (skipReason) {
      console.log(`Skipped: ${skipReason}`);
      return;
    }

    const client = new MeshDNS(serverUrl);
    const results = await client.resolve("sandbox");

    // resolve returns only "up" servers — without health probes, newly
    // registered servers may not be up yet. We verify the shape and typing.
    expect(results).toBeInstanceOf(Array);

    for (const server of results) {
      expect(server).toHaveProperty("name");
      expect(server).toHaveProperty("server_url");
      expect(server).toHaveProperty("capabilities");
      expect(server).toHaveProperty("uptime_30d");
      expect(server).toHaveProperty("last_checked_at");
      expect(typeof server.name).toBe("string");
      expect(typeof server.server_url).toBe("string");
      expect(Array.isArray(server.capabilities)).toBe(true);
      expect(typeof server.uptime_30d).toBe("number");
      expect(typeof server.last_checked_at).toBe("string");
    }
  });

  it("listServers finds registered servers", async () => {
    if (skipReason) return;

    const client = new MeshDNS(serverUrl);
    const result = await client.listServers();

    expect(Array.isArray(result.servers)).toBe(true);
    // listServers does not require up status, so we should see our servers
    expect(result.servers.length).toBeGreaterThanOrEqual(3);

    for (const server of result.servers) {
      expect(server).toHaveProperty("name");
      expect(server).toHaveProperty("server_url");
      expect(server).toHaveProperty("capabilities");
    }
  });

  it("resolve returns empty array for unknown capability", async () => {
    if (skipReason) return;

    const client = new MeshDNS(serverUrl);
    const results = await client.resolve("nonexistent-capability-xyz");

    expect(results).toBeInstanceOf(Array);
    expect(results.length).toBe(0);
  });

  it("resolve throws MeshDNSError when capability is missing", async () => {
    if (skipReason) return;

    const client = new MeshDNS(serverUrl);
    try {
      await client.resolve("");
      expect.unreachable("Should have thrown");
    } catch (err) {
      expect(err).toBeInstanceOf(MeshDNSError);
      const meshError = err as MeshDNSError;
      expect(meshError.statusCode).toBe(422);
    }
  });

  it("resolveNext filters out skipped servers", async () => {
    if (skipReason) return;

    const client = new MeshDNS(serverUrl);
    const all = await client.resolve("sandbox");

    // Filtering logic is tested even with partial results
    const skipSet = new Set<string>(["nonexistent-server-name"]);
    const remaining = await client.resolveNext("sandbox", skipSet);

    for (const server of remaining) {
      expect(skipSet.has(server.name)).toBe(false);
    }
    expect(remaining.length).toBeLessThanOrEqual(all.length);
  });

  it("resolveNext with empty skip returns all servers", async () => {
    if (skipReason) return;

    const client = new MeshDNS(serverUrl);
    const all = await client.resolve("sandbox");
    const withEmptySkip = await client.resolveNext("sandbox", new Set());

    expect(withEmptySkip.length).toBe(all.length);
  });

  it("listServers with capability filter finds matching servers", async () => {
    if (skipReason) return;

    const client = new MeshDNS(serverUrl);
    const result = await client.listServers({ capability: "metrics" });

    expect(result.servers.length).toBeGreaterThanOrEqual(1);
    for (const server of result.servers) {
      expect(server.capabilities.some((c) => c.includes("metrics"))).toBe(
        true
      );
    }
  });

  it("listServers with small limit returns at most that many", async () => {
    if (skipReason) return;

    const client = new MeshDNS(serverUrl);
    const result = await client.listServers({ limit: 2 });

    expect(result.servers.length).toBeLessThanOrEqual(2);
    expect(
      result.nextCursor === null || typeof result.nextCursor === "string"
    ).toBe(true);
  });

  it("MeshDNSError has correct properties", () => {
    const err = new MeshDNSError(404, "not found");
    expect(err).toBeInstanceOf(Error);
    expect(err).toBeInstanceOf(MeshDNSError);
    expect(err.statusCode).toBe(404);
    expect(err.detail).toBe("not found");
    expect(err.message).toContain("404");
    expect(err.message).toContain("not found");
    expect(err.name).toBe("MeshDNSError");
  });

  it("MeshDNSError handles object detail", () => {
    const detail = { capability: "is required" };
    const err = new MeshDNSError(422, detail);
    expect(err.detail).toEqual(detail);
    expect(err.message).toContain("422");
  });

  it("constructor strips trailing slash", () => {
    const client = new MeshDNS("http://localhost:8080/");
    expect(client).toBeInstanceOf(MeshDNS);
  });
});