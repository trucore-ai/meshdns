export interface ServerInfo {
  name: string;
  server_url: string;
  capabilities: string[];
  uptime_30d: number;
  last_checked_at: string;
}

interface ServerListResponse {
  servers: ServerInfo[];
  next_cursor: string;
}

interface ErrorResponse {
  error: {
    code: string;
    detail: string | Record<string, string>;
  };
}

const DEFAULT_TIMEOUT_MS = 5000;

export class MeshDNSError extends Error {
  statusCode: number;
  detail: string | Record<string, string>;

  constructor(statusCode: number, detail: string | Record<string, string>) {
    const detailStr = typeof detail === "string" ? detail : JSON.stringify(detail);
    super(`MeshDNS error ${statusCode}: ${detailStr}`);
    this.name = "MeshDNSError";
    this.statusCode = statusCode;
    this.detail = detail;
  }
}

function stripTrailingSlash(url: string): string {
  return url.endsWith("/") ? url.slice(0, -1) : url;
}

async function fetchWithTimeout(
  url: string,
  options: RequestInit = {},
  timeoutMs: number = DEFAULT_TIMEOUT_MS
): Promise<Response> {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), timeoutMs);

  try {
    const response = await fetch(url, {
      ...options,
      signal: controller.signal,
    });
    return response;
  } finally {
    clearTimeout(timeout);
  }
}

async function checkError(response: Response): Promise<void> {
  if (!response.ok) {
    let detail: string | Record<string, string> = response.statusText;
    try {
      const body = (await response.json()) as ErrorResponse;
      if (body?.error?.detail) {
        detail = body.error.detail;
      }
    } catch {
      // use status text if body is not JSON
    }
    throw new MeshDNSError(response.status, detail);
  }
}

// ── Knowledge ──────────────────────────────────────────────

export interface ClaimResponse {
  claim_id: string;
  issuer: string;
  domain: string;
  content: string;
  supersedes?: string;
  contradicts?: string;
  attested_by: string[];
  created_at: string;
}

// ── Memory ─────────────────────────────────────────────────

export interface MemoryResponse {
  memory_id: string;
  owner: string;
  category?: string;
  content: string;
  retention?: string;
  purpose?: string;
  subject?: string;
  remembered_by: string[];
  created_at: string;
  updated_at: string;
}

// ── Client ─────────────────────────────────────────────────

export class MeshDNS {
  private baseUrl: string;

  constructor(baseUrl: string) {
    this.baseUrl = stripTrailingSlash(baseUrl);
  }

  /**
   * Resolve servers that provide the given capability.
   * Returns active (up) servers ordered by uptime descending.
   */
  async resolve(capability: string): Promise<ServerInfo[]> {
    const url = `${this.baseUrl}/v0/resolve?capability=${encodeURIComponent(capability)}`;
    const response = await fetchWithTimeout(url);
    await checkError(response);
    return (await response.json()) as ServerInfo[];
  }

  /**
   * Resolve servers for a capability, excluding any whose name is in the skip set.
   * Useful for iterating through fallback servers.
   */
  async resolveNext(
    capability: string,
    skip: Set<string>
  ): Promise<ServerInfo[]> {
    const all = await this.resolve(capability);
    return all.filter((s) => !skip.has(s.name));
  }

  /**
   * List servers with optional filters and cursor-based pagination.
   */
  async listServers(opts?: {
    query?: string;
    capability?: string;
    status?: string;
    cursor?: string;
    limit?: number;
  }): Promise<{ servers: ServerInfo[]; nextCursor: string | null }> {
    const params = new URLSearchParams();
    if (opts?.query) params.set("query", opts.query);
    if (opts?.capability) params.set("capability", opts.capability);
    if (opts?.status) params.set("status", opts.status);
    if (opts?.cursor) params.set("cursor", opts.cursor);
    if (opts?.limit != null) params.set("limit", String(opts.limit));

    const qs = params.toString();
    const url = qs
      ? `${this.baseUrl}/v0/servers?${qs}`
      : `${this.baseUrl}/v0/servers`;

    const response = await fetchWithTimeout(url);
    await checkError(response);
    const body = (await response.json()) as ServerListResponse;
    return {
      servers: body.servers,
      nextCursor: body.next_cursor || null,
    };
  }

  // ── Knowledge methods ─────────────────────────────────────

  /**
   * Create a new knowledge claim.
   * Returns the claim_id and a write_key needed for updates/relations.
   */
  async createClaim(
    content: string,
    domain: string,
    issuer?: string
  ): Promise<{ claim_id: string; write_key: string }> {
    const body: Record<string, string> = { content, domain };
    if (issuer) body.issuer = issuer;

    const response = await fetchWithTimeout(`${this.baseUrl}/v0/knowledge`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    await checkError(response);
    return (await response.json()) as { claim_id: string; write_key: string };
  }

  /** Fetch a single claim by ID. */
  async getClaim(claimId: string): Promise<ClaimResponse> {
    const url = `${this.baseUrl}/v0/knowledge/${encodeURIComponent(claimId)}`;
    const response = await fetchWithTimeout(url);
    await checkError(response);
    return (await response.json()) as ClaimResponse;
  }

  /** List claims, optionally filtered by domain and/or free-text query. */
  async listClaims(
    domain?: string,
    query?: string
  ): Promise<{ claims: ClaimResponse[] }> {
    const params = new URLSearchParams();
    if (domain) params.set("domain", domain);
    if (query) params.set("query", query);

    const qs = params.toString();
    const url = qs
      ? `${this.baseUrl}/v0/knowledge?${qs}`
      : `${this.baseUrl}/v0/knowledge`;

    const response = await fetchWithTimeout(url);
    await checkError(response);
    return (await response.json()) as { claims: ClaimResponse[] };
  }

  /** Mark one claim as superseding another. Requires the new claim's write_key. */
  async supersedeClaim(
    claimId: string,
    supersedesId: string,
    writeKey: string
  ): Promise<{ status: string }> {
    const url = `${this.baseUrl}/v0/knowledge/${encodeURIComponent(claimId)}/supersede`;
    const response = await fetchWithTimeout(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ supersedes: supersedesId, write_key: writeKey }),
    });
    await checkError(response);
    return (await response.json()) as { status: string };
  }

  /** Mark one claim as contradicting another. Requires the new claim's write_key. */
  async contradictClaim(
    claimId: string,
    contradictsId: string,
    writeKey: string
  ): Promise<{ status: string }> {
    const url = `${this.baseUrl}/v0/knowledge/${encodeURIComponent(claimId)}/contradict`;
    const response = await fetchWithTimeout(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ contradicts: contradictsId, write_key: writeKey }),
    });
    await checkError(response);
    return (await response.json()) as { status: string };
  }

  /** Attest to an existing claim by providing an issuer identity. */
  async attestClaim(
    claimId: string,
    issuer: string
  ): Promise<{ status: string }> {
    const url = `${this.baseUrl}/v0/knowledge/${encodeURIComponent(claimId)}/attest`;
    const response = await fetchWithTimeout(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ issuer }),
    });
    await checkError(response);
    return (await response.json()) as { status: string };
  }

  // ── Memory methods ────────────────────────────────────────

  /**
   * Create a new memory entry.
   * Returns the memory_id and a write_key needed for updates/deletion.
   */
  async createMemory(
    content: string,
    owner: string,
    category?: string,
    retention?: string,
    purpose?: string,
    subject?: string
  ): Promise<{ memory_id: string; write_key: string }> {
    const body: Record<string, string> = { content, owner };
    if (category) body.category = category;
    if (retention) body.retention = retention;
    if (purpose) body.purpose = purpose;
    if (subject) body.subject = subject;

    const response = await fetchWithTimeout(`${this.baseUrl}/v0/memory`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    await checkError(response);
    return (await response.json()) as { memory_id: string; write_key: string };
  }

  /** Fetch a single memory entry by ID. */
  async getMemory(memoryId: string): Promise<MemoryResponse> {
    const url = `${this.baseUrl}/v0/memory/${encodeURIComponent(memoryId)}`;
    const response = await fetchWithTimeout(url);
    await checkError(response);
    return (await response.json()) as MemoryResponse;
  }

  /** List memories, optionally filtered by agent, category, or free-text query. */
  async listMemories(
    agent?: string,
    category?: string,
    query?: string
  ): Promise<{ memories: MemoryResponse[] }> {
    const params = new URLSearchParams();
    if (agent) params.set("agent", agent);
    if (category) params.set("category", category);
    if (query) params.set("query", query);

    const qs = params.toString();
    const url = qs
      ? `${this.baseUrl}/v0/memory?${qs}`
      : `${this.baseUrl}/v0/memory`;

    const response = await fetchWithTimeout(url);
    await checkError(response);
    return (await response.json()) as { memories: MemoryResponse[] };
  }

  /** Update an existing memory entry. Requires the write_key. */
  async updateMemory(
    memoryId: string,
    writeKey: string,
    updates: Record<string, any>
  ): Promise<MemoryResponse> {
    const url = `${this.baseUrl}/v0/memory/${encodeURIComponent(memoryId)}`;
    const response = await fetchWithTimeout(url, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ write_key: writeKey, ...updates }),
    });
    await checkError(response);
    return (await response.json()) as MemoryResponse;
  }

  /** Delete a memory entry. Requires the write_key. */
  async deleteMemory(
    memoryId: string,
    writeKey: string
  ): Promise<{ status: string }> {
    const url = `${this.baseUrl}/v0/memory/${encodeURIComponent(memoryId)}`;
    const response = await fetchWithTimeout(url, {
      method: "DELETE",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ write_key: writeKey }),
    });
    await checkError(response);
    return (await response.json()) as { status: string };
  }

  /** Register an agent as remembering a memory entry. */
  async remember(
    memoryId: string,
    agentId: string
  ): Promise<{ status: string }> {
    const url = `${this.baseUrl}/v0/memory/${encodeURIComponent(memoryId)}/remember`;
    const response = await fetchWithTimeout(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ agent: agentId }),
    });
    await checkError(response);
    return (await response.json()) as { status: string };
  }

  /** Remove an agent from the remember list of a memory entry. */
  async forget(
    memoryId: string,
    agentId: string
  ): Promise<{ status: string }> {
    const url = `${this.baseUrl}/v0/memory/${encodeURIComponent(memoryId)}/forget?agent=${encodeURIComponent(agentId)}`;
    const response = await fetchWithTimeout(url, {
      method: "DELETE",
    });
    await checkError(response);
    return (await response.json()) as { status: string };
  }
}