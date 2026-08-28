#!/usr/bin/env python3
"""MeshDNS load probe — verify NFR-001: resolve p99 < 100ms with 1,000 registered servers.

Creates a temporary SQLite DB, seeds 1,000 fake servers directly (fast),
starts the meshdns binary, runs 500 sequential resolve requests, and computes
latency percentiles.

Usage: python3 scripts/load_probe.py
"""

import json
import os
import signal
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.request


SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
REPO_DIR = os.path.dirname(SCRIPT_DIR)  # src/
BINARY = os.path.join(REPO_DIR, "meshdns")

DB_PATH = "/tmp/load_probe.db"
PORT = 8097
BASE_URL = f"http://localhost:{PORT}"
NUM_SERVERS = 1000
NUM_REQUESTS = 500


def build_binary():
    """go build -o provengraph ./cmd/provengraph"""
    print(f"Building meshdns binary...", flush=True)
    result = subprocess.run(
        ["go", "build", "-o", BINARY, "./cmd/provengraph"],
        cwd=REPO_DIR,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        print(f"go build failed:\n{result.stderr}", flush=True)
        sys.exit(1)
    print(f"Binary built: {BINARY}", flush=True)


def seed_db(db_path: str, num_servers: int):
    """Write servers, capabilities, and server_state rows directly via sqlite3 for speed."""
    print(f"Seeding {num_servers} servers into {db_path}...", flush=True)

    conn = sqlite3.connect(db_path)

    # Create schema (same as store.go migrations)
    conn.executescript("""
    CREATE TABLE IF NOT EXISTS servers (
        id TEXT PRIMARY KEY,
        name TEXT UNIQUE NOT NULL,
        description TEXT,
        server_url TEXT NOT NULL,
        health_url TEXT,
        write_key_hash TEXT NOT NULL,
        owner_contact TEXT,
        status TEXT NOT NULL DEFAULT 'active',
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL
    );
    CREATE TABLE IF NOT EXISTS capabilities (
        server_id TEXT NOT NULL,
        capability TEXT NOT NULL,
        PRIMARY KEY (server_id, capability)
    );
    CREATE TABLE IF NOT EXISTS probes (
        id INTEGER PRIMARY KEY,
        server_id TEXT NOT NULL,
        ts TEXT NOT NULL,
        up INTEGER NOT NULL,
        latency_ms INTEGER
    );
    CREATE INDEX IF NOT EXISTS idx_probes_server_ts ON probes(server_id, ts);
    CREATE TABLE IF NOT EXISTS server_state (
        server_id TEXT PRIMARY KEY,
        up INTEGER NOT NULL DEFAULT 0,
        last_checked_at TEXT,
        uptime_30d REAL NOT NULL DEFAULT 0
    );
    CREATE TABLE IF NOT EXISTS events (
        id INTEGER PRIMARY KEY,
        ts TEXT NOT NULL,
        type TEXT NOT NULL,
        payload TEXT NOT NULL
    );
    """)

    now = "2026-08-21T00:00:00Z"

    # Bulk insert servers
    servers_data = []
    for i in range(num_servers):
        srv_id = f"aaaaaaaa-bbbb-cccc-dddd-{i:012d}"
        name = f"srv-{i:04d}"
        servers_data.append((
            srv_id, name, "", "https://example.com", "",
            "0000000000000000000000000000000000000000000000000000000000000000",
            "", "active", now, now,
        ))
    conn.executemany(
        "INSERT INTO servers (id, name, description, server_url, health_url, "
        "write_key_hash, owner_contact, status, created_at, updated_at) "
        "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
        servers_data,
    )

    # Bulk insert capabilities
    cap_data = [(f"aaaaaaaa-bbbb-cccc-dddd-{i:012d}", "sandbox") for i in range(num_servers)]
    conn.executemany(
        "INSERT INTO capabilities (server_id, capability) VALUES (?, ?)",
        cap_data,
    )

    # Bulk insert server_state (up=1, uptime_30d=1.0)
    state_data = [(f"aaaaaaaa-bbbb-cccc-dddd-{i:012d}", 1, now, 1.0) for i in range(num_servers)]
    conn.executemany(
        "INSERT INTO server_state (server_id, up, last_checked_at, uptime_30d) "
        "VALUES (?, ?, ?, ?)",
        state_data,
    )

    conn.commit()
    conn.close()

    # Verify
    conn = sqlite3.connect(db_path)
    count = conn.execute("SELECT count(*) FROM servers").fetchone()[0]
    up_count = conn.execute("SELECT count(*) FROM server_state WHERE up = 1").fetchone()[0]
    cap_count = conn.execute("SELECT count(*) FROM capabilities WHERE capability = 'sandbox'").fetchone()[0]
    conn.close()

    print(f"Seeded: {count} servers, {up_count} up, {cap_count} with capability 'sandbox'", flush=True)


def start_server():
    """Start meshdns binary on :8097 with the seeded DB."""
    print(f"Starting meshdns on :{PORT}...", flush=True)

    env = os.environ.copy()
    env["PROVENGRAPH_DB"] = DB_PATH
    env["PROVENGRAPH_PORT"] = f":{PORT}"
    env["PROVENGRAPH_PROBE_INTERVAL"] = "999s"

    proc = subprocess.Popen(
        [BINARY],
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )

    # Wait for server to be ready
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        try:
            req = urllib.request.Request(f"{BASE_URL}/v0/stats")
            with urllib.request.urlopen(req, timeout=2) as resp:
                if resp.status == 200:
                    data = json.loads(resp.read().decode())
                    if data.get("servers_active", 0) >= NUM_SERVERS:
                        print(f"Server ready: {data}", flush=True)
                        return proc
        except Exception:
            pass
        time.sleep(0.1)

    proc.kill()
    proc.wait()
    print("Server failed to start within 10s", flush=True)
    sys.exit(1)


def run_benchmark(num_requests: int):
    """Run sequential resolve requests and collect latencies."""
    print(f"Running {num_requests} resolve requests...", flush=True)

    url = f"{BASE_URL}/v0/resolve?capability=sandbox"
    latencies = []

    for i in range(num_requests):
        start = time.monotonic()
        try:
            req = urllib.request.Request(url)
            with urllib.request.urlopen(req, timeout=5) as resp:
                body = resp.read()
            elapsed = (time.monotonic() - start) * 1000  # ms
            latencies.append(elapsed)

            # Parse result count for logging
            try:
                servers = json.loads(body)
                resolved_count = len(servers) if isinstance(servers, list) else 0
            except Exception:
                resolved_count = -1

            if (i + 1) % 100 == 0:
                recent = latencies[-100:]
                avg = sum(recent) / len(recent)
                print(f"  {i+1}/{num_requests}: avg(100)={avg:.1f}ms resolved={resolved_count}", flush=True)
        except Exception as e:
            elapsed = (time.monotonic() - start) * 1000
            latencies.append(elapsed)
            print(f"  Request {i+1} error: {e} ({elapsed:.1f}ms)", flush=True)

    return latencies


def compute_percentiles(latencies, *percentiles):
    """Compute given percentiles from sorted latencies list."""
    sorted_lats = sorted(latencies)
    results = {}
    for p in percentiles:
        idx = int(round(p / 100.0 * (len(sorted_lats) - 1)))
        results[p] = sorted_lats[idx]
    results["max"] = sorted_lats[-1] if sorted_lats else 0
    return results, sorted_lats


def main():
    # Clean up any leftover
    if os.path.exists(DB_PATH):
        os.remove(DB_PATH)

    # 1. Build binary
    build_binary()

    # 2. Seed DB
    seed_db(DB_PATH, NUM_SERVERS)

    # 3. Start server
    proc = start_server()

    try:
        # 4. Run benchmark
        latencies = run_benchmark(NUM_REQUESTS)

        # 5. Compute percentiles
        pcts, _ = compute_percentiles(latencies, 50, 99)

        # 6. Print results
        print()
        print("=" * 60)
        print(f"resolved {NUM_SERVERS} servers in {NUM_REQUESTS} requests: "
              f"p50={pcts[50]:.1f}ms p99={pcts[99]:.1f}ms max={pcts['max']:.1f}ms")
        print("=" * 60)

        if pcts[99] < 100:
            print("✅ NFR-001 PASS: resolve p99 < 100ms")
        else:
            print(f"❌ NFR-001 FAIL: resolve p99 = {pcts[99]:.1f}ms >= 100ms")
    finally:
        # 7. Cleanup
        print("Shutting down server...", flush=True)
        proc.send_signal(signal.SIGTERM)
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()

        if os.path.exists(DB_PATH):
            os.remove(DB_PATH)
            print(f"Removed {DB_PATH}", flush=True)

        # Don't remove the binary — it can be reused
        print("Done.", flush=True)


if __name__ == "__main__":
    main()