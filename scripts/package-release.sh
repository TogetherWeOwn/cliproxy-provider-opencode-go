#!/usr/bin/env bash
set -euo pipefail

# Build tooling only: never installs a plugin or changes a running service.
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
go_bin=${GO:-go}
version=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["plugins"][0]["version"])' "$root/registry.json")
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Invalid version' >&2; exit 1; }
output=${1:-"$root/dist"}
mkdir -p -- "$output"
output=$(cd -- "$output" && pwd)
cache=${XDG_CACHE_HOME:-"$HOME/.cache"}
mkdir -p -- "$cache"
scratch=$(mktemp -d "$cache/opencode-go-release.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT

# Never build release artifacts with the hostcompat test tag.
GOFLAGS= CGO_ENABLED=1 GOOS=linux GOARCH=amd64 "$go_bin" -C "$root" build -trimpath -buildmode=c-shared -o "$scratch/opencode-go.so" ./cmd/plugin
nm -D "$scratch/opencode-go.so" > "$scratch/symbols.txt"
grep -q ' T cliproxy_plugin_init$' "$scratch/symbols.txt"
(cd -- "$scratch" && sha256sum opencode-go.so) > "$scratch/checksums.txt"
archive="opencode-go_${version}_linux_amd64.zip"
zip -q -X -j "$scratch/$archive" "$scratch/opencode-go.so" "$scratch/checksums.txt" "$root/LICENSE"
# The store verifies the archive using this external release asset. The archive
# also contains the shared library's checksum for manual verification.
(cd -- "$scratch" && sha256sum "$archive") > "$scratch/archive-checksums.txt"
cp -- "$scratch/$archive" "$output/"
cp -- "$scratch/archive-checksums.txt" "$output/checksums.txt"
printf 'Created %s and checksums.txt\n' "$archive"
