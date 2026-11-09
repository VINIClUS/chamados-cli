#!/usr/bin/env bash
# CI-facing backup dispatcher. No operator-side dependencies beyond SSH/Bash;
# Python runs on the trusted CI runner/CT, and the job token travels via stdin.
set +x
set -Eeuo pipefail
umask 077
action="${1:-}" environment="${2:-}" backup_id="${3:-}"
operation=backup
[[ "$action" != restore ]] || operation=restore
result="${OPS_RESULT_FILE:-operation-result.json}"
temp_result="$result.tmp.$$"
incoming=""
completed=false
ssh_opts=(-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes)
# shellcheck disable=SC2329,SC2317 # Invoked by the EXIT trap (ShellCheck 0.10).
finish() {
    local rc=$?
    trap - EXIT
    set +e
    if [[ "$completed" != true ]]; then
        python3 - "$operation" "$environment" >"$result" <<'PY'
import json,sys
print(json.dumps(dict(schema_version=1, operation=sys.argv[1], environment=sys.argv[2], status='failed', revision='')))
PY
    fi
    rm -f -- "$temp_result"
    if [[ -n "$incoming" ]]; then
        ssh "${ssh_opts[@]}" -- "$DEPLOY_SSH" "rm -rf -- '$incoming'" </dev/null >/dev/null 2>&1
    fi
    exit "$rc"
}
trap finish EXIT
die() { echo "backup: $*" >&2; exit 2; }
case "$action" in create|restore|cleanup) ;; *) die 'expected create|restore|cleanup';; esac
case "$environment" in
    staging)
        DEPLOY_SSH="${DEPLOY_SSH:-root@10.10.10.180}"
        DEPLOY_SSH_KEY="${DEPLOY_SSH_KEY:-$HOME/.ssh/chamados-stg_ed25519}"
        DEPLOY_URL="${DEPLOY_URL:-https://stg-chamados.portosoftware.com.br}"
        ;;
    production)
        DEPLOY_SSH="${DEPLOY_SSH:-root@10.10.10.181}"
        DEPLOY_SSH_KEY="${DEPLOY_SSH_KEY:-$HOME/.ssh/chamados-prod_ed25519}"
        DEPLOY_URL="${DEPLOY_URL:-https://chamados.portosoftware.com.br}"
        ;;
    *) die 'invalid environment';;
esac
[[ "$action" != restore || "${OPS_CONFIRM:-}" == "$environment" ]] || die 'restore requires confirm equal to environment'
[[ "$action" != restore || "$backup_id" =~ ^[0-9]{14}\.[0-9]+$ ]] || die 'invalid backup_id'
[[ "$DEPLOY_SSH" =~ ^[a-zA-Z0-9_.@:-]+$ ]] || die 'invalid SSH target'
[[ -n "${CI_JOB_TOKEN:-}" && "${CI_PIPELINE_IID:-}" =~ ^[0-9]+$ ]] || die 'CI_JOB_TOKEN and numeric CI_PIPELINE_IID required'
for bin in ssh tar python3; do command -v "$bin" >/dev/null || die "missing runner dependency: $bin"; done
if [[ -f "$DEPLOY_SSH_KEY" ]]; then ssh_opts+=(-i "$DEPLOY_SSH_KEY" -o IdentitiesOnly=yes); fi
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
incoming="/opt/chamados/incoming/backup-executor-${CI_PIPELINE_IID}-$$"
tar -C "$script_dir" -cf - remote-backup.sh backup_archive.py |
    ssh "${ssh_opts[@]}" -- "$DEPLOY_SSH" "umask 077; mkdir -p '$incoming'; tar -xf - -C '$incoming'"
# Input settings never occur in SSH argv, logs, or a persistent remote file.
export CI_API_V4_URL="${CI_API_V4_URL:-https://gitlab.com/api/v4}" CI_PROJECT_ID="${CI_PROJECT_ID:-74155066}"
export CI_PROJECT_PATH="${CI_PROJECT_PATH:-portosoftware/SistemaDeChamados}"
export OPS_REF="${OPS_REF:-main}" OPS_CONFIRM="${OPS_CONFIRM:-}"
rc=0
python3 -c 'import json,os; print(json.dumps({k:os.environ.get(k, "") for k in ("CI_JOB_TOKEN","CI_API_V4_URL","CI_PROJECT_ID","CI_PIPELINE_IID","OPS_REF","OPS_CONFIRM","CI_PROJECT_PATH")}))' |
    ssh "${ssh_opts[@]}" -- "$DEPLOY_SSH" "bash '$incoming/remote-backup.sh' '$action' '$environment' '$backup_id'" >"$temp_result" || rc=$?
if python3 - "$temp_result" "$operation" "$environment" <<'PY'
import json,sys
value=json.load(open(sys.argv[1]))
assert value['schema_version']==1 and value['operation']==sys.argv[2] and value['environment']==sys.argv[3]
assert value['status'] in ('success','failed')
PY
then
    mv -- "$temp_result" "$result"
    completed=true
    cat "$result"
else
    ((rc != 0)) || rc=1
fi
exit "$rc"
