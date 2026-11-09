#!/usr/bin/env bash
set -euo pipefail
environment="${1:-}" operation="${2:-}" service="${3:-}" tail="${4:-200}" confirm="${5:-}"
# This file is sent by the protected main executor, never taken from the requested source ref.
# shellcheck source=scripts/operations/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
OPS_ENVIRONMENT="$environment" OPS_OPERATION="$operation" OPS_SERVICE="$service" OPS_TAIL="$tail" OPS_CONFIRM="$confirm"
ops_validate
case "$operation" in status | logs | health | smoke | restart) ;; *) ops_error 'operação remota inválida'; exit 2 ;; esac
deploy_dir="${DEPLOY_DIR:-/opt/chamados}"
exec 9>"$deploy_dir/.deploy.lock"
flock -n -E 75 9
project=chamados-stg
[[ "$environment" != production ]] || project=chamados-prod
compose_file="$deploy_dir/app/infrastructure/application/docker-compose.yml"
[[ -f "$compose_file" ]] || compose_file="$deploy_dir/app/docker-compose.yml"
compose=(docker compose -p "$project" --env-file "$deploy_dir/.env"
    -f "$compose_file" -f "$deploy_dir/tooling/deploy/docker-compose.$environment.yml")
selected=()
[[ -z "$service" ]] || selected=("$service")
if [[ "$operation" == restart && -z "$service" ]]; then
    selected=(chamados-backend chamados-frontend)
fi
output="$(mktemp)"
trap 'rm -f "$output"' EXIT
health() {
    curl --fail --silent --show-error --max-time 20 --retry 3 --retry-all-errors \
        -o /dev/null http://127.0.0.1:8181/api/auth/csrf
    curl --fail --silent --show-error --max-time 20 --retry 3 --retry-all-errors \
        -o /dev/null http://127.0.0.1/
}
case "$operation" in
    status) "${compose[@]}" ps --all --format json >"$output" ;;
    logs) "${compose[@]}" logs --no-color --tail "$tail" "${selected[@]}" >"$output" ;;
    health) health >"$output" ;;
    smoke) (cd "$deploy_dir"; bash "$(dirname "${BASH_SOURCE[0]}")/verify-call-endpoints.sh" http://localhost:8181/api) >"$output" ;;
    restart) "${compose[@]}" restart "${selected[@]}" >"$output"; health >>"$output" ;;
esac
python3 - "$deploy_dir/app/REVISION" "$output" <<'PY'
import json, pathlib, sys
revision_path = pathlib.Path(sys.argv[1])
revision = revision_path.read_text().strip() if revision_path.exists() else ""
print(json.dumps({"revision": revision, "data": {"output": pathlib.Path(sys.argv[2]).read_text(errors="replace")}}))
PY
