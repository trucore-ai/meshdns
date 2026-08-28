"""MeshDNS HTTP client with resolve, resolve_next, and list_servers."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Optional

import httpx


@dataclass
class ServerInfo:
    """A server returned by MeshDNS resolution or listing."""

    name: str
    server_url: str
    capabilities: list[str] = field(default_factory=list)
    uptime_30d: float = 0.0
    last_checked_at: str = ""


class MeshDNSError(Exception):
    """Raised on 4xx/5xx responses from the MeshDNS server."""

    def __init__(self, status_code: int, detail: str) -> None:
        self.status_code = status_code
        self.detail = detail
        super().__init__(f"MeshDNS {status_code}: {detail}")


class MeshDNS:
    """Synchronous HTTP client for MeshDNS.

    Args:
        base_url: MeshDNS server base URL (e.g. ``http://localhost:8080``).
    """

    def __init__(self, base_url: str) -> None:
        self._base = base_url.rstrip("/")
        self._client = httpx.Client(
            base_url=self._base,
            timeout=httpx.Timeout(5.0),
        )

    def resolve(self, capability: str) -> list[ServerInfo]:
        """Resolve servers advertising *capability*.

        Returns only servers that are currently up and active, ordered by
        descending 30-day uptime then by name.
        """
        r = self._client.get("/v0/resolve", params={"capability": capability})
        self._raise_for_error(r)
        return [_parse_server_json(obj) for obj in r.json()]

    def resolve_next(
        self, capability: str, skip: frozenset[str]
    ) -> list[ServerInfo]:
        """Like :meth:`resolve` but excludes servers whose **name** is in *skip*."""
        servers = self.resolve(capability)
        return [s for s in servers if s.name not in skip]

    def list_servers(
        self,
        query: str = "",
        capability: str = "",
        status: str = "",
        cursor: str = "",
        limit: int = 20,
    ) -> tuple[list[ServerInfo], Optional[str]]:
        """List servers with optional filtering and cursor-based pagination.

        Args:
            query: Free-text search across name and description.
            capability: Filter to servers with a specific capability.
            status: One of ``"active"``, ``"delisted"``, or ``"all"``.
            cursor: Opaque pagination cursor from a previous response.
            limit: Page size (capped at 100).

        Returns:
            A ``(servers, next_cursor)`` tuple.  *next_cursor* is ``None``
            when there are no more pages.
        """
        if limit > 100:
            limit = 100
        if limit < 1:
            limit = 1

        r = self._client.get(
            "/v0/servers",
            params={
                "query": query,
                "capability": capability,
                "status": status,
                "cursor": cursor,
                "limit": str(limit),
            },
        )
        self._raise_for_error(r)
        body = r.json()
        servers = [_parse_server_json(obj) for obj in body.get("servers", [])]
        next_cursor = body.get("next_cursor") or None
        return servers, next_cursor

    def close(self) -> None:
        """Close the underlying HTTP client."""
        self._client.close()

    # ------------------------------------------------------------------
    # Knowledge API
    # ------------------------------------------------------------------

    def create_claim(
        self, content: str, domain: str, issuer: str = ""
    ) -> dict:
        """Create a new knowledge claim.

        POST /v0/knowledge

        Returns:
            ``{claim_id, write_key}``
        """
        r = self._client.post(
            "/v0/knowledge",
            json={"content": content, "domain": domain, "issuer": issuer},
        )
        self._raise_for_error(r)
        return r.json()

    def get_claim(self, claim_id: str) -> dict:
        """Retrieve a claim with its provenance.

        GET /v0/knowledge/{id}
        """
        r = self._client.get(f"/v0/knowledge/{claim_id}")
        self._raise_for_error(r)
        return r.json()

    def list_claims(
        self, domain: str = "", query: str = ""
    ) -> list:
        """List knowledge claims.

        GET /v0/knowledge

        Returns:
            ``{claims: [...]}``  — the ``claims`` key is extracted and returned
            as a list of claim dicts for convenience.
        """
        params = {}
        if domain:
            params["domain"] = domain
        if query:
            params["query"] = query
        r = self._client.get("/v0/knowledge", params=params if params else None)
        self._raise_for_error(r)
        return r.json().get("claims", []) if isinstance(r.json(), dict) else r.json()

    def supersede_claim(
        self, claim_id: str, supersedes_id: str, write_key: str
    ) -> dict:
        """Mark a claim as superseding another.

        POST /v0/knowledge/{id}/supersede
        """
        r = self._client.post(
            f"/v0/knowledge/{claim_id}/supersede",
            json={"supersedes_id": supersedes_id, "write_key": write_key},
        )
        self._raise_for_error(r)
        return r.json()

    def contradict_claim(
        self, claim_id: str, contradicts_id: str, write_key: str
    ) -> dict:
        """Mark a claim as contradicting another.

        POST /v0/knowledge/{id}/contradict
        """
        r = self._client.post(
            f"/v0/knowledge/{claim_id}/contradict",
            json={"contradicts_id": contradicts_id, "write_key": write_key},
        )
        self._raise_for_error(r)
        return r.json()

    def attest_claim(self, claim_id: str, issuer: str) -> dict:
        """Attest to an existing claim.

        POST /v0/knowledge/{id}/attest
        """
        r = self._client.post(
            f"/v0/knowledge/{claim_id}/attest",
            json={"issuer": issuer},
        )
        self._raise_for_error(r)
        return r.json()

    # ------------------------------------------------------------------
    # Memory API
    # ------------------------------------------------------------------

    def create_memory(
        self,
        content: str,
        owner: str,
        category: str = "fact",
        retention: str = "permanent",
        purpose: str = "",
        subject: str = "",
    ) -> dict:
        """Create a new memory entry.

        POST /v0/memory

        Returns:
            ``{memory_id, write_key}``
        """
        r = self._client.post(
            "/v0/memory",
            json={
                "content": content,
                "owner": owner,
                "category": category,
                "retention": retention,
                "purpose": purpose,
                "subject": subject,
            },
        )
        self._raise_for_error(r)
        return r.json()

    def get_memory(self, memory_id: str) -> dict:
        """Retrieve a memory entry.

        GET /v0/memory/{id}
        """
        r = self._client.get(f"/v0/memory/{memory_id}")
        self._raise_for_error(r)
        return r.json()

    def list_memories(
        self,
        agent: str = "",
        category: str = "",
        query: str = "",
    ) -> list:
        """List memory entries.

        GET /v0/memory

        Returns:
            ``{memories: [...]}``  — the ``memories`` key is extracted and returned
            as a list of memory dicts for convenience.
        """
        params = {}
        if agent:
            params["agent"] = agent
        if category:
            params["category"] = category
        if query:
            params["query"] = query
        r = self._client.get("/v0/memory", params=params if params else None)
        self._raise_for_error(r)
        body = r.json()
        return body.get("memories", []) if isinstance(body, dict) else body

    def update_memory(
        self, memory_id: str, write_key: str, **kwargs
    ) -> dict:
        """Update a memory entry.

        PUT /v0/memory/{id}
        """
        payload = {"write_key": write_key}
        payload.update(kwargs)
        r = self._client.put(f"/v0/memory/{memory_id}", json=payload)
        self._raise_for_error(r)
        return r.json()

    def delete_memory(self, memory_id: str, write_key: str) -> dict:
        """Delete a memory entry.

        DELETE /v0/memory/{id}
        """
        r = self._client.delete(
            f"/v0/memory/{memory_id}", json={"write_key": write_key}
        )
        self._raise_for_error(r)
        return r.json()

    def remember(self, memory_id: str, agent_id: str) -> dict:
        """Mark a memory as remembered by an agent.

        POST /v0/memory/{id}/remember
        """
        r = self._client.post(
            f"/v0/memory/{memory_id}/remember",
            json={"agent_id": agent_id},
        )
        self._raise_for_error(r)
        return r.json()

    def forget(self, memory_id: str, agent_id: str) -> dict:
        """Remove an agent's remember marker from a memory.

        DELETE /v0/memory/{id}/forget?agent=...
        """
        r = self._client.delete(
            f"/v0/memory/{memory_id}/forget",
            params={"agent": agent_id},
        )
        self._raise_for_error(r)
        return r.json()

    def _raise_for_error(self, r: httpx.Response) -> None:
        if r.is_success:
            return
        detail = "unknown error"
        try:
            err_body = r.json()
            detail = err_body.get("error", {}).get("detail", str(err_body))
        except Exception:
            detail = r.text or f"HTTP {r.status_code}"
        raise MeshDNSError(r.status_code, detail)


def _parse_server_json(obj: dict) -> ServerInfo:
    return ServerInfo(
        name=obj.get("name", ""),
        server_url=obj.get("server_url", ""),
        capabilities=list(obj.get("capabilities", [])),
        uptime_30d=float(obj.get("uptime_30d", 0.0)),
        last_checked_at=str(obj.get("last_checked_at", "")),
    )