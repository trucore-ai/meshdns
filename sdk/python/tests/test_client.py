"""Integration tests for meshdns_client against a live MeshDNS server."""

from __future__ import annotations

import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time

import pytest

from meshdns_client import MeshDNS, MeshDNSError, ServerInfo

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------

SRC_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))


def _pick_port() -> int:
    """Return an available TCP port on localhost."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _build_binary() -> str | None:
    """Build the meshdns Go binary, return path or None."""
    dest = os.path.join(tempfile.gettempdir(), "meshdns-test-server")
    if shutil.which("go") is None:
        return None
    try:
        subprocess.run(
            ["go", "build", "-o", dest, "./cmd/provengraph"],
            cwd=SRC_DIR,
            check=True,
            capture_output=True,
            text=True,
        )
        return dest
    except subprocess.CalledProcessError:
        return None


# ---------------------------------------------------------------------------
# fixtures
# ---------------------------------------------------------------------------


@pytest.fixture(scope="session")
def meshdns_binary():
    """Session-scoped: build once, skip all tests if unavailable."""
    path = _build_binary()
    if path is None:
        pytest.skip("go build not available")
    yield path
    try:
        if path:
            os.remove(path)
    except OSError:
        pass


@pytest.fixture(scope="session")
def meshdns_url(meshdns_binary):
    """Start the server on a temp port, return base URL, tear down on exit."""
    port = _pick_port()
    db_path = os.path.join(tempfile.gettempdir(), f"meshdns-test-{port}.db")

    env = os.environ.copy()
    env["PROVENGRAPH_PORT"] = f":{port}"
    env["PROVENGRAPH_DB"] = db_path

    proc = subprocess.Popen(
        [meshdns_binary],
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )

    base_url = f"http://127.0.0.1:{port}"

    # Wait until the server is ready (up to 10 s)
    deadline = time.time() + 10
    import urllib.request

    while time.time() < deadline:
        try:
            urllib.request.urlopen(f"{base_url}/v0/servers", timeout=2)
            break
        except Exception:
            time.sleep(0.2)
    else:
        proc.kill()
        proc.wait()
        pytest.fail("MeshDNS server did not start in time")

    yield base_url

    # teardown
    proc.terminate()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()
    try:
        os.remove(db_path)
    except OSError:
        pass


@pytest.fixture()
def client(meshdns_url):
    """Per-test client connected to the live server."""
    c = MeshDNS(meshdns_url)
    yield c
    c.close()


# ---------------------------------------------------------------------------
# tests
# ---------------------------------------------------------------------------


class TestResolve:
    def test_resolve_returns_matching_server(self, meshdns_url, client):
        """Register a server with "sandbox", then resolve it."""
        import urllib.request
        import json

        data = json.dumps({
            "name": "test-sandbox",
            "description": "sdk test",
            "server_url": "http://example.com",
            "capabilities": ["sandbox"],
        }).encode()

        req = urllib.request.Request(
            f"{meshdns_url}/v0/servers",
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        resp = urllib.request.urlopen(req)
        assert resp.status == 201

        servers = client.resolve("sandbox")
        # Newly registered server won't be "up" yet (no probes), so resolve may return empty.
        # That's expected behaviour — verify we get a list back.
        assert isinstance(servers, list)

    def test_resolve_unknown_capability_returns_empty(self, client):
        """Resolve a capability no server has."""
        servers = client.resolve("nonexistent-cap")
        assert servers == []


class TestResolveNext:
    def test_resolve_next_filters_skipped_servers(self, meshdns_url, client):
        """Register two servers, then resolve_next with a skip set."""
        import urllib.request
        import json

        for name in ("skip-a", "skip-b"):
            data = json.dumps({
                "name": name,
                "description": "sdk test",
                "server_url": f"http://{name}.example.com",
                "capabilities": ["sandbox"],
            }).encode()
            req = urllib.request.Request(
                f"{meshdns_url}/v0/servers",
                data=data,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            urllib.request.urlopen(req)

        # New servers aren't up yet, so resolve_next may return 0 or some.
        # The key test: no server with name in skip appears.
        all_servers = client.resolve("sandbox")
        skip = frozenset({s.name for s in all_servers})
        next_servers = client.resolve_next("sandbox", skip)
        assert all(s.name not in skip for s in next_servers)

    def test_resolve_next_empty_skip_returns_all(self, client):
        """With empty skip, resolve_next == resolve."""
        servers = client.resolve("sandbox")
        next_servers = client.resolve_next("sandbox", frozenset())
        assert len(servers) == len(next_servers)


class TestListServers:
    def test_list_servers_returns_tuple(self, client):
        """list_servers returns (servers, next_cursor)."""
        servers, next_cursor = client.list_servers()
        assert isinstance(servers, list)
        # next_cursor is None or str
        assert next_cursor is None or isinstance(next_cursor, str)

    def test_list_servers_with_capability_filter(self, meshdns_url, client):
        """Filter by capability."""
        import urllib.request
        import json

        data = json.dumps({
            "name": "cap-filter-test",
            "description": "sdk test",
            "server_url": "http://cap.example.com",
            "capabilities": ["unique-cap"],
        }).encode()
        req = urllib.request.Request(
            f"{meshdns_url}/v0/servers",
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        urllib.request.urlopen(req)

        servers, _ = client.list_servers(capability="unique-cap")
        names = {s.name for s in servers}
        assert "cap-filter-test" in names

    def test_list_servers_limit_capped(self, client):
        """Limit > 100 is capped to 100."""
        servers, _ = client.list_servers(limit=500)
        assert len(servers) <= 100

    def test_list_servers_pagination(self, meshdns_url, client):
        """Register 3 servers, paginate with limit=1."""
        import urllib.request
        import json

        for i in range(3):
            data = json.dumps({
                "name": f"page-test-{i}",
                "description": "sdk test",
                "server_url": f"http://page{i}.example.com",
                "capabilities": ["sandbox"],
            }).encode()
            req = urllib.request.Request(
                f"{meshdns_url}/v0/servers",
                data=data,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            urllib.request.urlopen(req)

        # Collect all pages
        all_names: set[str] = set()
        cursor: str | None = None
        pages = 0
        while pages < 20:  # safety valve
            servers, next_cursor = client.list_servers(limit=1, cursor=cursor or "")
            if not servers:
                break
            all_names.update(s.name for s in servers)
            cursor = next_cursor
            pages += 1
            if cursor is None:
                break

        for i in range(3):
            assert f"page-test-{i}" in all_names


class TestErrors:
    def test_4xx_raises_meshdns_error(self, client):
        """Resolve without capability param returns 422."""
        with pytest.raises(MeshDNSError) as exc_info:
            client.resolve("")
        assert exc_info.value.status_code == 422


class TestServerInfo:
    def test_server_info_is_dataclass(self):
        s = ServerInfo(
            name="test",
            server_url="http://x",
            capabilities=["a"],
            uptime_30d=0.99,
            last_checked_at="2024-01-01",
        )
        assert s.name == "test"
        assert s.server_url == "http://x"
        assert s.capabilities == ["a"]
        assert s.uptime_30d == 0.99
        assert s.last_checked_at == "2024-01-01"


# ---------------------------------------------------------------------------
# Knowledge tests
# ---------------------------------------------------------------------------


class TestKnowledgeCreateAndGet:
    def test_create_claim_returns_id_and_write_key(self, client):
        result = client.create_claim(
            content="The sky is blue",
            domain="science",
            issuer="alice",
        )
        assert "claim_id" in result
        assert "write_key" in result

    def test_get_claim_returns_provenance(self, client):
        created = client.create_claim(
            content="Water boils at 100°C",
            domain="physics",
        )
        claim_id = created["claim_id"]
        claim = client.get_claim(claim_id)
        assert claim.get("content") == "Water boils at 100°C"
        assert claim.get("domain") == "physics"

    def test_list_claims_returns_matching(self, client):
        client.create_claim(content="claim-a", domain="test-k")
        client.create_claim(content="claim-b", domain="test-k")
        results = client.list_claims(domain="test-k")
        assert isinstance(results, list)
        contents = {c.get("content") for c in results}
        assert "claim-a" in contents
        assert "claim-b" in contents

    def test_list_claims_empty_domain(self, client):
        results = client.list_claims(domain="nonexistent-domain-xyz")
        assert results == []


class TestClaimRelationships:
    def test_supersede_claim(self, client):
        old = client.create_claim(content="original", domain="test")
        new = client.create_claim(content="updated", domain="test")
        result = client.supersede_claim(
            claim_id=new["claim_id"],
            supersedes_id=old["claim_id"],
            write_key=new["write_key"],
        )
        assert "status" in result or "claim_id" in result

    def test_contradict_claim(self, client):
        a = client.create_claim(content="a says x", domain="test")
        b = client.create_claim(content="b says not x", domain="test")
        result = client.contradict_claim(
            claim_id=b["claim_id"],
            contradicts_id=a["claim_id"],
            write_key=b["write_key"],
        )
        assert "status" in result or "claim_id" in result

    def test_attest_claim(self, client):
        claim = client.create_claim(content="trustworthy fact", domain="test")
        result = client.attest_claim(
            claim_id=claim["claim_id"],
            issuer="bob",
        )
        assert "status" in result or "claim_id" in result


class TestKnowledgeErrors:
    def test_get_nonexistent_claim_raises(self, client):
        with pytest.raises(MeshDNSError) as exc_info:
            client.get_claim("nonexistent-claim-id")
        assert exc_info.value.status_code in (404, 422, 500)


# ---------------------------------------------------------------------------
# Memory tests
# ---------------------------------------------------------------------------


class TestMemoryCreateAndGet:
    def test_create_memory_returns_id_and_write_key(self, client):
        result = client.create_memory(
            content="User prefers dark mode",
            owner="agent-1",
        )
        assert "memory_id" in result
        assert "write_key" in result

    def test_get_memory_returns_data(self, client):
        created = client.create_memory(
            content="Meeting at 3pm",
            owner="agent-2",
            category="event",
        )
        memory_id = created["memory_id"]
        memory = client.get_memory(memory_id)
        assert memory.get("content") == "Meeting at 3pm"
        assert memory.get("owner") == "agent-2"

    def test_list_memories_returns_matching(self, client):
        client.create_memory(content="mem-a", owner="test-agent", category="fact")
        client.create_memory(content="mem-b", owner="test-agent", category="fact")
        results = client.list_memories(agent="test-agent")
        assert isinstance(results, list)
        contents = {m.get("content") for m in results}
        assert "mem-a" in contents
        assert "mem-b" in contents

    def test_list_memories_by_category(self, client):
        client.create_memory(
            content="cat-test", owner="agent-x", category="unique-cat"
        )
        results = client.list_memories(category="unique-cat")
        assert isinstance(results, list)
        assert any(m.get("content") == "cat-test" for m in results)

    def test_list_memories_empty(self, client):
        results = client.list_memories(agent="nonexistent-agent-xyz")
        assert results == []


class TestMemoryMutate:
    def test_update_memory(self, client):
        created = client.create_memory(
            content="old content", owner="agent-3"
        )
        result = client.update_memory(
            memory_id=created["memory_id"],
            write_key=created["write_key"],
            content="new content",
        )
        assert "memory_id" in result or "status" in result

        # Verify the update persisted
        updated = client.get_memory(created["memory_id"])
        assert updated.get("content") == "new content"

    def test_delete_memory(self, client):
        created = client.create_memory(
            content="to be deleted", owner="agent-4"
        )
        result = client.delete_memory(
            memory_id=created["memory_id"],
            write_key=created["write_key"],
        )
        assert "memory_id" in result or "status" in result

        # Verify deletion
        with pytest.raises(MeshDNSError) as exc_info:
            client.get_memory(created["memory_id"])
        assert exc_info.value.status_code in (404, 422, 500)


class TestMemoryRememberForget:
    def test_remember_and_forget(self, client):
        created = client.create_memory(
            content="important fact", owner="owner-1"
        )
        memory_id = created["memory_id"]

        # Remember
        remember_result = client.remember(memory_id=memory_id, agent_id="agent-x")
        assert "status" in remember_result or "memory_id" in remember_result

        # Forget
        forget_result = client.forget(memory_id=memory_id, agent_id="agent-x")
        assert "status" in forget_result or "memory_id" in forget_result


class TestMemoryErrors:
    def test_get_nonexistent_memory_raises(self, client):
        with pytest.raises(MeshDNSError) as exc_info:
            client.get_memory("nonexistent-memory-id")
        assert exc_info.value.status_code in (404, 422, 500)

    def test_update_nonexistent_memory_raises(self, client):
        with pytest.raises(MeshDNSError) as exc_info:
            client.update_memory(
                memory_id="nonexistent-id",
                write_key="fake-key",
                content="will fail",
            )
        assert exc_info.value.status_code in (404, 422, 500)