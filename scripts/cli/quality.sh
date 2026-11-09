#!/usr/bin/env bash
# Informational in CI; local check returns the actual tool status. Only fmt rewrites.
set -uo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root" || exit 2
mode="${1:-check}"
case "$mode" in check|fmt|install) ;; *) echo 'Use: quality.sh check|fmt|install' >&2; exit 2 ;; esac
version=2.15.0
tools="$root/.quality/tools/$version"
binary="$tools/golangci-lint"
mkdir -p .quality || exit 2
if [[ ! -x "$binary" ]]; then
    case "$(uname -s)" in Linux) target_os=linux ;; Darwin) target_os=darwin ;; *) echo 'Use the pinned golangci-lint release on this OS.' >&2; exit 2 ;; esac
    case "$(uname -m)" in x86_64) target_arch=amd64 ;; aarch64|arm64) target_arch=arm64 ;; *) exit 2 ;; esac
    archive="golangci-lint-${version}-${target_os}-${target_arch}.tar.gz"
    temporary="$(mktemp -d)" || exit 2
    trap 'rm -rf -- "$temporary"' EXIT
    base="https://github.com/golangci/golangci-lint/releases/download/v$version"
    curl --fail --silent --show-error --location --retry 3 --max-time 120 "$base/$archive" -o "$temporary/$archive" || exit 2
    curl --fail --silent --show-error --location --retry 3 --max-time 120 "$base/golangci-lint-${version}-checksums.txt" -o "$temporary/checksums.txt" || exit 2
    awk -v name="$archive" '$2 == name {print}' "$temporary/checksums.txt" >"$temporary/selected.sha256"
    [[ -s "$temporary/selected.sha256" ]] || exit 2
    (cd "$temporary" && sha256sum --check selected.sha256) || exit 2
    tar -xzf "$temporary/$archive" -C "$temporary" || exit 2
    mkdir -p "$tools" || exit 2
    install -m 755 "$temporary/golangci-lint-${version}-${target_os}-${target_arch}/golangci-lint" "$binary" || exit 2
fi
export GOMAXPROCS="${GOMAXPROCS:-1}" GOMEMLIMIT="${GOMEMLIMIT:-512MiB}" GOFLAGS="${GOFLAGS:--p=1}"
"$binary" version >.quality/tool-version.txt 2>&1 || exit 2
"$binary" config verify >.quality/config.log 2>&1 || { cat .quality/config.log; exit 2; }
[[ "$mode" != install ]] || exit 0
if [[ "$mode" == fmt ]]; then
    exec "$binary" fmt --config .golangci.yml
fi
format_status=0
"$binary" fmt --config .golangci.yml --diff >.quality/format.diff 2>.quality/format.log || format_status=$?
if [[ -s .quality/format.diff && "$format_status" == 0 ]]; then format_status=1; fi
lint_status=0
"$binary" run --config .golangci.yml --path-prefix . >.quality/lint.log 2>&1 || lint_status=$?
cat .quality/lint.log
printf 'format=%s\nlint=%s\n' "$format_status" "$lint_status" >.quality/status.txt
if ((lint_status != 0)); then exit "$lint_status"; fi
exit "$format_status"
