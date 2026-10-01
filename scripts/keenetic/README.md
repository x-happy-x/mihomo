# Keenetic build and install

The target used by this fork is Linux arm64 (`aarch64-3.10_kn` in Entware),
with Mihomo installed at `/opt/sbin/mihomo`.

## Build

```sh
./scripts/keenetic/build.sh
```

Artifacts are written to `dist/keenetic/` together with SHA-256 and build
metadata files. Set `KEENETIC_ARCH` to override the default `arm64` target.

## Deploy from a workstation

```sh
./scripts/keenetic/deploy.sh dist/keenetic/mihomo-linux-arm64-*.gz
```

The deploy script uses SSH port `222` and never stores a router password.
Override `ROUTER` and `ROUTER_PORT` as needed. By default the binary is
validated and replaced atomically, but the running process is left untouched.

To restart XKeen and verify the local Mihomo API after installation:

```sh
ACTIVATE=1 ./scripts/keenetic/deploy.sh dist/keenetic/mihomo-linux-arm64-*.gz
```

Activation keeps the previous binary under `/opt/etc/mihomo/backup/bin/` and
restores it automatically if the process or API does not become healthy.

## Console access through Tailscale

Mihomo can expose a loopback-only SOCKS listener that is forced through one
named Tailscale proxy or proxy group. It does not start another `tailscaled` or
use another tsnet state directory. `socks5h` keeps destination DNS resolution
inside Mihomo as well.

First validate the generated config without changing the router:

```sh
TAIL_PROXY='tailscale' ./scripts/keenetic/deploy-tail-console.sh
```

Use the exact proxy or group name from the active Mihomo config. Apply the
validated change without restarting XKeen. The installer rejects unknown names
before modifying any files:

```sh
TAIL_PROXY='tailscale' APPLY=1 ./scripts/keenetic/deploy-tail-console.sh
```

To apply it immediately, restart XKeen and roll back the config automatically
if Mihomo does not become healthy:

```sh
TAIL_PROXY='tailscale' APPLY=1 ACTIVATE=1 \
  ./scripts/keenetic/deploy-tail-console.sh
```

The installed helper defaults to `127.0.0.1:11080`:

```sh
mihomo-tail check https://internal.example
mihomo-tail curl https://internal.example
mihomo-tail git clone https://git.internal.example/repository.git
mihomo-tail run proxy-aware-command argument
eval "$(mihomo-tail env)"
```

Set `TAIL_PORT` during deployment and `MIHOMO_TAIL_PORT` when using the helper
to select another loopback port. The listener has no routing mark and does not
require a firewall bypass rule because its outbound is selected explicitly by
the Mihomo `proxy` listener option.
