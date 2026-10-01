#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
artifact=${1:-}
router=${ROUTER:-root@192.168.1.1}
port=${ROUTER_PORT:-222}
activate=${ACTIVATE:-0}

if [[ -z "$artifact" || ! -f "$artifact" ]]; then
  echo "Usage: deploy.sh <mihomo.gz>" >&2
  exit 2
fi

if [[ "$activate" != "0" && "$activate" != "1" ]]; then
  echo "ACTIVATE must be 0 or 1." >&2
  exit 2
fi

checksum_file="$artifact.sha256"
[[ -f "$checksum_file" ]] || {
  echo "Checksum file not found: $checksum_file" >&2
  exit 1
}

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
cp "$script_dir/install.sh" "$stage/install.sh"
cp "$artifact" "$stage/mihomo.gz"
cp "$checksum_file" "$stage/mihomo.gz.sha256"

echo "Uploading to $router:$port. The router may prompt for its SSH password."
tar -C "$stage" -cf - install.sh mihomo.gz mihomo.gz.sha256 | \
  ssh -p "$port" "$router" \
    "set -e; d=\$(mktemp -d /tmp/mihomo-deploy.XXXXXX); trap 'rm -rf \"\$d\"' EXIT; tar -xf - -C \"\$d\"; ACTIVATE=$activate sh \"\$d/install.sh\" \"\$d/mihomo.gz\""
