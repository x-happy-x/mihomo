#!/bin/sh
set -eu

CONFIG_FILE=${CONFIG_FILE:-/opt/etc/mihomo/config.yaml}
CONFIG_DIR=${CONFIG_DIR:-/opt/etc/mihomo}
MIHOMO=${MIHOMO:-/opt/sbin/mihomo}
HELPER_TARGET=${HELPER_TARGET:-/opt/bin/mihomo-tail}
BACKUP_DIR=${BACKUP_DIR:-/opt/etc/mihomo/backup/config}
HEALTH_URL=${HEALTH_URL:-http://127.0.0.1:9090/version}
TAIL_LISTENER=${TAIL_LISTENER:-tail-console}
APPLY=${APPLY:-0}
ACTIVATE=${ACTIVATE:-0}
TAIL_PROXY=${1:-}
TAIL_PORT=${2:-11080}
SERVICE=/opt/etc/init.d/S05xkeen
LOCK_DIR=/tmp/mihomo-tail-console-install.lock
SCRIPT_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd)
CANDIDATE=

usage() {
    cat <<'EOF'
Usage: install-tail-console.sh <Tailscale proxy name> [local port]

Environment:
  APPLY=1       write the validated config and install mihomo-tail
  ACTIVATE=1    restart XKeen, check the API, and roll back on failure
  CONFIG_FILE=  active Mihomo config (default: /opt/etc/mihomo/config.yaml)

The default run only validates a candidate config and changes nothing.
EOF
}

cleanup() {
    [ -n "$CANDIDATE" ] && rm -f "$CANDIDATE"
    rmdir "$LOCK_DIR" 2>/dev/null || true
}
trap cleanup EXIT INT TERM HUP

[ -n "$TAIL_PROXY" ] || {
    usage >&2
    exit 2
}

case "$TAIL_PORT" in
    ''|*[!0-9]*)
        echo "The listener port must be a number." >&2
        exit 2
        ;;
esac
[ "$TAIL_PORT" -ge 1 ] && [ "$TAIL_PORT" -le 65535 ] || {
    echo "The listener port must be between 1 and 65535." >&2
    exit 2
}

case "$APPLY:$ACTIVATE" in
    0:0|1:0|1:1) ;;
    0:1)
        echo "ACTIVATE=1 requires APPLY=1." >&2
        exit 2
        ;;
    *)
        echo "APPLY and ACTIVATE must be 0 or 1." >&2
        exit 2
        ;;
esac

[ "$(id -u)" = "0" ] || {
    echo "Run this installer as root." >&2
    exit 1
}
[ -x "$MIHOMO" ] || {
    echo "Mihomo executable not found: $MIHOMO" >&2
    exit 1
}
[ -f "$CONFIG_FILE" ] || {
    echo "Mihomo config not found: $CONFIG_FILE" >&2
    exit 1
}
[ -f "$SCRIPT_DIR/mihomo-tail" ] || {
    echo "mihomo-tail must be next to this installer." >&2
    exit 1
}

YQ=${YQ:-}
if [ -z "$YQ" ]; then
    YQ=$(command -v yq 2>/dev/null || true)
fi
[ -n "$YQ" ] && [ -x "$YQ" ] || {
    echo "yq is required to update YAML safely." >&2
    exit 1
}

if [ "$ACTIVATE" = "1" ] && [ ! -x "$SERVICE" ]; then
    echo "XKeen service not found at $SERVICE; refusing activation." >&2
    exit 1
fi

mkdir "$LOCK_DIR" 2>/dev/null || {
    echo "Another tail-console installation is already running." >&2
    exit 1
}

config_target=$(readlink -f "$CONFIG_FILE" 2>/dev/null || true)
[ -n "$config_target" ] || config_target=$CONFIG_FILE
CANDIDATE="${config_target}.tail-console.$$"

if ! TAIL_PROXY="$TAIL_PROXY" "$YQ" -e '
  [.proxies[]?.name, ."proxy-groups"[]?.name] |
  any_c(. == strenv(TAIL_PROXY))
' "$config_target" >/dev/null 2>&1; then
    echo "Proxy or proxy group not found in the active config: $TAIL_PROXY" >&2
    exit 1
fi

occupied_ports=$(TAIL_LISTENER="$TAIL_LISTENER" "$YQ" -r '
  [
    .port,
    ."socks-port",
    ."redir-port",
    ."tproxy-port",
    ."mixed-port",
    (.listeners[]? |
      select((.name // "") != strenv(TAIL_LISTENER)) |
      .port)
  ] |
  .[] |
  select(. != null and . != 0)
' "$config_target")

if printf '%s\n' "$occupied_ports" | awk -v wanted="$TAIL_PORT" '
  {
    count = split($0, parts, ",")
    for (i = 1; i <= count; i++) {
      gsub(/[[:space:]]/, "", parts[i])
      range_count = split(parts[i], bounds, "-")
      if ((range_count == 1 && bounds[1] == wanted) ||
          (range_count == 2 && wanted >= bounds[1] && wanted <= bounds[2])) {
        found = 1
      }
    }
  }
  END { exit found ? 0 : 1 }
'; then
    echo "Port $TAIL_PORT is already used by another Mihomo inbound." >&2
    exit 1
fi

TAIL_LISTENER="$TAIL_LISTENER" TAIL_PROXY="$TAIL_PROXY" TAIL_PORT="$TAIL_PORT" \
    "$YQ" '
      .listeners = (
        (.listeners // [] | map(select((.name // "") != strenv(TAIL_LISTENER)))) +
        [{
          "name": strenv(TAIL_LISTENER),
          "type": "socks",
          "listen": "127.0.0.1",
          "port": env(TAIL_PORT),
          "proxy": strenv(TAIL_PROXY),
          "udp": false,
          "users": []
        }]
      )
    ' "$config_target" > "$CANDIDATE"
chmod 0600 "$CANDIDATE"

"$MIHOMO" -t -d "$CONFIG_DIR" -f "$CANDIDATE"

if [ "$APPLY" != "1" ]; then
    echo "Candidate config is valid; no files were changed."
    echo "Listener: 127.0.0.1:$TAIL_PORT -> $TAIL_PROXY"
    echo "Run again with APPLY=1 to install it."
    exit 0
fi

timestamp=$(date '+%Y%m%d_%H%M%S')
mkdir -p "$BACKUP_DIR" "$(dirname "$HELPER_TARGET")"
backup="$BACKUP_DIR/config.yaml.$timestamp.$$"
cp -p "$config_target" "$backup"

helper_new="${HELPER_TARGET}.new.$$"
cp "$SCRIPT_DIR/mihomo-tail" "$helper_new"
chmod 0755 "$helper_new"
mv "$helper_new" "$HELPER_TARGET"
mv "$CANDIDATE" "$config_target"
CANDIDATE=

if [ "$ACTIVATE" != "1" ]; then
    echo "Tail console installed but XKeen was not restarted."
    echo "Listener after the next restart: 127.0.0.1:$TAIL_PORT -> $TAIL_PROXY"
    echo "Config backup: $backup"
    exit 0
fi

"$SERVICE" restart || true

healthy=0
attempt=0
while [ "$attempt" -lt 60 ]; do
    if pidof mihomo >/dev/null 2>&1; then
        if command -v curl >/dev/null 2>&1; then
            curl -sS --max-time 3 -o /dev/null "$HEALTH_URL" >/dev/null 2>&1 && healthy=1
        else
            wget -qO- "$HEALTH_URL" >/dev/null 2>&1 && healthy=1
        fi
    fi
    [ "$healthy" = "1" ] && break
    attempt=$((attempt + 1))
    sleep 2
done

if [ "$healthy" != "1" ]; then
    echo "Mihomo failed its health check; restoring the previous config." >&2
    cp -p "$backup" "$config_target"
    "$SERVICE" restart || true
    echo "Previous config restored from: $backup" >&2
    exit 1
fi

echo "Tail console is active: 127.0.0.1:$TAIL_PORT -> $TAIL_PROXY"
echo "Config backup: $backup"
