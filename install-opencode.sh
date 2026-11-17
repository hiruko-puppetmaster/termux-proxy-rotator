#!/usr/bin/env bash
#
# Install and activate Termux Proxy Rotator for OpenCode.
#
# One-liner:
#   curl -fsSL https://raw.githubusercontent.com/hiruko-puppetmaster/termux-proxy-rotator/main/install-opencode.sh | bash
#
# The installer builds the proxy, detects local upstream credentials, starts a
# background service, and registers a "rotator" provider in OpenCode's JSONC
# configuration without removing comments or unrelated settings.
set -euo pipefail

REPO_URL="${ROTATOR_REPO_URL:-https://github.com/hiruko-puppetmaster/termux-proxy-rotator.git}"
REPO_REF="${ROTATOR_REF:-main}"
VERSION="1.0.0"

KEEP_DEFAULT="${ROTATOR_KEEP_DEFAULT:-0}"
SKIP_DEPS="${ROTATOR_SKIP_DEPS:-0}"
SKIP_START="${ROTATOR_SKIP_START:-0}"
SKIP_VERIFY="${ROTATOR_SKIP_VERIFY:-0}"
PORT="${ROTATOR_PORT:-9090}"
ADMIN_PORT="${ROTATOR_ADMIN_PORT:-9091}"
RUNTIME_DIR="${ROTATOR_HOME:-}"
BIN_DIR="${ROTATOR_BIN_DIR:-}"
SRC_DIR="${ROTATOR_SOURCE:-}"
OPENCODE_CONF="${OPENCODE_CONFIG:-}"
MODELS_FILE="${ROTATOR_MODELS_FILE:-}"
PREFER_MODEL="${ROTATOR_PREFER_MODEL:-deepseek-v4.1-flash-free}"
EXPLICIT_DEFAULT="${ROTATOR_DEFAULT_MODEL:-}"

say()  { printf '  %s\n' "$*"; }
step() { printf '\n[%s] %s\n' "$1" "$2"; }
warn() { printf '  warning: %s\n' "$*" >&2; }
die()  { printf '\nerror: %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<EOF
Termux Proxy Rotator installer $VERSION

Usage:
  bash install-opencode.sh [options]

Options:
  --keep-default          Do not change OpenCode's default model
  --port N                Proxy port (default: 9090)
  --admin-port N          Admin/status port (default: 9091)
  --runtime DIR           Runtime directory (default: ~/.config/opencode/rotator)
  --bin-dir DIR           Binary directory (default: \$PREFIX/bin or ~/.local/bin)
  --config PATH           OpenCode config file (default: ~/.config/opencode/opencode.jsonc)
  --source DIR            Build from a local checkout instead of cloning
  --upstream-url URL      Use this upstream instead of auto-detected ones
  --upstream-key KEY      API key for --upstream-url
  --prefer-model ID       Preferred default model (falls back if missing)
  --default-model ID      Require this default model (fails if missing)
  --no-start              Install files but do not start the proxy
  --no-deps               Do not run pkg install for missing build tools
  --no-verify             Skip the final "opencode models" check
  -h, --help              Show this help

Environment equivalents: ROTATOR_PORT, ROTATOR_ADMIN_PORT, ROTATOR_HOME,
ROTATOR_BIN_DIR, OPENCODE_CONFIG, ROTATOR_SOURCE, ROTATOR_UPSTREAM_URL,
ROTATOR_UPSTREAM_KEY, ROTATOR_KEEP_DEFAULT, ROTATOR_SKIP_DEPS,
ROTATOR_SKIP_START, ROTATOR_MODELS_FILE, ROTATOR_ADD_DETECTED=1.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--keep-default) KEEP_DEFAULT=1; shift ;;
	--port) PORT="${2:?--port needs a value}"; shift 2 ;;
	--admin-port) ADMIN_PORT="${2:?--admin-port needs a value}"; shift 2 ;;
	--runtime) RUNTIME_DIR="${2:?--runtime needs a value}"; shift 2 ;;
	--bin-dir) BIN_DIR="${2:?--bin-dir needs a value}"; shift 2 ;;
	--config) OPENCODE_CONF="${2:?--config needs a value}"; shift 2 ;;
	--source) SRC_DIR="${2:?--source needs a value}"; shift 2 ;;
	--upstream-url) ROTATOR_UPSTREAM_URL="${2:?--upstream-url needs a value}"; shift 2 ;;
	--upstream-key) ROTATOR_UPSTREAM_KEY="${2:?--upstream-key needs a value}"; shift 2 ;;
	--prefer-model) PREFER_MODEL="${2:?--prefer-model needs a value}"; shift 2 ;;
	--default-model) EXPLICIT_DEFAULT="${2:?--default-model needs a value}"; shift 2 ;;
	--no-start) SKIP_START=1; shift ;;
	--no-deps) SKIP_DEPS=1; shift ;;
	--no-verify) SKIP_VERIFY=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown option: $1 (use --help)" ;;
	esac
done

is_true() {
	case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
	1 | true | yes | on) return 0 ;;
	*) return 1 ;;
	esac
}

if is_true "$KEEP_DEFAULT"; then KEEP_DEFAULT=1; else KEEP_DEFAULT=0; fi
if is_true "$SKIP_DEPS"; then SKIP_DEPS=1; else SKIP_DEPS=0; fi
if is_true "$SKIP_START"; then SKIP_START=1; else SKIP_START=0; fi
if is_true "$SKIP_VERIFY"; then SKIP_VERIFY=1; else SKIP_VERIFY=0; fi

for value in "$PORT" "$ADMIN_PORT"; do
	case "$value" in
	'' | *[!0-9]*) die "ports must be numbers (got $value)" ;;
	esac
	if [ "$value" -lt 1 ] || [ "$value" -gt 65535 ]; then
		die "port out of range: $value"
	fi
done
[ "$PORT" != "$ADMIN_PORT" ] || die "the proxy and admin ports must differ"

HOME="${HOME:-}"
[ -n "$HOME" ] || die "HOME is not set"
CONFIG_HOME="${XDG_CONFIG_HOME:-$HOME/.config}"

if [ -z "$RUNTIME_DIR" ]; then
	RUNTIME_DIR="$CONFIG_HOME/opencode/rotator"
fi
if [ -z "$BIN_DIR" ]; then
	if [ -n "${PREFIX:-}" ] && [ -d "$PREFIX/bin" ] && [ -w "$PREFIX/bin" ]; then
		BIN_DIR="$PREFIX/bin"
	else
		BIN_DIR="$HOME/.local/bin"
	fi
fi

expand_home() {
	case "$1" in
	"~") printf '%s' "$HOME" ;;
	"~/"*) printf '%s/%s' "$HOME" "${1#\~/}" ;;
	*) printf '%s' "$1" ;;
	esac
}

RUNTIME_DIR="$(expand_home "$RUNTIME_DIR")"
BIN_DIR="$(expand_home "$BIN_DIR")"
SRC_DIR="$(expand_home "$SRC_DIR")"
OPENCODE_CONF="$(expand_home "$OPENCODE_CONF")"
MODELS_FILE="$(expand_home "$MODELS_FILE")"

printf '\nTermux Proxy Rotator installer %s\n' "$VERSION"
printf 'OpenCode + automatic provider setup\n'

# ---------------------------------------------------------------- source ---

script_dir=""
if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
	script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fi

if [ -n "$SRC_DIR" ]; then
	[ -f "$SRC_DIR/go.mod" ] || die "ROTATOR_SOURCE does not look like the project: $SRC_DIR"
	SOURCE_KIND="local"
elif [ -n "$script_dir" ] && [ -f "$script_dir/go.mod" ] && [ -f "$script_dir/main.go" ]; then
	SRC_DIR="$script_dir"
	SOURCE_KIND="local"
else
	SRC_DIR="$HOME/.local/share/termux-proxy-rotator"
	SOURCE_KIND="clone"
fi

need_git=0
if [ "$SOURCE_KIND" = "clone" ]; then need_git=1; fi

missing=""
command -v go >/dev/null 2>&1 || missing="$missing go"
if [ "$need_git" = "1" ]; then
	command -v git >/dev/null 2>&1 || missing="$missing git"
fi

if [ -n "$missing" ]; then
	if [ "$SKIP_DEPS" = "1" ]; then
		die "missing required commands:$missing"
	fi
	if command -v pkg >/dev/null 2>&1; then
		step "1/6" "Installing Termux build tools:$missing"
		# shellcheck disable=SC2086
		if ! pkg install -y $missing; then
			die "pkg install failed; run: pkg install -y$missing"
		fi
	else
		die "missing required commands:$missing (install Go 1.22+, plus git when cloning)"
	fi
fi

if [ "$SOURCE_KIND" = "clone" ]; then
	step "1/6" "Fetching the latest source"
	if [ -d "$SRC_DIR/.git" ]; then
		dirty=0
		git -C "$SRC_DIR" diff --quiet 2>/dev/null || dirty=1
		git -C "$SRC_DIR" diff --cached --quiet 2>/dev/null || dirty=1
		if [ -n "$(git -C "$SRC_DIR" ls-files --others --exclude-standard 2>/dev/null)" ]; then
			dirty=1
		fi
		if [ "$dirty" = "1" ]; then
			warn "local changes in $SRC_DIR; using them without updating"
		elif GIT_TERMINAL_PROMPT=0 git -C "$SRC_DIR" fetch --depth 1 origin "$REPO_REF" >/dev/null 2>&1; then
			git -C "$SRC_DIR" reset --hard FETCH_HEAD >/dev/null 2>&1 || warn "could not reset $SRC_DIR; using the current checkout"
		else
			warn "could not update $SRC_DIR; using the existing checkout"
		fi
	else
		if [ -e "$SRC_DIR" ] && [ -n "$(ls -A "$SRC_DIR" 2>/dev/null)" ]; then
			die "$SRC_DIR exists but is not a git checkout; remove it or use --source"
		fi
		mkdir -p "$(dirname "$SRC_DIR")"
		if ! GIT_TERMINAL_PROMPT=0 git clone --depth 1 --branch "$REPO_REF" "$REPO_URL" "$SRC_DIR"; then
			rm -rf "$SRC_DIR"
			die "could not clone $REPO_URL"
		fi
	fi
	say "source: $SRC_DIR"
else
	step "1/6" "Using the local source checkout"
	say "source: $SRC_DIR"
fi

# ----------------------------------------------------------------- build ---

step "2/6" "Building proxy-rotator and opencode-apply"
mkdir -p "$BIN_DIR" || die "cannot create $BIN_DIR"
[ -w "$BIN_DIR" ] || die "$BIN_DIR is not writable"
PROXY_BIN="$BIN_DIR/proxy-rotator"
HELPER_BIN="$BIN_DIR/opencode-apply"

if ! (
	cd "$SRC_DIR"
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$PROXY_BIN" .
); then
	die "failed to build proxy-rotator"
fi
if ! (
	cd "$SRC_DIR"
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$HELPER_BIN" ./cmd/opencode-apply
); then
	die "failed to build opencode-apply"
fi
say "installed: $PROXY_BIN"
say "installed: $HELPER_BIN"

case ":$PATH:" in
*":$BIN_DIR:"*) ;;
*) warn "$BIN_DIR is not on PATH; use the absolute paths printed below" ;;
esac

# --------------------------------------------------------------- runtime ---

step "3/6" "Preparing the runtime directory"
mkdir -p "$RUNTIME_DIR"
chmod 700 "$RUNTIME_DIR"

# Stop an older instance before replacing its configuration.
if [ -x "$RUNTIME_DIR/stop.sh" ]; then
	"$RUNTIME_DIR/stop.sh" >/dev/null 2>&1 || true
fi

if [ -s "$RUNTIME_DIR/client.key" ]; then
	CLIENT_KEY="$(tr -d '\r\n' <"$RUNTIME_DIR/client.key")"
else
	if command -v openssl >/dev/null 2>&1; then
		CLIENT_KEY="$(openssl rand -hex 32)"
	else
		CLIENT_KEY="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
	fi
fi
case "$CLIENT_KEY" in
	*[!0-9a-fA-F]* | '') die "could not generate a valid client key" ;;
esac
[ "${#CLIENT_KEY}" -eq 64 ] || die "client.key must be 64 hex characters"

# Detect local upstreams with the same helper used for the OpenCode config.
detected=""
unset DETECTED_OPENCODE2API_CONFIG DETECTED_OPENCODE2API_KEY DETECTED_OPENCODE2API_URL \
	DETECTED_OPENCODE_ZEN_AUTH_FILE DETECTED_OPENCODE_ZEN_KEY 2>/dev/null || true
detected="$("$HELPER_BIN" detect -home "$HOME")" || die "upstream detection failed"
if [ -n "$detected" ]; then
	eval "$detected"
fi
GW_URL="${OPENCODE2API_URL:-${DETECTED_OPENCODE2API_URL:-}}"
GW_KEY="${OPENCODE2API_KEY:-${DETECTED_OPENCODE2API_KEY:-}}"
GW_CONFIG="${DETECTED_OPENCODE2API_CONFIG:-}"
ZEN_KEY="${OPENCODE_ZEN_KEY:-${DETECTED_OPENCODE_ZEN_KEY:-}}"
ZEN_AUTH="${DETECTED_OPENCODE_ZEN_AUTH_FILE:-}"

UP_NAME=()
UP_URL=()
UP_KEYREF=()
UP_PRIO=()
use_gateway=0
use_zen=0
use_custom=0

add_upstream() {
	UP_NAME+=("$1")
	UP_URL+=("$2")
	UP_KEYREF+=("$3")
	UP_PRIO+=("$4")
}

url_is_loopback() {
	case "$1" in
	http://127.* | http://localhost* | http://\[::1\]* | https://127.* | https://localhost* | https://\[::1\]*) return 0 ;;
	*) return 1 ;;
	esac
}

custom_url="${ROTATOR_UPSTREAM_URL:-}"
custom_key="${ROTATOR_UPSTREAM_KEY:-}"

if [ -n "$custom_url" ]; then
	case "$custom_url" in
	http://* | https://*) ;;
	*) die "ROTATOR_UPSTREAM_URL must start with http:// or https://" ;;
	esac
	case "$custom_url" in
	*$'\n'* | *$'\r'*) die "ROTATOR_UPSTREAM_URL must not contain a newline" ;;
	esac
	if [ -z "$custom_key" ] && ! url_is_loopback "$custom_url"; then
		if [ -r /dev/tty ] && [ -w /dev/tty ]; then
			printf 'API key for %s (leave empty to forward the client key): ' "$custom_url" >/dev/tty
			IFS= read -r -s custom_key </dev/tty || custom_key=""
			printf '\n' >/dev/tty
		else
			warn "custom upstream has no key; the client's Authorization header will be forwarded"
		fi
	fi
	add_upstream "custom" "$custom_url" '${ROTATOR_CUSTOM_KEY}' 0
	use_custom=1
	say "upstream: custom ($custom_url)"
	if is_true "${ROTATOR_ADD_DETECTED:-0}"; then
		if [ -n "$GW_URL" ] && [ -n "$GW_KEY" ]; then
			add_upstream "opencode2api-local" "$GW_URL" '${OPENCODE2API_KEY}' 10
			use_gateway=1
			say "upstream: opencode2api-local ($GW_URL)"
		fi
		if [ -n "$ZEN_KEY" ]; then
			add_upstream "opencode-zen-direct" "https://opencode.ai/zen/v1" '${OPENCODE_ZEN_KEY}' 20
			use_zen=1
			say "upstream: opencode-zen-direct (https://opencode.ai/zen/v1)"
		fi
	fi
else
	zen_priority=0
	if [ -n "$GW_URL" ] && [ -n "$GW_KEY" ]; then
		add_upstream "opencode2api-local" "$GW_URL" '${OPENCODE2API_KEY}' 0
		use_gateway=1
		say "upstream: opencode2api-local ($GW_URL)"
		if [ -n "$GW_CONFIG" ]; then say "detected: $GW_CONFIG"; fi
		zen_priority=10
	elif [ -n "$GW_URL" ]; then
		warn "found an opencode2api URL but no server key; skipping it"
	fi
	if [ -n "$ZEN_KEY" ]; then
		add_upstream "opencode-zen-direct" "https://opencode.ai/zen/v1" '${OPENCODE_ZEN_KEY}' "$zen_priority"
		use_zen=1
		say "upstream: opencode-zen-direct (https://opencode.ai/zen/v1)"
		if [ -n "$ZEN_AUTH" ]; then say "detected: $ZEN_AUTH"; fi
	fi
	if [ "${#UP_URL[@]}" -eq 0 ]; then
		if [ -r /dev/tty ] && [ -w /dev/tty ]; then
			printf 'No local upstream detected. Upstream base URL [http://127.0.0.1:8080/v1]: ' >/dev/tty
			IFS= read -r custom_url </dev/tty || custom_url=""
			[ -n "$custom_url" ] || custom_url="http://127.0.0.1:8080/v1"
			printf 'API key (leave empty for a local upstream): ' >/dev/tty
			IFS= read -r -s custom_key </dev/tty || custom_key=""
			printf '\n' >/dev/tty
			case "$custom_url" in
			http://* | https://*) ;;
			*) die "upstream URL must start with http:// or https://" ;;
			esac
			add_upstream "custom" "$custom_url" '${ROTATOR_CUSTOM_KEY}' 0
			use_custom=1
			say "upstream: custom ($custom_url)"
		else
			die "no upstream detected and no terminal for a prompt.
Set ROTATOR_UPSTREAM_URL and ROTATOR_UPSTREAM_KEY and run the installer again:
  curl -fsSL https://raw.githubusercontent.com/hiruko-puppetmaster/termux-proxy-rotator/main/install-opencode.sh | ROTATOR_UPSTREAM_URL='https://api.example.com/v1' ROTATOR_UPSTREAM_KEY='sk-...' bash"
		fi
	fi
fi

# Shell-quote a value for env.sh, which is sourced by bash.
shell_quote() { printf '%q' "$1"; }

umask 077
{
	printf '# Generated by install-opencode.sh. Mode 600; contains credentials.\n'
	printf 'export ROTATOR_CLIENT_KEY=%s\n' "$(shell_quote "$CLIENT_KEY")"
	if [ "$use_gateway" = "1" ]; then
		printf 'export OPENCODE2API_KEY=%s\n' "$(shell_quote "$GW_KEY")"
	fi
	if [ "$use_zen" = "1" ]; then
		printf 'export OPENCODE_ZEN_KEY=%s\n' "$(shell_quote "$ZEN_KEY")"
	fi
	if [ "$use_custom" = "1" ]; then
		printf 'export ROTATOR_CUSTOM_KEY=%s\n' "$(shell_quote "$custom_key")"
	fi
} >"$RUNTIME_DIR/env.sh"
chmod 600 "$RUNTIME_DIR/env.sh"
printf '%s\n' "$CLIENT_KEY" >"$RUNTIME_DIR/client.key"
chmod 600 "$RUNTIME_DIR/client.key"

json_escape() {
	printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

{
	cat <<EOF
{
  "listen": "127.0.0.1:${PORT}",
  "admin_listen": "127.0.0.1:${ADMIN_PORT}",
  "max_attempts": 4,
  "max_request_bytes": 33554432,
  "upstream_header_timeout_seconds": 300,
  "max_cooldown_seconds": 600,
  "client_api_key": "\${ROTATOR_CLIENT_KEY}",
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
EOF
	first=1
	i=0
	while [ "$i" -lt "${#UP_URL[@]}" ]; do
		if [ "$first" -eq 1 ]; then
			first=0
		else
			printf ',\n'
		fi
		printf '    {\n'
		printf '      "name": "%s",\n' "$(json_escape "${UP_NAME[$i]}")"
		printf '      "base_url": "%s",\n' "$(json_escape "${UP_URL[$i]}")"
		if [ -n "${UP_KEYREF[$i]}" ]; then
			printf '      "api_key": "%s",\n' "${UP_KEYREF[$i]}"
		fi
		printf '      "priority": %s\n' "${UP_PRIO[$i]}"
		printf '    }'
		i=$((i + 1))
	done
	printf '\n  ]\n}\n'
} >"$RUNTIME_DIR/config.json"
chmod 600 "$RUNTIME_DIR/config.json"
umask 022

start_script="$RUNTIME_DIR/start.sh"
stop_script="$RUNTIME_DIR/stop.sh"

{
	printf '#!/usr/bin/env bash\n'
	printf 'set -euo pipefail\n\n'
	printf 'DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"\n'
	printf 'BIN=%s\n' "$(shell_quote "$PROXY_BIN")"
	printf 'PIDFILE="$DIR/rotator.pid"\n'
	printf 'LOG="$DIR/rotator.log"\n'
	printf 'PORT=%s\n' "$(shell_quote "$PORT")"
	printf 'ADMIN_PORT=%s\n\n' "$(shell_quote "$ADMIN_PORT")"
	cat <<'EOF'
if [ -f "$PIDFILE" ]; then
	pid="$(cat "$PIDFILE" 2>/dev/null || true)"
	if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
		echo "proxy-rotator already running (pid $pid)"
		exit 0
	fi
	rm -f "$PIDFILE"
fi

if [ ! -x "$BIN" ]; then
	echo "proxy-rotator binary not found: $BIN" >&2
	exit 1
fi

. "$DIR/env.sh"
command -v termux-wake-lock >/dev/null 2>&1 && termux-wake-lock || true

nohup "$BIN" -config "$DIR/config.json" >>"$LOG" 2>&1 &
echo $! >"$PIDFILE"

for _ in 1 2 3 4 5 6 7 8 9 10; do
	if ! kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
		echo "proxy-rotator failed to start; last log lines:" >&2
		tail -n 20 "$LOG" >&2 || true
		rm -f "$PIDFILE"
		exit 1
	fi
	if command -v curl >/dev/null 2>&1 &&
		curl -fsS "http://127.0.0.1:$ADMIN_PORT/healthz" 2>/dev/null |
		grep -q '"service": *"proxy-rotator"'; then
		echo "proxy-rotator started (pid $(cat "$PIDFILE")) on http://127.0.0.1:$PORT/v1"
		exit 0
	fi
	sleep 0.5
done

echo "proxy-rotator started (pid $(cat "$PIDFILE")), but the admin endpoint did not answer yet" >&2
tail -n 10 "$LOG" >&2 || true
EOF
} >"$start_script"

{
	printf '#!/usr/bin/env bash\n'
	printf 'set -euo pipefail\n\n'
	printf 'DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"\n'
	printf 'PIDFILE="$DIR/rotator.pid"\n\n'
	cat <<'EOF'
if [ -f "$PIDFILE" ]; then
	pid="$(cat "$PIDFILE" 2>/dev/null || true)"
	if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
		kill "$pid"
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$pid" 2>/dev/null || break
			sleep 0.2
		done
		rm -f "$PIDFILE"
		echo "proxy-rotator stopped"
		exit 0
	fi
fi
rm -f "$PIDFILE"
echo "proxy-rotator is not running"
EOF
} >"$stop_script"

chmod 700 "$start_script" "$stop_script"
touch "$RUNTIME_DIR/rotator.log"
chmod 600 "$RUNTIME_DIR/rotator.log"
say "runtime: $RUNTIME_DIR"

# Validate the generated config with the same binary that will run it.
check_output=""
if ! check_output="$(
	set -a
	# shellcheck disable=SC1091
	. "$RUNTIME_DIR/env.sh"
	set +a
	"$PROXY_BIN" -check -config "$RUNTIME_DIR/config.json" 2>&1
)"; then
	printf '%s\n' "$check_output" >&2
	die "generated config did not validate"
fi
say "config: valid pool with ${#UP_URL[@]} upstream(s)"

# --------------------------------------------------------------- service ---

step "4/6" "Starting the background service"
if [ "$SKIP_START" = "1" ]; then
	warn "--no-start was requested; the proxy was not started"
else
	"$start_script"
	ready=0
	if command -v curl >/dev/null 2>&1; then
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			if curl -fsS "http://127.0.0.1:$ADMIN_PORT/healthz" 2>/dev/null |
				grep -q '"service": *"proxy-rotator"'; then
				ready=1
				break
			fi
			sleep 0.5
		done
		if [ "$ready" != "1" ]; then
			die "the proxy did not answer on 127.0.0.1:$ADMIN_PORT; check $RUNTIME_DIR/rotator.log"
		fi
	fi
	say "status: http://127.0.0.1:$ADMIN_PORT/status"
fi

# Optional Termux:Boot hook.
if [ -d "$HOME/.termux/boot" ]; then
	boot_hook="$HOME/.termux/boot/90-proxy-rotator"
	{
		printf '#!/usr/bin/env bash\n'
		printf '%s\n' "$(shell_quote "$start_script")"
	} >"$boot_hook"
	chmod 700 "$boot_hook"
	say "boot hook: $boot_hook"
fi

# --------------------------------------------------------------- OpenCode ---

step "5/6" "Registering the provider in OpenCode"
if [ -z "$OPENCODE_CONF" ]; then
	if [ -f "$CONFIG_HOME/opencode/opencode.jsonc" ]; then
		OPENCODE_CONF="$CONFIG_HOME/opencode/opencode.jsonc"
	elif [ -f "$CONFIG_HOME/opencode/opencode.json" ]; then
		OPENCODE_CONF="$CONFIG_HOME/opencode/opencode.json"
	else
		OPENCODE_CONF="$CONFIG_HOME/opencode/opencode.jsonc"
	fi
fi
if [ -d "$OPENCODE_CONF" ]; then
	OPENCODE_CONF="$OPENCODE_CONF/opencode.jsonc"
fi

apply_args=(
	apply
	-config "$OPENCODE_CONF"
	-provider-id rotator
	-name "Termux Proxy Rotator"
	-base-url "http://127.0.0.1:$PORT/v1"
	-api-key "{file:$RUNTIME_DIR/client.key}"
	-key-file "$RUNTIME_DIR/client.key"
	-models-url "http://127.0.0.1:$PORT/v1/models"
	-quiet
)
if [ -n "$MODELS_FILE" ]; then apply_args+=(-models-file "$MODELS_FILE"); fi
if [ "$KEEP_DEFAULT" = "1" ]; then apply_args+=(-keep-default); fi
if [ -n "$EXPLICIT_DEFAULT" ]; then
	apply_args+=(-default-model "$EXPLICIT_DEFAULT")
elif [ -n "$PREFER_MODEL" ]; then
	apply_args+=(-prefer-model "$PREFER_MODEL")
fi

if ! "$HELPER_BIN" "${apply_args[@]}"; then
	die "could not update $OPENCODE_CONF"
fi
say "OpenCode config: $OPENCODE_CONF"
if [ -f "$OPENCODE_CONF.bak" ]; then
	say "backup: $OPENCODE_CONF.bak"
fi

# --------------------------------------------------------------- verify ---

step "6/6" "Verifying"
if [ "$SKIP_VERIFY" = "1" ]; then
	say "verification skipped"
elif command -v opencode >/dev/null 2>&1; then
	models_output=""
	if command -v timeout >/dev/null 2>&1; then
		models_output="$(timeout 120 opencode models 2>/dev/null || true)"
	else
		models_output="$(opencode models 2>/dev/null || true)"
	fi
	if printf '%s\n' "$models_output" | grep -q '^rotator/'; then
		count="$(printf '%s\n' "$models_output" | grep -c '^rotator/')"
		say "OpenCode sees $count rotator model(s)"
	else
		warn "OpenCode did not list rotator models yet"
		warn "restart OpenCode and run: opencode models | grep '^rotator/'"
	fi
else
	warn "the opencode command was not found; the config is ready for when it is installed"
fi

# --------------------------------------------------------------- summary ---

default_line=""
if [ "$KEEP_DEFAULT" = "0" ]; then
	default_line="Default model: rotator/${EXPLICIT_DEFAULT:-$PREFER_MODEL}"
	[ -n "$EXPLICIT_DEFAULT" ] || default_line="$default_line (or the first available model)"
fi

cat <<EOF

Installation complete.

  Provider:      rotator
  Base URL:      http://127.0.0.1:$PORT/v1
  OpenCode:      $OPENCODE_CONF
  Runtime:       $RUNTIME_DIR
  Binary:        $PROXY_BIN
EOF
if [ -n "$default_line" ]; then printf '  %s\n' "$default_line"; fi
cat <<EOF

Controls:
  $start_script
  $stop_script
  curl -s http://127.0.0.1:$ADMIN_PORT/status
  tail -f $RUNTIME_DIR/rotator.log

Start the proxy after a device reboot (or install Termux:Boot and create
~/.termux/boot/90-proxy-rotator if the hook was not added automatically).
EOF
