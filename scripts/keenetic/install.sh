#!/bin/sh
set -eu

TARGET=${TARGET:-/opt/sbin/mihomo}
CONFIG_FILE=${CONFIG_FILE:-/opt/etc/mihomo/config.yaml}
CONFIG_DIR=${CONFIG_DIR:-/opt/etc/mihomo}
BACKUP_DIR=${BACKUP_DIR:-/opt/etc/mihomo/backup/bin}
ACTIVATE=${ACTIVATE:-0}
ALLOW_UNVERIFIED=${ALLOW_UNVERIFIED:-0}
HEALTH_URL=${HEALTH_URL:-http://127.0.0.1:9090/version}
SOURCE=${1:-}
EXPECTED_SHA256=${2:-}
SERVICE=/opt/etc/init.d/S05xkeen
LOCK_DIR=/tmp/mihomo-keenetic-install.lock
WORK_DIR=

usage() {
    cat <<'EOF'
Usage: install.sh <mihomo.gz path-or-url> [sha256]

Environment:
  ACTIVATE=1          restart XKeen and verify the Mihomo API
  ALLOW_UNVERIFIED=1  permit an artifact without a .sha256 file
  TARGET=...          binary path (default: /opt/sbin/mihomo)
  CONFIG_FILE=...     active Mihomo config
EOF
}

cleanup() {
    [ -n "$WORK_DIR" ] && rm -rf "$WORK_DIR"
    rmdir "$LOCK_DIR" 2>/dev/null || true
}
trap cleanup EXIT INT TERM HUP

[ -n "$SOURCE" ] || {
    usage >&2
    exit 2
}

[ "$(id -u)" = "0" ] || {
    echo "Run this installer as root." >&2
    exit 1
}

case "$(uname -m)" in
    aarch64|arm64) ;;
    *)
        echo "This fork currently installs the Keenetic arm64 artifact only." >&2
        exit 1
        ;;
esac

case "$ACTIVATE" in
    0) ;;
    1)
        [ -x "$SERVICE" ] || {
            echo "XKeen service not found at $SERVICE; refusing activation before changing the binary." >&2
            exit 1
        }
        ;;
    *)
        echo "ACTIVATE must be 0 or 1." >&2
        exit 1
        ;;
esac

mkdir "$LOCK_DIR" 2>/dev/null || {
    echo "Another Mihomo installation is already running." >&2
    exit 1
}

WORK_DIR=$(mktemp -d /tmp/mihomo-install.XXXXXX)
archive="$WORK_DIR/mihomo.gz"
candidate="$WORK_DIR/mihomo"
checksum_file="$WORK_DIR/mihomo.gz.sha256"

fetch() {
    source=$1
    destination=$2
    case "$source" in
        http://*|https://*)
            if command -v curl >/dev/null 2>&1; then
                curl -fL --connect-timeout 20 --max-time 600 -o "$destination" "$source"
            else
                wget -O "$destination" "$source"
            fi
            ;;
        *)
            cp "$source" "$destination"
            ;;
    esac
}

fetch "$SOURCE" "$archive"
[ -s "$archive" ] || {
    echo "Downloaded artifact is empty." >&2
    exit 1
}

if [ -z "$EXPECTED_SHA256" ]; then
    checksum_source="${SOURCE}.sha256"
    if fetch "$checksum_source" "$checksum_file" 2>/dev/null; then
        EXPECTED_SHA256=$(awk 'NR == 1 {print $1}' "$checksum_file")
    fi
fi

if [ -n "$EXPECTED_SHA256" ]; then
    actual_sha256=$(sha256sum "$archive" | awk '{print $1}')
    [ "$actual_sha256" = "$EXPECTED_SHA256" ] || {
        echo "SHA-256 verification failed." >&2
        exit 1
    }
elif [ "$ALLOW_UNVERIFIED" != "1" ]; then
    echo "No SHA-256 checksum was supplied or found next to the artifact." >&2
    exit 1
fi

gzip -cd "$archive" > "$candidate"
chmod 0755 "$candidate"
"$candidate" -v >/dev/null 2>&1 || {
    echo "The artifact cannot run on this router." >&2
    exit 1
}

[ -f "$CONFIG_FILE" ] || {
    echo "Mihomo config not found: $CONFIG_FILE" >&2
    exit 1
}

"$candidate" -t -d "$CONFIG_DIR" -f "$CONFIG_FILE"

timestamp=$(date '+%Y%m%d_%H%M%S')
mkdir -p "$BACKUP_DIR" "$(dirname "$TARGET")"
backup="$BACKUP_DIR/mihomo.$timestamp"
had_target=0

if [ -f "$TARGET" ]; then
    had_target=1
    cp -p "$TARGET" "$backup"
fi

target_new="${TARGET}.new.$$"
cp "$candidate" "$target_new"
chmod 0755 "$target_new"
mv "$target_new" "$TARGET"

if [ "$ACTIVATE" != "1" ]; then
    echo "Mihomo installed at $TARGET. The running process was not restarted."
    [ -f "$backup" ] && echo "Backup: $backup"
    exit 0
fi

"$SERVICE" restart || true

healthy=0
attempt=0
while [ "$attempt" -lt 90 ]; do
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
    echo "New Mihomo failed its health check; restoring the previous binary." >&2
    if [ -f "$backup" ]; then
        cp -p "$backup" "$target_new"
        mv "$target_new" "$TARGET"
        "$SERVICE" restart || true
    elif [ "$had_target" = "0" ]; then
        rm -f "$TARGET"
    fi
    exit 1
fi

echo "Mihomo activated successfully."
[ -f "$backup" ] && echo "Backup: $backup"
