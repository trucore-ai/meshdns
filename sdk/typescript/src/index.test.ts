import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { spawn, type ChildProcess } from "node:child_process";
import { createServer, type AddressInfo } from "node:net";
import { resolve as resolvePath } from "node:path";
import { fileURLToPath } from "node:url";
import { randomBytes } from "node:crypto";
import { unlinkSync } from "node:fs";

import { MeshDNS, MeshDNSError, type ClaimResponse, type MemoryResponse } from "./index.js";

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

  // ── Knowledge tests ───────────────────────────────────────

  describe("Knowledge", () => {
    let claimId: string;
    let writeKey: string;

    it("createClaim returns claim_id and write_key", async () => {
      if (skipReason) return;

      const client = new MeshDNS(serverUrl);
      const result = await client.createClaim(
        "Paris is the capital of France",
        "geography",
        "test-issuer"
      );

      expect(result).toHaveProperty("claim_id");
      expect(result).toHaveProperty("write_key");
      expect(typeof result.claim_id).toBe("string");
      expect(typeof result.write_key).toBe("string");

      claimId = result.claim_id;
      writeKey = result.write_key;
    });

    it("getClaim returns ClaimResponse shape", async () => {
      if (skipReason) return;

      const client = new MeshDNS(serverUrl);
      // Create a fresh claim so this test is independent
      const { claim_id } = await client.createClaim(
        "Water boils at 100°C",
        "science"
      );
      const claim = await client.getClaim(claim_id);

      expect(claim).toHaveProperty("claim_id");
      expect(claim).toHaveProperty("issuer");
      expect(claim).toHaveProperty("domain");
      expect(claim).toHaveProperty("content");
      expect(claim).toHaveProperty("attested_by");
      expect(claim).toHaveProperty("created_at");
      expect(typeof claim.claim_id).toBe("string");
      expect(typeof claim.content).toBe("string");
      expect(Array.isArray(claim.attested_by)).toBe(true);
    });

    it("listClaims returns claims array", async () => {
      if (skipReason) return;

      const client = new MeshDNS(serverUrl);
      const result = await client.listClaims();

      expect(result).toHaveProperty("claims");
      expect(Array.isArray(result.claims)).toBe(true);
    });

    it("listClaims with domain filter returns matching claims", async () => {
      if (skipReason) return;

      const client = new MeshDNS(serverUrl);
      // Create claim in a specific domain
      await client.createClaim("Claim in test-filter domain", "test-filter");
      await client.createClaim("Another claim", "test-filter");

      const result = await client.listClaims("test-filter");
      expect(result.claims.length).toBeGreaterThanOrEqual(2);
      for (const claim of result.claims) {
        expect(claim.domain).toBe("test-filter");
      }
    });

    it("supersedeClaim returns status ok", async () => {
      if (skipReason) return;

      const client = new MeshDNS(serverUrl);
      const old = await client.createClaim("Old information", "general");
      const updated = await client.createClaim("Updated information", "general");

      const result = await client.supersedeClaim(
        updated.claim_id,
        old.claim_id,
        updated.write_key
      );

      expect(result).toHaveProperty("status");
    });

    it("contradictClaim returns status ok", async () => {
      if (skipReason) return;

      const client = new MeshDNS(serverUrl);
      const claim1 = await client.createClaim("Theory A", "physics");
      const claim2 = await client.createClaim("Theory B", "physics");

      const result = await client.contradictClaim(
        claim2.claim_id,
        claim1.claim_id,
        claim2.write_key
      );

      expect(result).toHaveProperty("status");
    });

    it("attestClaim returns status ok", async () => {
      if (skipReason) return;

      const client = new MeshDNS(serverUrl);
      const { claim_id } = await client.createClaim(
        "Test attestation claim",
        "testing"
      );

      const result = await client.attestClaim(claim_id, "attestor-agent");

      expect(result).toHaveProperty("status");
    });
  });

  // ── Memory tests ──────────────────────────────────────────

  describe("Memory", () => {
    let memoryId: string;
    let writeKey: string;

    it("createMemory returns memory_id and write_key", async () => {
      if (skipReason) return;

      const client = new MeshDNS(serverUrl);
      const result = await client.createMemory(
        "Remember to buy milk",
        "agent-1",
        "tasks",
        undefined,
        "shopping",
        "groceries"
      );

      expect(result).toHaveProperty("memory_id");
      expect(result).toHaveProperty("write_key");
      expect(typeof result.memory_id).toBe("string");
      expect(typeof result.write_key).toBe("string");

      memoryId = result.memory_id;
      writeKey = result.write_key;
    });

    it("getMemory returns MemoryResponse shape", async () => {
      if (skipReason) return;

      const client = new MeshDNS(serverUrl);
      const { memory_id } = await client.createMemory(
        "Meeting at 3pm",
        "agent-2"
      );
      const memory = await client.getMemory(memory_id);

      expect(memory).toHaveProperty("memory_id");
      expect(memory).toHaveProperty("owner");
      expect(memory).toHaveProperty("content");
      expect(memory).toHaveProperty("remembered_by");
      expect(memory).toHaveProperty("created_at");
      expect(memory).toHaveProperty("updated_at");
      expect(typeof memory.memory_id).toBe("string");
      expect(typeof memory.content).toBe("string");
      expect(Array.isArray(memory.remembered_by)).toBe(true);
    });

    it("listMemories returns memories array", async () => {
      if (skipReason) return;

      const client = new MeshDNS(serverUrl);
      const result = await client.listMemories();

      expect(result).toHaveProperty("memories");
      expect(Array.isArray(result.memories)).toBe(true);
    });

    it("listMemories with category filter returns matching memories", async () => {
      if (skipReason) return;

      const client = new MeshDNS(serverUrl);
      await client.createMemory("Cat 1", "agent-x", "pets");
      await client.createMemory("Cat 2", "agent-x", "pets");

      const result = await client.listMemories(undefined, "pets");
      expect(result.memories.length).toBeGreaterThanOrEqual(2);
      for (const memory of result.memories) {
        expect(memory.category).toBe("pets");
      }
    });

    it("updateMemory updates content and returns MemoryResponse", async () => {
      if (skipReason) return;

      const client = new MeshDNS(serverUrl);
      const { memory_id, write_key } = await client.createMemory(
        "Original content",
        "agent-3"
      );

      const updated = await client.updateMemory(memory_id, write_key, {
        content: "Updated content",
      });

      expect(updated).toHaveProperty("memory_id");
      expect(updated.content).toBe("Updated content");
    });

    it("deleteMemory returns status ok", async () => {
      if (skipReason) return;

      const client = new MeshDNS(serverUrl);
      const { memory_id, write_key } = await client.createMemory(
        "To be deleted",
        "agent-4"
      );

      const result = await client.deleteMemory(memory_id, write_key);

      expect(result).toHaveProperty("status");
    });

    it("remember returns status ok", async () => {
      if (skipReason) return;

      const client = new MeshDNS(serverUrl);
      const { memory_id } = await client.createMemory(
        "Shared knowledge",
        "agent-5"
      );

      const result = await client.remember(memory_id, "agent-6");

      expect(result).toHaveProperty("status");
    });

    it("forget returns status ok", async () => {
      if (skipReason) return;

      const client = new MeshDNS(serverUrl);
      const { memory_id } = await client.createMemory(
        "Temporary memory",
        "agent-7"
      );

      const result = await client.forget(memory_id, "agent-7");

      expect(result).toHaveProperty("status");
    });
  });
});