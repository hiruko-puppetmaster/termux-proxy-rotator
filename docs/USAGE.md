# Detailed usage guide

This guide walks through installing, configuring, testing, and operating the
proxy on Android/Termux. The short version is in [README.md](../README.md);
this document focuses on doing it correctly in real-world setups.

## Automatic OpenCode setup (recommended)

If you already use OpenCode in Termux, the installer can build the proxy,
detect your local upstreams, start the background service, and register the
provider in one command:

```bash
curl -fsSL https://raw.githubusercontent.com/hiruko-puppetmaster/termux-proxy-rotator/main/install-opencode.sh |
  bash
```

You can also clone the repository and run the installer locally:

```bash
git clone https://github.com/hiruko-puppetmaster/termux-proxy-rotator.git
cd termux-proxy-rotator
bash install-opencode.sh
```

### What it changes

The installer writes two kinds of files: the proxy runtime and the OpenCode
configuration.

| Path | Purpose |
| --- | --- |
| `~/.local/bin/proxy-rotator` | Proxy binary (`$PREFIX/bin` when writable) |
| `~/.local/bin/opencode-apply` | JSONC/config helper used by the installer |
| `~/.config/opencode/rotator/config.json` | Generated pool config, mode `600` |
| `~/.config/opencode/rotator/env.sh` | Upstream and client keys, mode `600` |
| `~/.config/opencode/rotator/client.key` | Random proxy token, mode `600` |
| `~/.config/opencode/rotator/start.sh` | Starts the proxy in the background |
| `~/.config/opencode/rotator/stop.sh` | Stops the proxy |
| `~/.config/opencode/rotator/rotator.log` | Request and failover log |
| `~/.config/opencode/opencode.jsonc` | Adds `providers.rotator` and the default model |
| `~/.config/opencode/opencode.jsonc.bak` | First-run backup, kept for reverting |

Upstream keys are expanded from `env.sh` at runtime; they are not copied into
`config.json`. The proxy still listens on loopback only and requires the random
`client.key` on its port.

### Upstream detection order

1. If `ROTATOR_UPSTREAM_URL` is set, the installer uses that upstream. It also
   prompts for the key when the URL is not loopback and no key was supplied.
2. Otherwise it looks for an opencode2api gateway config in
   `~/opencode2api/config.json`, `~/.config/opencode2api/config.json`,
   `~/.local/share/opencode2api/config.json`, and the Termux home fallback
   `/data/data/com.termux/files/home/opencode2api/config.json`. It reads
   `server_keys[0]` and builds the URL from `listen`.
3. It looks for an OpenCode Zen key in
   `$XDG_DATA_HOME/opencode/auth.json`, `~/.local/share/opencode/auth.json`, and
   the Termux home fallback.
4. If both are found, opencode2api gets priority `0` and direct Zen gets
   priority `10`, so the local gateway is preferred and direct Zen is the
   fallback.
5. If nothing is found, the installer prompts for a base URL and key. Without
   a terminal it exits with instructions for a non-interactive install.

`ROTATOR_ADD_DETECTED=1` adds the detected opencode2api and Zen entries after a
custom `ROTATOR_UPSTREAM_URL`, with priorities `10` and `20`.

### Custom or unattended install

```bash
curl -fsSL https://raw.githubusercontent.com/hiruko-puppetmaster/termux-proxy-rotator/main/install-opencode.sh |
  ROTATOR_UPSTREAM_URL='https://api.example.com/v1' \
  ROTATOR_UPSTREAM_KEY='sk-...' bash
```

The same values can be passed as flags when running a local checkout:

```bash
bash install-opencode.sh \
  --upstream-url 'https://api.example.com/v1' \
  --upstream-key 'sk-...'
```

### Installer options

| Option | Environment variable | Meaning |
| --- | --- | --- |
| `--keep-default` | `ROTATOR_KEEP_DEFAULT=1` | Do not change OpenCode's default model |
| `--port N` | `ROTATOR_PORT=N` | Proxy port (default `9090`) |
| `--admin-port N` | `ROTATOR_ADMIN_PORT=N` | Admin/status port (default `9091`) |
| `--runtime DIR` | `ROTATOR_HOME=DIR` | Runtime directory |
| `--bin-dir DIR` | `ROTATOR_BIN_DIR=DIR` | Where to install the binaries |
| `--config PATH` | `OPENCODE_CONFIG=PATH` | OpenCode config file to edit |
| `--source DIR` | `ROTATOR_SOURCE=DIR` | Build from a local checkout |
| `--upstream-url URL` | `ROTATOR_UPSTREAM_URL=URL` | Use this upstream |
| `--upstream-key KEY` | `ROTATOR_UPSTREAM_KEY=KEY` | Key for the custom upstream |
| `--prefer-model ID` | `ROTATOR_PREFER_MODEL=ID` | Preferred default model |
| `--default-model ID` | `ROTATOR_DEFAULT_MODEL=ID` | Required default model |
| `--no-start` | `ROTATOR_SKIP_START=1` | Do not start the proxy |
| `--no-deps` | `ROTATOR_SKIP_DEPS=1` | Do not run `pkg install` |
| `--no-verify` | `ROTATOR_SKIP_VERIFY=1` | Skip the final `opencode models` check |
| — | `ROTATOR_ADD_DETECTED=1` | Add detected upstreams after a custom one |
| — | `ROTATOR_MODELS_FILE=PATH` | Use a saved `/v1/models` response (testing) |

The installer is idempotent. Rerunning it rebuilds the binaries, stops the old
proxy, regenerates the runtime files, starts the proxy again, and updates the
provider block. The first `opencode.jsonc.bak` is preserved so it still
represents the configuration from before the first install.

### Default model selection

Unless `--keep-default` is used, the installer sets the top-level `"model"`
value to `rotator/<model>`. It chooses in this order:

1. `--default-model ID`, which fails if the model is absent,
2. an existing `rotator/<model>` default that is still present in the catalog,
3. `--prefer-model ID` (default `deepseek-v4.1-flash-free`),
4. the first model in the sorted catalog.

All models returned by the rotator's `/v1/models` endpoint are registered as
OpenCode models. Metadata such as context limits, input modalities, tool use,
and reasoning efforts is copied from the catalog.

### Verify the result

```bash
~/.config/opencode/rotator/start.sh
curl -s http://127.0.0.1:9091/status

opencode models | grep '^rotator/'
opencode run -m rotator/deepseek-v4.1-flash-free "Reply with exactly: OK"
opencode run "Reply with exactly: DEFAULT OK"
```

The installer runs `opencode models` automatically when `opencode` is on
`PATH`. If OpenCode was already running, it normally hot-reloads the provider;
restart it if the new models do not appear.

### Revert or uninstall

Restore the pre-install OpenCode configuration:

```bash
cp ~/.config/opencode/opencode.jsonc.bak ~/.config/opencode/opencode.jsonc
```

To keep the provider but use another default, remove the top-level `"model"`
line or set it to the model you want. To remove the runtime entirely:

```bash
~/.config/opencode/rotator/stop.sh
rm -rf ~/.config/opencode/rotator
rm -f ~/.local/bin/proxy-rotator ~/.local/bin/opencode-apply
```

### Automatic setup troubleshooting

| Symptom | Check |
| --- | --- |
| `pkg: command not found` | Run the installer from Termux, not another Linux shell |
| `pkg` refuses to run as root | Do not run the installer as root; use the normal Termux user |
| `bind: address already in use` | Change `--port`/`--admin-port`, or stop the old instance |
| No upstream detected | Set `ROTATOR_UPSTREAM_URL`/`ROTATOR_UPSTREAM_KEY`, or run interactively |
| Model catalog returns `401` | The gateway key changed; rerun the installer so `env.sh` is refreshed |
| `opencode models` does not show `rotator/*` | Check `--config`/`OPENCODE_CONFIG`, then restart OpenCode |
| Default model is rejected | Rerun with `--prefer-model` naming a model from `opencode models` |
| Proxy did not start | Read `~/.config/opencode/rotator/rotator.log` |
| `--no-start` left OpenCode unchanged | Start the proxy, then rerun the installer; or pass `ROTATOR_MODELS_FILE` |

### Security notes for the automatic setup

- `env.sh`, `client.key`, and `config.json` are mode `600` and live in your
  home directory.
- The proxy and admin ports bind to `127.0.0.1`.
- The admin endpoint is unauthenticated; never expose it on a network.
- The generated provider uses a file reference (`{file:...}`) for the proxy
  token, so the token is not duplicated in `opencode.jsonc`.

## 1. What the proxy does

The proxy accepts OpenAI-compatible HTTP requests on loopback and forwards
each request to one upstream from your pool. If the upstream fails in a way you
configured as retryable, the proxy selects another pool entry and replays the
request before returning anything to the client.

It does **not**:

- convert between different API formats (requests must be OpenAI-compatible),
- proxy WebSocket/OpenAI Realtime traffic,
- guarantee that a retried `POST` was never processed by the failed upstream,
- make an invalid API key valid, or bypass provider quotas.

It **does**:

- rotate API keys and endpoints,
- cool down failing entries,
- fail over on `429`, `403`, `502`, `503`, and network errors by default,
- stream SSE chunks to the client as they arrive,
- expose local health and pool status.

## 2. Install Termux and the Go toolchain

Install Termux from F-Droid or the official Termux GitHub releases. The Play
Store build is obsolete.

```bash
pkg update
pkg install -y golang git
```

Optional but useful:

```bash
pkg install -y jq tmux ca-certificates
```

`jq` makes the status endpoint easier to read, `tmux` keeps the proxy alive
while the Termux UI is closed, and `ca-certificates` refreshes TLS roots.

Check Go:

```bash
go version   # needs Go 1.22 or newer
```

## 3. Get the project

Clone the repository:

```bash
cd ~
git clone https://github.com/hiruko-puppetmaster/termux-proxy-rotator.git
cd termux-proxy-rotator
```

If you copied files manually, simply `cd` into the directory containing
`go.mod` and `main.go`.

Build it:

```bash
go build -trimpath -ldflags="-s -w" -o rotator .
```

The result is one self-contained binary. No Node, Python, or system libraries
are required at runtime.

## 4. First configuration

Start from the example:

```bash
cp config.example.json config.json
chmod 600 config.json
nano config.json
```

The example contains:

1. a local opencode2api gateway at priority `0`,
2. an OpenAI key at priority `10`,
3. disabled examples for a second OpenAI key, OpenRouter, and Groq.

Set environment variables for any `${...}` references:

```bash
export OPENAI_API_KEY='sk-...'
# export OPENAI_API_KEY_2='sk-...'
# export OPENROUTER_API_KEY='sk-or-...'
# export GROQ_API_KEY='gsk_...'
```

If you do not use an optional entry, leave `"enabled": false`. If you enable it
without setting its variable, the proxy warns that no key was resolved.

Validate before starting:

```bash
./rotator -check -config config.json
```

`-check` loads and validates the JSON, expands environment variables, prints
the effective pool, and exits without binding a port. Fix any error it prints
before continuing.

Start the proxy:

```bash
./rotator -config config.json
```

Expected startup lines:

```
03:00:00.000  INFO    proxy-rotator 1.0.0 starting (config: config.json)
03:00:00.001  INFO    pool: 2 upstream(s), up to 2 attempt(s) per request
03:00:00.002  INFO    listening on http://127.0.0.1:9090
03:00:00.002  INFO    admin on http://127.0.0.1:9091/status (POST /reset resets cooldowns)
```

## 5. Choose a pool layout

### A. Local gateway first, cloud as fallback

This is useful when opencode2api or another local gateway is preferred, but
you want cloud capacity when it is down:

```json
"upstreams": [
  {
    "name": "local",
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
```

Notes:

- The local entry has no `api_key`, so the client's `Authorization` header is
  passed through. Local gateways usually ignore it.
- Cloud entries inject their own key. Client credentials are removed before the
  configured key is added.
- Priority `0` is always preferred while healthy.

### B. Several keys for the same provider

Use one entry per key, with equal priority so they round-robin:

```json
"upstreams": [
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
]
```

If key 1 returns `429`, it is cooled down while key 2 continues serving.
Cooldowns are per entry, so only the failing key is removed from rotation.

### C. Multiple providers with tiers

```json
"upstreams": [
  {
    "name": "local",
    "base_url": "http://127.0.0.1:8080/v1",
    "priority": 0
  },
  {
    "name": "openai-a",
    "base_url": "https://api.openai.com/v1",
    "api_key": "${OPENAI_API_KEY_A}",
    "priority": 10
  },
  {
    "name": "openai-b",
    "base_url": "https://api.openai.com/v1",
    "api_key": "${OPENAI_API_KEY_B}",
    "priority": 10
  },
  {
    "name": "openrouter",
    "base_url": "https://openrouter.ai/api/v1",
    "api_key": "${OPENROUTER_API_KEY}",
    "priority": 20,
    "headers": {
      "HTTP-Referer": "http://localhost",
      "X-Title": "Termux Proxy Rotator"
    }
  }
]
```

Selection order while everything is healthy:

1. round-robin between `local` (only entry at priority 0),
2. otherwise round-robin between the two OpenAI keys,
3. otherwise OpenRouter.

A lower priority tier is skipped as soon as at least one member of a higher
tier is healthy.

### D. Protect the local proxy

If other apps or devices can reach your proxy port, set a client token:

```json
"client_api_key": "${PROXY_CLIENT_KEY}"
```

Then configure your AI client with:

- Base URL: `http://127.0.0.1:9090/v1`
- API key: the value of `PROXY_CLIENT_KEY`

The proxy accepts it as `Authorization: Bearer ...` or `X-Api-Key: ...`,
compares it in constant time, and strips it before forwarding so it is not
leaked upstream.

A client token is not a replacement for a firewall. Keep the listener on
`127.0.0.1` unless you know exactly why you are changing it.

## 6. Connect an AI client

Any tool that supports a custom OpenAI base URL can use the proxy. The two
required settings are:

```
Base URL: http://127.0.0.1:9090/v1
API key:  sk-local        # or your client_api_key
```

The model name is passed through unchanged, so use the model IDs accepted by
the upstream that ultimately serves the request. If your upstreams do not all
support the same model, the request may fail with `400` after failover; add or
remove model-specific entries accordingly.

### curl

Non-streaming:

```bash
curl http://127.0.0.1:9090/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer sk-local' \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role": "user", "content": "Reply with OK"}]
  }'
```

Streaming (`-N` disables curl's own buffering):

```bash
curl -N http://127.0.0.1:9090/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer sk-local' \
  -d '{
    "model": "gpt-4o-mini",
    "stream": true,
    "messages": [{"role": "user", "content": "Count from 1 to 5."}]
  }'
```

### Python OpenAI SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:9090/v1",
    api_key="sk-local",
)

stream = client.chat.completions.create(
    model="gpt-4o-mini",
    messages=[{"role": "user", "content": "Say hello."}],
    stream=True,
)
for chunk in stream:
    print(chunk.choices[0].delta.content or "", end="", flush=True)
```

### Other coding assistants

Look for **OpenAI-compatible**, **custom base URL**, or **custom endpoint**
settings. Enter `http://127.0.0.1:9090/v1` as the base URL and a placeholder or
client token as the key. Do not append `/chat/completions` to the base URL
unless that tool explicitly asks for a full completion URL.

## 7. Verify failover is working

The most reliable test is to use an upstream that returns one of the retryable
statuses. You can observe this safely with a local mock rather than burning
real quota.

Create `mock.py` in a separate terminal:

```python
from http.server import BaseHTTPRequestHandler, HTTPServer

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        self.send_response(429)
        self.send_header("Retry-After", "10")
        self.end_headers()
        self.wfile.write(b'{"error":{"message":"mock limit"}}')
    def log_message(self, *args):
        pass

HTTPServer(("127.0.0.1", 18080), H).serve_forever()
```

Run it with `python3 mock.py`, add this upstream at priority 0 in a test
config, keep a real provider at priority 10, then call the proxy. The log
should show:

```
FAIL ... mock -> 429 Too Many Requests (attempt 1/2): mock limit; cooling 10.00s, rotating
OK   ... real-provider 200 | ... sse
```

Check the pool state:

```bash
curl -s http://127.0.0.1:9091/status | jq '.upstreams[] | {name, healthy, cooldown_remaining_seconds, last_status}'
```

The mock entry should be unhealthy with a cooldown; the real entry should be
healthy. After the cooldown, the mock is eligible again; if it keeps failing,
the cooldown grows exponentially.

Do not use invalid real API keys for testing unless you are prepared for
provider alerts or account restrictions.

## 8. Understand logs and status

Logs go to stdout and always include a millisecond timestamp.

| Label | Meaning | Quiet mode |
| --- | --- | --- |
| `INFO` | startup, configuration, lifecycle | shown |
| `REQ` | an incoming proxied request | hidden |
| `OK` | completed response below 400 | hidden |
| `REPLY` | passthrough 4xx response | hidden |
| `FAIL` | retryable failure and rotation | shown |
| `WARN` | config/security warnings, forced attempts | shown |
| `ERROR` | exhausted pool, server errors | shown |
| `DEBUG` | pool details and internal decisions | debug only |

Set `"log_level"` to `quiet`, `info` (default), or `debug`.

Logs never contain API keys. Client request bodies are not logged; only the
path, model, and `stream` flag are shown.

The admin status endpoint reports:

- process uptime and load counters,
- total proxy requests and failovers,
- per-upstream health, cooldown remaining, attempts, successes, failures,
- last HTTP status, last error, last use time, and last latency.

Admin commands:

```bash
curl -s http://127.0.0.1:9091/status | jq
curl -s http://127.0.0.1:9091/healthz
curl -X POST http://127.0.0.1:9091/reset
```

`/reset` clears cooldowns and consecutive failure counts immediately. It does
not clear cumulative counters or change configuration.

## 9. Tuning

### `max_attempts`

Number of upstream attempts allowed for one client request. The effective
value is capped by the number of pool entries.

- Default `6` is good for small pools.
- Use `0` to try every entry.
- Larger values increase resilience but also latency when everything is down.
- Retries stop at the first non-retryable response.

### Priority strategy

- Put the cheapest/fastest local gateway at `0`.
- Put paid keys at `10`-`29`.
- Put backup providers at `30`+.
- Equal priorities are load-balanced round-robin.
- A lower tier is only used when no higher-tier entry is healthy.

### Cooldowns

```json
"cooldown_seconds": {
  "429": 30,
  "403": 120,
  "502": 10,
  "503": 15,
  "network": 10,
  "default": 15
}
```

Cooldown grows as `base * 2^(consecutive failures - 1)` and is capped by
`max_cooldown_seconds`. A valid upstream `Retry-After` header overrides the
backoff growth.

Use larger cooldowns for keys with strict quotas. Use shorter cooldowns if the
provider recovers quickly or if you need aggressive rotation.

### `retry_statuses`

Defaults: `[429, 403, 502, 503]`. Add `401` if rotating bad keys is desirable
in your setup. Remove `403` if that provider uses `403` for request policy
errors that another key cannot fix.

### Timeouts and body limits

- `upstream_header_timeout_seconds` (default 120): raise this if slow/reasoning
  models take longer to send response headers.
- `max_request_bytes` (default 32 MiB): the body is buffered for replay.
  Raise it for large audio uploads; lower it to reduce peak memory on phones.

There are intentionally no server read/write timeouts so long streams can run.
Your AI client should still have its own timeout.

## 10. Keep it running on Android

Android can freeze or kill background processes. For reliable operation:

```bash
pkg install -y tmux
termux-wake-lock

tmux new -s rotator
cd ~/termux-proxy-rotator
export OPENAI_API_KEY='sk-...'
./rotator -config config.json
```

Detach with `Ctrl-b`, then `d`. Reattach with:

```bash
tmux attach -t rotator
```

Also disable battery optimization for Termux in Android system settings.

### Start after reboot

Install Termux:Boot from F-Droid and open it once. Create:

`~/.termux/boot/start-rotator.sh`

```bash
#!/data/data/com.termux/files/usr/bin/sh
termux-wake-lock
cd "$HOME/termux-proxy-rotator" || exit 1
export OPENAI_API_KEY='sk-...'
nohup ./rotator -config config.json >> "$HOME/rotator.log" 2>&1 &
```

Then:

```bash
chmod +x ~/.termux/boot/start-rotator.sh
mkdir -p ~/.termux/boot
```

Logs go to `~/rotator.log`. Stop the process with `pkill -f rotator` or from
its tmux session.

## 11. Security checklist

- Keep `listen` and `admin_listen` on `127.0.0.1`.
- Set `client_api_key` if any other app/device can reach the proxy port.
- Keep `config.json` at mode `600` and prefer environment variables for keys.
- Do not expose the admin endpoint; it has no authentication.
- Use HTTPS upstreams. TLS verification is always enabled.
- Do not commit `config.json`, `.env`, logs, or the binary to Git. The included
  `.gitignore` excludes `config.json`, logs, and build outputs.
- Treat the proxy as a local convenience service, not a multi-tenant gateway.

## 12. Troubleshooting

### The proxy starts, but every request returns 401

The upstream rejected the credential. Check:

```bash
./rotator -check -config config.json
```

Look for warnings that an environment variable resolved to an empty key. For a
configured upstream key, the client's key is intentionally removed and
replaced.

### All requests return 403

Some providers use `403` for regional restrictions or policy blocks. Rotating
keys may not help. Check `/status` for which entry is being used and inspect the
provider's response through the client.

### All requests return 429

Every healthy entry is rate-limited. Add keys/providers, wait for cooldowns, or
inspect `/status`. When the whole pool is exhausted, the proxy returns `429`
with `Retry-After` if the last failure was a rate limit.

### Streaming is slow to start but eventually completes

This is usually model latency, not proxy buffering. Verify with `curl -N`. The
proxy flushes after response headers and after every read chunk.

### `413 Request Entity Too Large`

Raise `max_request_bytes`. The default is 32 MiB.

### `bind: address already in use`

Another instance is running:

```bash
pkill -f rotator
```

Or change the configured port.

### Upstream TLS certificate error

Refresh certificates:

```bash
pkg install ca-certificates
pkg upgrade
```

TLS verification cannot be disabled in this tool.

### The process dies when Termux is closed

Use `termux-wake-lock`, tmux, and Termux:Boot as described above. Also disable
Android battery optimization for Termux.

## 13. Updating

```bash
cd ~/termux-proxy-rotator
git pull
go build -trimpath -ldflags="-s -w" -o rotator .
pkill -f rotator
./rotator -config config.json
```

`config.json` is ignored by Git, so pulling updates will not overwrite it.
Re-run `./rotator -check` after updating because new versions may add
validation or configuration fields.

## 14. Uninstalling

Stop the process and remove the directory:

```bash
pkill -f rotator
rm -rf ~/termux-proxy-rotator
```

Remove the boot script if you created one:

```bash
rm -f ~/.termux/boot/start-rotator.sh
```

Revoke or rotate any API keys that were configured on the device.
