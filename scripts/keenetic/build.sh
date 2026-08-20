#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
output_dir=${OUTPUT_DIR:-"$repo_root/dist/keenetic"}
target=${KEENETIC_ARCH:-arm64}

case "$target" in
  arm64|aarch64)
    goarch=arm64
    artifact_arch=arm64
    ;;
  armv7|armv7l)
    goarch=arm
    goarm=7
    artifact_arch=armv7
    ;;
  mipsle-softfloat)
    goarch=mipsle
    gomips=softfloat
    artifact_arch=mipsle-softfloat
    ;;
  mipsle-hardfloat)
    goarch=mipsle
    gomips=hardfloat
    artifact_arch=mipsle-hardfloat
    ;;
  *)
    printf 'Unsupported KEENETIC_ARCH: %s\n' "$target" >&2
    exit 2
    ;;
esac

for command in go git gzip sha256sum; do
  command -v "$command" >/dev/null 2>&1 || {
    printf 'Required command is missing: %s\n' "$command" >&2
    exit 1
  }
done

commit=$(git -C "$repo_root" rev-parse --short HEAD)
version=${MIHOMO_VERSION:-"alpha-$commit"}
build_time=${BUILD_TIME:-$(date -u '+%Y-%m-%dT%H:%M:%SZ')}
binary="$output_dir/mihomo-linux-$artifact_arch-$version"
archive="$binary.gz"

mkdir -p "$output_dir"
rm -f "$binary" "$archive" "$archive.sha256" "$archive.manifest"

printf 'Building Mihomo %s for linux/%s...\n' "$version" "$artifact_arch"
(
  cd "$repo_root"
  export CGO_ENABLED=0 GOOS=linux GOARCH="$goarch"
  if [[ -n "${goarm:-}" ]]; then
    export GOARM="$goarm"
  fi
  if [[ -n "${gomips:-}" ]]; then
    export GOMIPS="$gomips"
  fi

  go build \
      -tags with_gvisor \
      -trimpath \
      -ldflags "-X github.com/metacubex/mihomo/constant.Version=$version -X github.com/metacubex/mihomo/constant.BuildTime=$build_time -w -s -buildid=" \
      -o "$binary" .
)

chmod 0755 "$binary"
gzip -n -9 "$binary"
(
  cd "$output_dir"
  sha256sum "$(basename "$archive")" > "$(basename "$archive").sha256"
)

cat > "$archive.manifest" <<EOF
version=$version
commit=$commit
goos=linux
goarch=$goarch
artifact_arch=$artifact_arch
build_time=$build_time
EOF

printf 'Created:\n  %s\n  %s\n  %s\n' "$archive" "$archive.sha256" "$archive.manifest"
