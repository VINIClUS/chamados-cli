#!/usr/bin/env bash
# Built on a normal CI runner, never on the 512 MiB protected runner.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"
revision="${OPS_EXECUTOR_SHA:-${CI_COMMIT_SHA:-$(git rev-parse HEAD)}}"
[[ "$revision" =~ ^[a-f0-9]{40}$ ]] || { echo 'Full executor SHA required' >&2; exit 2; }
build_date="${CI_COMMIT_TIMESTAMP:-$(git show -s --format=%cI HEAD)}"
mkdir -p .ops-executor
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
    -ldflags "-s -w -X main.version=executor-$revision -X main.commit=$revision -X main.date=$build_date" \
    -o .ops-executor/chamadosctl ./cmd/chamadosctl
.ops-executor/chamadosctl internal inspect --binary >.ops-executor/manifest.json
(cd .ops-executor && sha256sum chamadosctl >SHA256SUMS)
