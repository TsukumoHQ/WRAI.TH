# Deployment

Single Go binary, SQLite on disk, embedded web UI. No required external services.
This guide covers exposing the relay to a team/server: bind, auth, reverse proxy,
TLS, and the full environment reference.

## Bind & exposure

By default the relay binds **loopback only** (`127.0.0.1:8090`) — safe for a
single machine where every client is local.

- `RELAY_BIND` — host to bind. Default `127.0.0.1`. Set `0.0.0.0` (or a specific
  interface / docker-bridge IP) to expose beyond the host.
- `PORT` — port. Default `8090`.

**The relay refuses to bind a non-loopback address without auth.** If you set
`RELAY_BIND=0.0.0.0` without `RELAY_API_KEY`, startup fails:

```
refusing to bind 0.0.0.0:8090 without auth: set RELAY_API_KEY to expose on a
non-loopback address, or unset RELAY_BIND to bind 127.0.0.1
```

## Authentication

Set `RELAY_API_KEY` to a strong shared secret. When set, every request must
carry `Authorization: Bearer <key>` — **except loopback clients**, which stay
keyless so the host's own MCP connections (`http://localhost:8090/mcp`), API
scripts, ingest hooks, and health checks keep working without the key.

```bash
RELAY_API_KEY=$(openssl rand -hex 32)
```

- Remote callers (through a reverse proxy / another host) send the Bearer token.
- `RELAY_TRUST_LOOPBACK=0` disables the loopback exemption — the token is then
  required even from `127.0.0.1`.
- The loopback check uses the real TCP peer (`RemoteAddr`), not
  `X-Forwarded-For`, so a remote client cannot spoof it with a header.

### ⚠️ Reverse-proxy + loopback caveat (read this)

The loopback exemption is only safe if **your reverse proxy does not reach the
relay over `127.0.0.1`**. If the proxy connects to the relay on loopback (common
when both run on the same host), the proxy's forwarded public traffic appears as
a loopback peer and **skips authentication entirely**.

**Do one of:**

1. **Recommended** — bind a non-loopback address and point the proxy at *that*:
   `RELAY_BIND=0.0.0.0` (or the docker-bridge / LAN IP) + `RELAY_API_KEY=…`.
   The proxy connects over the non-loopback address → its traffic requires the
   token; the host's own clients still use `127.0.0.1` → keyless. Firewall the
   bind port so it is reachable only via the proxy.
2. Keep loopback bind but set `RELAY_TRUST_LOOPBACK=0` and give the proxy the
   key. (Local host clients then also need the key.)

## Reverse proxy

Terminate TLS at the proxy and forward to the relay over a **non-loopback**
address (see caveat above).

### Traefik (docker labels)

```yaml
labels:
  - traefik.enable=true
  - traefik.http.routers.relay.rule=Host(`relay.example.com`)
  - traefik.http.routers.relay.entrypoints=websecure
  - traefik.http.routers.relay.tls.certresolver=le
  - traefik.http.services.relay.loadbalancer.server.port=8090
```

Reach the relay via its container/service name (a non-loopback address), not
`127.0.0.1`. Clients send `Authorization: Bearer <RELAY_API_KEY>`; Traefik
forwards the header.

### nginx

```nginx
server {
  listen 443 ssl;
  server_name relay.example.com;
  # ssl_certificate / ssl_certificate_key ...
  location / {
    proxy_pass http://10.0.0.5:8090;   # the relay's non-loopback address
    proxy_http_version 1.1;
    proxy_set_header Connection "";     # keep SSE/long-poll alive
    proxy_buffering off;                # required for /api/*/stream (SSE)
    proxy_read_timeout 1h;
  }
}
```

### Caddy

```
relay.example.com {
  reverse_proxy 10.0.0.5:8090 {
    flush_interval -1   # stream SSE without buffering
  }
}
```

SSE endpoints (`/api/activity/stream`, `/api/events/stream`) need proxy
buffering **off** and a long read timeout.

## Environment reference

| Variable | Default | Purpose |
|---|---|---|
| `RELAY_BIND` | `127.0.0.1` | Bind host. `0.0.0.0` to expose (requires `RELAY_API_KEY`). |
| `PORT` | `8090` | Bind port. |
| `RELAY_API_KEY` | _(unset)_ | Bearer token. Required to bind non-loopback. Loopback exempt. |
| `RELAY_TRUST_LOOPBACK` | `1` | `0` requires the token even from loopback. |
| `RELAY_CORS_ORIGINS` | _(none)_ | Comma-separated allowed origins (else same-origin only). |
| `RELAY_MAX_BODY` | `1048576` (1 MiB) | Max request body, bytes. Set `0` to disable the cap. |
| `RELAY_RATE_LIMIT` | _(off)_ | Requests/minute per IP. |
| `RELAY_REQUIRE_REGISTERED` | _(off)_ | `1`/`true` rejects mutating tool calls from an anonymous or unregistered agent (reads + `register_agent` stay open). |
| `RELAY_DB` | `~/.agent-relay/relay.db` | DB file path (set in dev/CI so a local run never migrates prod). |
| `RELAY_LINEAR_MODE` | `false` | `1`/`true` enables the Linear mirror (needs `LINEAR_API_KEY`). |
| `LINEAR_API_KEY` | _(unset)_ | Linear GraphQL personal key (mirror mode). |
| `LINEAR_WEBHOOK_SECRET` | _(unset)_ | HMAC secret for inbound Linear webhooks. |
| `LINEAR_TEAM_KEY` | _(unset)_ | Linear team key (e.g. `SYN`) — reconcile + state scope. |
| `RELAY_LINEAR_RECONCILE_INTERVAL` | `5m` | Active-cycle reconcile poll interval. |

## TLS

The relay serves plain HTTP; terminate TLS at the reverse proxy (Traefik/nginx/
Caddy with Let's Encrypt). Do not expose the relay's HTTP port publicly — route
all external traffic through the proxy.

## Platform notes

- **macOS (launchd):** see `com.agent-relay.plist`; `RUN_AT_LOAD` + `KEEP_ALIVE`.
  Restart after env changes: `launchctl bootout gui/$(id -u)/com.agent-relay &&
  launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agent-relay.plist`.
- **Linux (systemd):** run the binary as a service with the env vars above; set
  `RELAY_DB` to a stable path and back it up (`relay.db` is the single source).
- **Docker:** put the relay and proxy on the same network; the proxy reaches the
  relay by service name (non-loopback) — satisfies the caveat above.
