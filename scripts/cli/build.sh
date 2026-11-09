#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"
tag="${CI_COMMIT_TAG:-}"
version="${VERSION:-${tag#cli-v}}"
version="${version:-dev}"
[[ "$version" =~ ^([0-9]+\.[0-9]+\.[0-9]+|dev)$ ]] || { echo 'Versão inválida' >&2; exit 2; }
commit="${CI_COMMIT_SHA:-$(git rev-parse HEAD)}"
build_date="${BUILD_DATE:-${CI_COMMIT_TIMESTAMP:-$(git show -s --format=%cI HEAD)}}"
out="${1:-$root/.cli-dist}"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
files=()
for target_os in linux darwin windows; do
    for target_arch in amd64 arm64; do
        name="chamadosctl_${version}_${target_os}_${target_arch}"
        [[ "$target_os" != windows ]] || name+=.exe
        CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build \
            -trimpath -buildvcs=false \
            -ldflags "-s -w -X main.version=$version -X main.commit=$commit -X main.date=$build_date" \
            -o "$out/$name" ./cmd/chamadosctl
        files+=("$name")
    done
done
(cd "$out" && sha256sum "${files[@]}" > SHA256SUMS)
printf 'Binários e checksums em %s\n' "$out"
