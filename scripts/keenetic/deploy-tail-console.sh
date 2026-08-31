#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tail_proxy=${TAIL_PROXY:-}
tail_port=${TAIL_PORT:-11080}
router=${ROUTER:-root@192.168.1.1}
router_port=${ROUTER_PORT:-222}
apply=${APPLY:-0}
activate=${ACTIVATE:-0}

if [[ -z "$tail_proxy" ]]; then
  echo "Set TAIL_PROXY to the exact Mihomo Tailscale proxy or group name." >&2
  exit 2
fi
if [[ "$tail_proxy" == *$'\n'* || "$tail_proxy" == *$'\r'* ]]; then
  echo "TAIL_PROXY must not contain line breaks." >&2
  exit 2
fi
if [[ ! "$tail_port" =~ ^[0-9]+$ ]] || ((tail_port < 1 || tail_port > 65535)); then
  echo "TAIL_PORT must be between 1 and 65535." >&2
  exit 2
fi
if [[ "$apply:$activate" != "0:0" && "$apply:$activate" != "1:0" && "$apply:$activate" != "1:1" ]]; then
  echo "APPLY and ACTIVATE must be 0 or 1; activation requires APPLY=1." >&2
  exit 2
fi

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
cp "$script_dir/install-tail-console.sh" "$stage/install-tail-console.sh"
cp "$script_dir/mihomo-tail" "$stage/mihomo-tail"

proxy_q=${tail_proxy//\'/\'\\\'\'}
proxy_q="'$proxy_q'"

echo "Validating tail-console on $router:$router_port (APPLY=$apply ACTIVATE=$activate)."
tar -C "$stage" -cf - install-tail-console.sh mihomo-tail | \
  ssh -p "$router_port" "$router" \
    "set -e; d=\$(mktemp -d /tmp/mihomo-tail-deploy.XXXXXX); trap 'rm -rf \"\$d\"' EXIT; tar -xf - -C \"\$d\"; APPLY=$apply ACTIVATE=$activate sh \"\$d/install-tail-console.sh\" $proxy_q $tail_port"
