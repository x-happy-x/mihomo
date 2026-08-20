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
