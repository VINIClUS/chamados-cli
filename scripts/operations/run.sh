#!/usr/bin/env bash
set -euo pipefail
scripts_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/operations/common.sh
source "$scripts_dir/operations/common.sh"
export OPS_OPERATION="${OPS_OPERATION:-}" OPS_ENVIRONMENT="${OPS_ENVIRONMENT:-}"
revision=''
finish() {
    local rc=$?
    trap - EXIT
    if ((rc != 0)); then
        # A backup failure can contain useful recovery metadata; retain it but force failed status.
        if [[ -f operation-result.json ]]; then
            python3 - <<'PY'
import json
from pathlib import Path
p = Path('operation-result.json')
r = json.loads(p.read_text())
r['status'] = 'failed'
p.write_text(json.dumps(r) + '\n')
PY
        else
            python3 "$scripts_dir/operations/metadata.py" result failed "$revision"
        fi
    fi
    exit "$rc"
}
trap finish EXIT
# Avoid accepting stale result/data from an earlier shell-executor workspace.
rm -f operation-result.json .ops-data.json
[[ "${CI_COMMIT_BRANCH:-}" == main && "${CI_COMMIT_REF_PROTECTED:-}" == true ]] || {
    ops_error 'operações exigem pipeline em main protegida'; exit 2;
}
ops_validate
case "$OPS_OPERATION" in
    deploy)
        revision="$(python3 "$scripts_dir/operations/metadata.py" read revision)"
        ref="$(python3 "$scripts_dir/operations/metadata.py" read ref)"
        bash "$scripts_dir/deploy.sh" "$OPS_ENVIRONMENT" deploy --archive .ops-target/source.tar \
            --revision "$revision" --source-ref "$ref" --keep-data >&2
        python3 "$scripts_dir/operations/metadata.py" result success "$revision"
        python3 "$scripts_dir/operations/metadata.py" deployment "$ref"
        # A release marks code that actually reached production, not just creation of a tag.
        if [[ "$OPS_ENVIRONMENT" == production && "$ref" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
            CI_COMMIT_TAG="$ref" CI_COMMIT_SHA="$revision" bash "$scripts_dir/deploy/publish-release.sh" >&2
        fi
        ;;
    backup | restore)
        action=create
        [[ "$OPS_OPERATION" != restore ]] || action=restore
        bash "$scripts_dir/operations/backup.sh" "$action" "$OPS_ENVIRONMENT" "${OPS_BACKUP_ID:-}" >&2
        [[ "$OPS_OPERATION" != restore ]] || python3 "$scripts_dir/operations/metadata.py" deployment
        ;;
    reset)
        bash "$scripts_dir/deploy.sh" "$OPS_ENVIRONMENT" reset --confirm "${OPS_CONFIRM:-}" >&2
        # Get the installed revision only after reset completed; this does not create a deployment.
        OPS_OPERATION=status bash "$scripts_dir/operations/basic.sh" >.ops-data.json
        python3 "$scripts_dir/operations/metadata.py" result success ''
        ;;
    *)
        bash "$scripts_dir/operations/basic.sh" >.ops-data.json
        python3 "$scripts_dir/operations/metadata.py" result success ''
        ;;
esac
cat operation-result.json
