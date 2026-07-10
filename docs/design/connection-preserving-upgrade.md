# Connection-preserving upgrade (🎯T97)

mnemo should upgrade its own binary without dropping live MCP sessions.
The blocker is not byte-swapping the binary but that Claude Code caches
`Mcp-Session-Id` and does not re-initialize on server restart
([claude-code#27142](https://github.com/anthropics/claude-code/issues/27142)).

## Architecture

A **thin transparent edge** owns the public listener (`:19419` by default)
and all client TCP connections. One or more **backends** run the full
mnemo daemon on loopback HTTP or a Unix domain socket. The edge forwards
standard MCP-over-HTTP — no custom framing, no protocol extensions.

```
  Claude Code ──TCP──► edge (:19419)
                           │
              Mcp-Session-Id routing
                           │
            ┌──────────────┼──────────────┐
            ▼              ▼              ▼
        backend A    backend B      (future)
     127.0.0.1:19421  UDS / tmp/mnemo.sock
```

### Session affinity

| Request | Routing |
|---------|---------|
| `POST … initialize` (no session header) | **Primary** backend — the one designated for new sessions |
| `POST/GET/DELETE` with `Mcp-Session-Id` | Backend that handled that session's `initialize` |
| Dashboard / `/api/*` (no session) | Primary backend |

On `initialize`, the edge records `Mcp-Session-Id` from the backend
response and pins the session. New `initialize` calls after a backend swap
can be directed to a different primary while existing sessions stay on
the draining backend.

### SSE / GET streams

The MCP streamable-HTTP transport opens a long-lived `GET` connection for
server-to-client notifications. The edge proxies this hop-by-hop with
flush-after-write so heartbeats and events arrive promptly.

### What stays in the edge process across a swap

- The listening socket and every accepted client connection
- The `Mcp-Session-Id → backend` pin table
- (Future) singleton-background lease arbitration (🎯T97.4)

The edge binary is small and changes rarely; backends are swapped during
upgrade.

## Sub-objectives

| ID | Status | Summary |
|----|--------|---------|
| T97.1 | achieved | Backend drains gracefully on SIGTERM; WAL checkpointed |
| T97.2 | identified | Release detection via `gh` + T83 health pipeline |
| T97.3 | achieved | Transparent edge proxy (this document's routing slice) |
| T97.4 | identified | Single-holder lease for ingest/compaction/mirrors |
| T97.5 | identified | Opt-in auto-apply at quiescent window |
| T97.6 | identified | One-time upgrade notice in tool results |

## Running edge + backend (T97.3 slice)

```bash
# Terminal 1 — backend on loopback (not reachable by clients directly)
bin/mnemo --addr 127.0.0.1:19421

# Terminal 2 — edge owns the public port
bin/mnemo edge --listen :19419 --backend http://127.0.0.1:19421
```

Multiple backends (affinity demo / drain prep):

```bash
bin/mnemo edge --listen :19419 \
  --backend http://127.0.0.1:19421 \
  --backend http://127.0.0.1:19422 \
  --primary 1
```

Unix socket backend:

```bash
bin/mnemo --addr unix:/tmp/mnemo-backend.sock
bin/mnemo edge --backend unix:///tmp/mnemo-backend.sock
```

Clients keep registering `http://localhost:19419/mcp` — only the edge
listens there.

## Invariants (carried from 🎯T27)

- Identity is `Mcp-Session-Id`, not a custom connection token
- Edge ↔ backend traffic is unmodified MCP streamable HTTP
- No session invalidation visible to the client during backend drain
