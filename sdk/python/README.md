# meshdns-client

Python client for [MeshDNS](https://github.com/trucore-ai/provengraph) — the distributed capability mesh.

## Install

```bash
pip install meshdns-client
```

## Quickstart

```python
from meshdns_client import MeshDNS

client = MeshDNS("http://localhost:8080")
servers = client.resolve("sandbox")
for s in servers:
    print(s.name, s.server_url, s.uptime_30d)
```