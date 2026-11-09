#!/usr/bin/env bash
set -euo pipefail
scripts_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/operations/common.sh
source "$scripts_dir/operations/common.sh"
ops_validate
ops_target
incoming="/opt/chamados/incoming/ops-${CI_PIPELINE_ID:-local}-${CI_JOB_ID:-$$}"
ops_remote mkdir -p "$incoming"
trap 'ops_remote rm -rf -- "$incoming" >&2 || true' EXIT
tar -C "$scripts_dir/operations" -cf - common.sh remote-basic.sh -C "$scripts_dir" verify-call-endpoints.sh |
    ops_remote tar -xf - -C "$incoming"
ops_remote bash "$incoming/remote-basic.sh" "$OPS_ENVIRONMENT" "$OPS_OPERATION" \
    "${OPS_SERVICE:-}" "${OPS_TAIL:-200}" "${OPS_CONFIRM:-}"
