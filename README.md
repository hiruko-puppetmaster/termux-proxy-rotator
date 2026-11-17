# Termux Proxy Rotator

A small, dependency-free local proxy for OpenAI-compatible APIs. It runs on
Android inside Termux, listens on loopback, and rotates between a pool of
API keys, upstream providers, and local gateways such as opencode2api.

**New to the tool? Read the [detailed usage guide](docs/USAGE.md).**

```
AI client -> 127.0.0.1:9090 -> priority/cooldown pool -> upstream provider
                              |-- key/endpoint A (429) -> cooling down
                              `-- key/endpoint B (200) -> streamed back
```

## Features

- **Endpoint + credential pool.** Multiple base URLs and API keys, priority
  tiers, and round-robin inside a tier.
- **Transparent failover.** Intercepts `429`, `403`, `502`, `503`, and network
  errors by default, cools the failing entry down, and retries on the next
  entry before anything is sent to the client.
- **Per-status exponential cooldowns.** Honors `Retry-After`; a successful
  request clears the cooldown.
- **SSE and chunked passthrough.** SSE data is copied and flushed chunk by
  chunk, so `stream: true` works with AI coding assistants.
- **Request replay.** The request body is buffered once (up to a configurable
  limit) and replayed byte-for-byte on failover.
- **Compact logs.** Timestamped request, failure, rotation, and completion
  lines, with short request IDs.
- **Local status endpoint.** Health, cooldowns, counters, and a manual reset.
- **Zero external Go dependencies.** Standard library only; builds as one
  binary.

## Requirements

- Termux with `golang` (Go 1.22 or newer)
- A reachable local or remote OpenAI-compatible upstream
- No root, no cgo, no external libraries

The quick start below clones the repository; `docs/USAGE.md` covers manual
copies and the full operating guide.

## Quick start in Termux

```bash
pkg update
pkg install -y golang git

git clone https://github.com/hiruko-puppetmaster/termux-proxy-rotator.git
cd termux-proxy-rotator

cp config.example.json config.json
nano config.json                 # edit the upstreams

export OPENAI_API_KEY='sk-...'   # variables referenced by config.json
# export OPENAI_API_KEY_2='sk-...'

go build -trimpath -ldflags="-s -w" -o rotator .
./rotator -check -config config.json   # validate config without starting
./rotator -config config.json
```

Or use the Makefile:

```bash
make build
./rotator -config config.json
```

If `config.json` is missing, the program prints a hint and exits. It never
creates or modifies the configuration itself.

### Command-line flags

| Flag | Meaning |
| --- | --- |
| `-config path` | JSON configuration file (default `config.json`) |
| `-listen addr` | Override `listen`, e.g. `127.0.0.1:9090` |
| `-admin-listen addr` | Override `admin_listen`; use `off` to disable |
| `-check` | Validate the config, print the effective pool, and exit |
| `-version` | Print the version and exit |

Termux's Go does not support the race detector on `android/arm64`; use
`make test` (not `make test-race`) inside Termux.

## Client setup

Configure any OpenAI-compatible client with:

- **Base URL:** `http://127.0.0.1:9090/v1`
- **API key:** any non-empty placeholder, for example `sk-local`, unless
  `client_api_key` is set in the config, in which case use that value. The
  proxy accepts it as `Authorization: Bearer ...` or `X-Api-Key: ...`.

The proxy accepts `/v1/chat/completions`, `/v1/responses`, `/v1/models`, and
other API paths. If both the client path and the upstream base URL contain
`/v1`, the duplicate is removed automatically. For example:

| Upstream base URL | Incoming path | Forwarded path |
| --- | --- | --- |
| `http://127.0.0.1:8080/v1` | `/v1/chat/completions` | `/v1/chat/completions` |
| `https://api.groq.com/openai/v1` | `/v1/chat/completions` | `/openai/v1/chat/completions` |
| `http://example.com/api` | `/v1/models` | `/api/v1/models` |

### Streaming smoke test

```bash
curl -N http://127.0.0.1:9090/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer sk-local' \
  -d '{
    "model": "gpt-4o-mini",
    "stream": true,
    "messages": [{"role": "user", "content": "Say hello in five words."}]
  }'
```

`-N` disables curl's output buffering. SSE events should appear as the
upstream produces them.

## Configuration

`config.example.json` is a working starting point:

```json
{
  "listen": "127.0.0.1:9090",
  "admin_listen": "127.0.0.1:9091",
  "max_attempts": 6,
  "max_request_bytes": 33554432,
  "upstream_header_timeout_seconds": 120,
  "max_cooldown_seconds": 600,
  "client_api_key": "",
  "log_level": "info",
  "retry_statuses": [429, 403, 502, 503],
  "cooldown_seconds": {
    "429": 30,
    "403": 120,
    "502": 10,
    "503": 15,
    "network": 10,
    "default": 15
  },
  "upstreams": [
    {
      "name": "opencode2api",
      "base_url": "http://127.0.0.1:8080/v1",
      "priority": 0
    },
    {
      "name": "openai-primary",
      "base_url": "https://api.openai.com/v1",
      "api_key": "${OPENAI_API_KEY}",
      "priority": 10
    }
  ]
}
```

### Top-level fields

| Field | Default | Meaning |
| --- | --- | --- |
| `listen` | `127.0.0.1:9090` | Proxy listen address |
| `admin_listen` | disabled | Separate status/control address; keep it on loopback |
| `max_attempts` | `6` | Maximum upstream attempts per request; `0` means try every entry |
| `max_request_bytes` | `33554432` (32 MiB) | Request body buffer limit; requests above it get `413` |
| `upstream_header_timeout_seconds` | `120` | Deadline for upstream response headers |
| `max_cooldown_seconds` | `600` | Ceiling for status/network cooldowns |
| `client_api_key` | empty | If set, clients must send this token on the proxy port |
| `log_level` | `info` | `quiet`, `info`, or `debug` |
| `retry_statuses` | `[429,403,502,503]` | HTTP statuses that trigger failover |
| `cooldown_seconds` | see above | Base cooldown per status, `network`, and `default` |
| `upstreams` | required | Non-empty pool (disabled entries are skipped) |

`retry_statuses` accepts error statuses only (`400`-`599`). Statuses not in
this list are passed through unchanged; a non-listed `5xx` is still cooled down
for future requests. An empty `retry_statuses` disables status-based failover
but keeps network-error failover.

### Upstream fields

| Field | Meaning |
| --- | --- |
| `name` | Stable label for logs, headers, and status; auto-generated if omitted |
| `base_url` | Required; `http` or `https`, with an optional path such as `/v1` |
| `api_key` | Injected as `<auth_header>: <auth_prefix><api_key>` |
| `auth_header` | Header used for the key; default `Authorization` |
| `auth_prefix` | Default `Bearer `; set `""` for raw-key headers |
| `priority` | Lower number is preferred; equal priorities round-robin |
| `enabled` | Set `false` to keep an entry in the file but not use it |
| `headers` | Extra static headers for this upstream (values support env vars) |

`base_url`, `api_key`, header values, `listen`, `admin_listen`, and
`client_api_key` expand `${ENV_VAR}` and `$ENV_VAR`. Export variables before
starting the proxy. Keep real keys out of the config file when possible:

```bash
chmod 600 config.json
export OPENAI_API_KEY='sk-...'
```

The config is read once at startup; restart the proxy to apply changes.

If an upstream has no `api_key`, the client's `Authorization` header is
passed through. This is what local gateways usually want.

### Multiple keys for one provider

Add one upstream entry per key. They can share a URL and priority; the pool
will round-robin between them and cool down only the failing key:

```json
{
  "name": "openai-key-1",
  "base_url": "https://api.openai.com/v1",
  "api_key": "${OPENAI_API_KEY_1}",
  "priority": 10
},
{
  "name": "openai-key-2",
  "base_url": "https://api.openai.com/v1",
  "api_key": "${OPENAI_API_KEY_2}",
  "priority": 10
}
```

### Custom auth headers

Azure-style APIs often use `api-key` with no `Bearer` prefix:

```json
{
  "name": "custom-auth-provider",
  "base_url": "https://api.example.com/v1",
  "api_key": "${CUSTOM_API_KEY}",
  "auth_header": "api-key",
  "auth_prefix": "",
  "priority": 20
}
```

Static provider headers can be added with `headers`:

```json
"headers": {
  "HTTP-Referer": "http://localhost",
  "X-Title": "Termux Proxy Rotator"
}
```

## Failover behavior

On a retryable response the proxy:

1. Reads a small error snippet (for the log), closes that response, and
   records a cooldown for the pool entry.
2. Selects the next healthy entry: lowest priority tier, round-robin within it.
3. Replays the buffered request body with the next entry's credentials.
4. Commits to the client only after an acceptable response arrives.

Cooldown per failure is `base cooldown * 2^(consecutive failures - 1)`, capped
by `max_cooldown_seconds`. A valid `Retry-After` header overrides the
exponential backoff. A later successful response resets the entry immediately.

If every entry is cooling down, one forced attempt is made on the entry that
recovers soonest. If that also fails, the client gets an OpenAI-style error:

- `429` (with `Retry-After`) when the last failure was rate limiting
- `502` otherwise

Retries happen only **before** response bytes are committed. Once a `200` SSE
stream has started, a mid-stream upstream failure cannot be replayed without
corrupting the stream; the proxy closes the stream and logs it. Client
disconnects cancel the upstream request.

A retried `POST` may be processed twice by an upstream in the unlikely case
that the upstream generated a completion and then the connection failed. For
strict billing-sensitive workloads, keep `max_attempts` modest.

## Streaming details

- `Accept-Encoding` is removed and upstream compression is disabled, so SSE
  chunks are not held in a gzip buffer.
- Response headers are flushed immediately, then every read chunk is written
  and flushed. Chunked transfer encoding is handled by `net/http`.
- There are intentionally no server read/write timeouts: long prompts and
  streams are allowed. The upstream response-header timeout is configurable.
  Your AI client's own timeout still applies.
- Request bodies are buffered so they can be replayed. Increase
  `max_request_bytes` for large audio uploads.

## Logs

Example:

```
14:03:22.417  INFO    proxy-rotator 1.0.0 starting (config: config.json)
14:03:22.418  INFO    pool: 3 upstream(s), up to 3 attempt(s) per request
14:03:22.419  INFO    listening on http://127.0.0.1:9090
14:03:22.419  INFO    admin on http://127.0.0.1:9091/status (POST /reset resets cooldowns)
14:03:25.002  REQ     [a1b2c3d4e5]  POST /v1/chat/completions  model=gpt-4o-mini stream=true
14:03:25.111  FAIL    [a1b2c3d4e5]  openai-primary -> 429 Too Many Requests (attempt 1/3): rate limit exceeded; cooling 30s, rotating
14:03:26.908  OK      [a1b2c3d4e5]  openai-secondary 200 | 1.4 KiB | 1.80s sse
```

Notes:

- `REQ`, `OK`, and `REPLY` lines are suppressed by `log_level: "quiet"`.
  Failovers, warnings, errors, and startup lines are always printed.
- `log_level: "debug"` adds pool entries, selection details, and extra health
  messages.
- ANSI colors are used only on a terminal and are disabled by `NO_COLOR`.
- API keys are never logged. Auth header injection and proxy-token stripping
  are performed server-side.

## Status endpoint

When `admin_listen` is set:

```bash
pkg install -y jq   # optional, for pretty status output
curl -s http://127.0.0.1:9091/status | jq
curl -s http://127.0.0.1:9091/healthz
curl -X POST http://127.0.0.1:9091/reset
```

`/status` returns uptime, requests, active requests, failover count, and each
upstream's health, cooldown, attempts, successes, failures, last status, last
error, and last latency. `/reset` clears cooldowns and consecutive failure
counts; cumulative counters remain.

The admin endpoint has no authentication. Leave it on `127.0.0.1` or disable
it with `"admin_listen": ""`.

The proxy port also exposes `/` and `/healthz` for a quick check, but those are
the only reserved forwarding paths.

## Run it in the background on Android

Termux processes are stopped when Android kills the app. For long-running use:

```bash
pkg install -y tmux
termux-wake-lock

# Start a persistent session
tmux new -s rotator
cd ~/termux-proxy-rotator
export OPENAI_API_KEY='sk-...'
./rotator -config config.json
# detach with Ctrl-b then d
```

Reattach later with `tmux attach -t rotator`.

To start automatically after a reboot, install **Termux:Boot** from F-Droid,
open it once, then create `~/.termux/boot/start-rotator.sh`:

```bash
#!/data/data/com.termux/files/usr/bin/sh
termux-wake-lock
cd "$HOME/termux-proxy-rotator" || exit 1
export OPENAI_API_KEY='sk-...'
nohup ./rotator -config config.json >> "$HOME/rotator.log" 2>&1 &
```

```bash
chmod +x ~/.termux/boot/start-rotator.sh
```

Disable battery optimization for Termux in Android settings. Logs then go to
`~/rotator.log`. Stop a background instance with `pkill -f rotator` or from its
tmux session.

## Security notes

- The defaults bind to loopback, which is the intended deployment.
- Set `client_api_key` if any other app or device could reach the proxy port.
  The proxy compares it in constant time and strips it from forwarded requests
  so it is not leaked upstream.
- Never expose the admin port. It is unauthenticated by design.
- Prefer environment variables over literal keys in `config.json`; keep the
  config readable only by your user (`chmod 600`).
- Upstream URLs use the system trust store and HTTPS when configured. There is
  no option to disable TLS verification.
- This is a local convenience proxy, not a hardened multi-tenant gateway.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| `bind: address already in use` | Another instance is running: `pkill -f rotator`, or change `listen` |
| Upstream `401` after startup | Set the provider key or export the referenced environment variable; the warning at startup names entries with no key |
| Constant `429` from the proxy | All entries are rate-limited; inspect `/status`, add keys, or wait for cooldowns |
| Streaming looks buffered | Test with `curl -N`; the proxy flushes per chunk, but clients can buffer on their side |
| Large upload gets `413` | Raise `max_request_bytes` |
| Works in tmux, dies later | Run `termux-wake-lock` and disable battery optimization |
| Admin endpoint refused | `admin_listen` is empty, or the process is not running |
| Local gateway unreachable | Test it directly: `curl http://127.0.0.1:8080/v1/models` |
| Upstream TLS `x509: certificate signed by unknown authority` | Refresh Termux certificates: `pkg install ca-certificates` (or `pkg upgrade`) |
| A `400` is not rotated | Only `retry_statuses` and network errors trigger failover; add the status if appropriate |
| Self-signed upstream TLS error | Use a trusted certificate; verification cannot be disabled |

## Limitations

- HTTP forwarding only (including SSE and chunked responses). WebSocket /
  OpenAI Realtime API proxying is not implemented.
- Request bodies are buffered to allow replay; very large streaming uploads are
  therefore limited by `max_request_bytes`.
- No background health probes or proactive key validation; cooldowns are
  learned from real requests.
- No per-model routing, per-key rate limiting, or response transformation.

## Development

```bash
go test ./...     # unit + end-to-end tests over httptest
go vet ./...
make build
```

Tests cover path joining, priority round-robin, cooldown backoff and
`Retry-After`, 403/429/502/503 failover, network-error failover, body replay,
SSE flush behavior, client auth token stripping, and the admin endpoints.

## License

MIT. See `LICENSE`.
