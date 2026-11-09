#!/usr/bin/env bash
# Trusted executor on the CT. One flock spans capture, registry commit and restore.
# Input: a single JSON object on stdin (credentials live only in process memory).
# shellcheck disable=SC2016 # sh -c snippets intentionally expand in the container.
set +x
set -Eeuo pipefail
umask 077
exec 3>&1 1>&2
action="${1:-}" environment="${2:-}" backup_id="${3:-}"
deploy_dir="${CHAMADOS_DEPLOY_DIR:-/opt/chamados}"
helper="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/backup_archive.py"
operation=backup
[[ "$action" != restore ]] || operation=restore
revision="" source_ref="" safety_backup_id="" work="" stopped=false restart_on_exit=false
compose=()

finish() {
    local rc=$?
    trap - EXIT HUP INT TERM
    set +e
    if [[ "$restart_on_exit" == true ]]; then
        if "${compose[@]}" start chamados-backend; then stopped=false; else rc=1; fi
    elif ((rc != 0)) && [[ "$action" == restore && "$stopped" == true ]]; then
        # A failed health check must not leave a partially recovered app writing.
        "${compose[@]}" stop chamados-backend
    fi
    python3 - "$operation" "$environment" "$rc" "$revision" "$backup_id" "$safety_backup_id" "$source_ref" "$stopped" <<'PY' >&3
import json, sys
operation, environment, rc, revision, backup_id, safety, ref, stopped = sys.argv[1:]
data = dict(ref=ref, backend_stopped=stopped == 'true')
if safety:
    data['safety_backup_id'] = safety
value = dict(schema_version=1, operation=operation, environment=environment,
             status='success' if rc == '0' else 'failed', revision=revision, data=data)
if backup_id:
    value['backup_id'] = backup_id
print(json.dumps(value))
PY
    [[ -z "$work" ]] || rm -rf -- "$work"
    unset CI_JOB_TOKEN
    exit "$rc"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM
fail() { echo "backup: $*" >&2; exit 1; }
case "$action" in create|restore|cleanup) ;; *) fail 'expected create|restore|cleanup';; esac
case "$environment" in staging|production) ;; *) fail 'invalid environment';; esac
[[ "$action" != restore || "$backup_id" =~ ^[0-9]{14}\.[0-9]+$ ]] || fail 'invalid backup_id'
for bin in python3 flock docker curl; do command -v "$bin" >/dev/null || fail "missing dependency: $bin"; done
if [[ "$environment" == production ]]; then command -v git >/dev/null || fail 'missing dependency: git'; fi
[[ -f "$helper" ]] || fail 'missing archive helper'
mapfile -d '' -t settings < <(python3 -c '
import json,sys
x=json.load(sys.stdin)
for key in ("CI_JOB_TOKEN","CI_API_V4_URL","CI_PROJECT_ID","CI_PIPELINE_IID","OPS_REF","OPS_CONFIRM","CI_PROJECT_PATH"):
    value=str(x.get(key,""))
    if "\x00" in value or "\n" in value or "\r" in value: raise ValueError("invalid settings")
    sys.stdout.write(value+"\x00")
')
[[ ${#settings[@]} == 7 && -n "${settings[0]}" ]] || fail 'missing/invalid job credentials'
export CI_JOB_TOKEN="${settings[0]}" CI_API_V4_URL="${settings[1]}" CI_PROJECT_ID="${settings[2]}"
export CI_PIPELINE_IID="${settings[3]}" OPS_REF="${settings[4]}" OPS_CONFIRM="${settings[5]}" OPS_ENVIRONMENT="$environment"
export CI_PROJECT_PATH="${settings[6]}"
unset settings
[[ "$CI_PIPELINE_IID" =~ ^[0-9]+$ ]] || fail 'invalid pipeline IID'
[[ "$action" != restore || "$OPS_CONFIRM" == "$environment" ]] || fail 'restore requires confirm equal to environment'
mkdir -p "$deploy_dir/incoming"
exec 9>"$deploy_dir/.deploy.lock"
flock -n -E 75 9 || exit $?
work="$(mktemp -d "$deploy_dir/incoming/backup.XXXXXXXX")"
if [[ "$action" == cleanup ]]; then
    python3 "$helper" cleanup
    exit
fi
docker compose version >/dev/null
project="${BACKUP_COMPOSE_PROJECT:-chamados-stg}"
if [[ "$environment" == production ]]; then project="${BACKUP_COMPOSE_PROJECT:-chamados-prod}"; fi
use_compose() {
    local compose_file="$1/infrastructure/application/docker-compose.yml"
    [[ -f "$compose_file" ]] || compose_file="$1/docker-compose.yml"
    compose=(docker compose -p "$project" --env-file "$deploy_dir/.env"
        -f "$compose_file" -f "$deploy_dir/tooling/deploy/docker-compose.$environment.yml")
}
[[ -f "$deploy_dir/.env" && -f "$deploy_dir/app/REVISION" ]] || fail 'no existing deployment to back up'
use_compose "$deploy_dir/app"
"${compose[@]}" config --quiet
revision="$(python3 "$helper" resolve-revision "$(cat "$deploy_dir/app/REVISION")")"
source_ref="$(cat "$deploy_dir/app/REF" 2>/dev/null || printf '%s' "$revision")"
# Capturing a resolvable legacy deployment is allowed. The protected-tag policy
# applies to the restoration target, including when current data needs a safety copy.
db_container="$("${compose[@]}" ps -q --status running chamados-postgresql)"
[[ -n "$db_container" ]] || fail 'PostgreSQL must be running before backup/restore'
db_image="$(docker inspect -f '{{.Image}}' "$db_container")"
docker run --rm --network none --read-only --entrypoint tar "$db_image" --help >/dev/null 2>&1
volume="${project}_call_attachments"
docker volume inspect "$volume" >/dev/null
major="$("${compose[@]}" exec -T chamados-postgresql sh -eu -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc "SHOW server_version_num"')"
[[ "$major" =~ ^[0-9]+$ ]] || fail 'cannot identify PostgreSQL major'
major="$((major / 10000))"
current_revision="$revision" current_ref="$source_ref"

capture() {
    local capture_revision="$1" capture_ref="$2" destination="$3"
    mkdir -p "$destination/raw"
    "${compose[@]}" exec -T chamados-postgresql sh -eu -c 'exec pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" --format=custom --no-owner --no-privileges' >"$destination/raw/database.dump"
    docker run --rm --network none --read-only -v "$volume:/snapshot:ro" --entrypoint tar "$db_image" -C /snapshot -cf - . >"$destination/raw/attachments.tar"
    python3 - "$environment" "$capture_revision" "$capture_ref" "$major" >"$destination/metadata.json" <<'PY'
import datetime,json,sys
env, revision, ref, major=sys.argv[1:]
print(json.dumps(dict(schema_version=1, environment=env, revision=revision, ref=ref,
                     postgres_major=int(major), created_at=datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'))))
PY
    python3 "$helper" pack "$destination/raw" "$destination/metadata.json" "$destination/package"
}
stop_backend() {
    stopped=true
    "${compose[@]}" stop chamados-backend
}
publish_snapshot() {
    local directory="$1" id
    id="$(date -u +%Y%m%d%H%M%S).$CI_PIPELINE_IID"
    python3 "$helper" publish "$directory/package" "$id"
    published_id="$id"
}
health() {
    local url="$1" deadline=$((SECONDS + ${BACKUP_HEALTH_TIMEOUT:-240}))
    until curl --fail --silent --show-error --max-time 5 -o /dev/null "$url"; do
        ((SECONDS < deadline)) || return 1
        sleep 2
    done
}

if [[ "$action" == create ]]; then
    if [[ -n "$("${compose[@]}" ps -q --status running chamados-backend)" ]]; then restart_on_exit=true; fi
    stop_backend
    capture "$revision" "$source_ref" "$work/snapshot"
    publish_snapshot "$work/snapshot"
    backup_id="$published_id"
    exit
fi

# Everything above and through the build is read-only with respect to live data.
python3 "$helper" download "$backup_id" "$work/download"
python3 "$helper" verify "$work/download/backup.tar.gz" "$work/download/manifest.json" "$environment" "$major" "$work/verified"
mapfile -t target < <(python3 - "$work/verified/manifest.json" <<'PY'
import json,sys
x=json.load(open(sys.argv[1])); print(x['revision']); print(x['ref'])
PY
)
[[ ${#target[@]} == 2 ]] || fail 'invalid snapshot metadata'
revision="${target[0]}" source_ref="${target[1]}"
if [[ "$environment" == production ]]; then
    if [[ "$OPS_REF" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then source_ref="$OPS_REF"; fi
    python3 "$helper" verify-production "$revision" "$source_ref"
fi
# pg_restore --list detects malformed/custom-format incompatibility before pausing.
"${compose[@]}" exec -T chamados-postgresql pg_restore --list <"$work/verified/database.dump" >/dev/null
python3 "$helper" source-download "$revision" "$work/source.tar.gz"
python3 "$helper" source-extract "$work/source.tar.gz" "$work/app"
if [[ -f "$work/app/infrastructure/application/docker-compose.yml" ]]; then
    [[ -f "$work/app/infrastructure/application/backend.Dockerfile" && -f "$work/app/infrastructure/application/frontend.Dockerfile" ]] || fail 'revision lacks compatible Docker build'
else
    [[ -f "$work/app/docker-compose.yml" && -f "$work/app/Dockerfile" && -f "$work/app/src/web/Dockerfile" ]] || fail 'legacy revision lacks compatible Docker build'
fi
printf '%s\n' "$revision" >"$work/app/REVISION"
printf '%s\n' "$source_ref" >"$work/app/REF"
use_compose "$work/app"
"${compose[@]}" config --quiet
"${compose[@]}" build chamados-backend
"${compose[@]}" build chamados-frontend
# Return to the running revision for the safety snapshot, under the same flock.
use_compose "$deploy_dir/app"
stop_backend
capture "$current_revision" "$current_ref" "$work/safety"
publish_snapshot "$work/safety"
safety_backup_id="$published_id"
echo "Safety backup published: $safety_backup_id"
# A failure from this point always leaves the backend stopped. Never invoke the
# normal deploy executor: it may apply migrations after the recovered schema.
"${compose[@]}" exec -T chamados-postgresql sh -eu -c 'dropdb -U "$POSTGRES_USER" --if-exists --force "$POSTGRES_DB"; createdb -U "$POSTGRES_USER" -O "$POSTGRES_USER" "$POSTGRES_DB"'
"${compose[@]}" exec -T chamados-postgresql sh -eu -c 'exec pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --exit-on-error --no-owner --no-privileges' <"$work/verified/database.dump"
docker run --rm -i --network none --read-only -v "$volume:/snapshot" --entrypoint sh "$db_image" -eu -c \
    'find /snapshot -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +; tar -xf - -C /snapshot' <"$work/verified/attachments.tar"
mv "$deploy_dir/app" "$work/previous-app"
mv "$work/app" "$deploy_dir/app"
use_compose "$deploy_dir/app"
cat >"$work/no-migrations.yml" <<'YAML'
services:
  chamados-backend:
    environment:
      SPRING_LIQUIBASE_ENABLED: "false"
      APP_INITIALIZER_ENABLED: "false"
YAML
compose+=(-f "$work/no-migrations.yml")
"${compose[@]}" up -d --no-build --remove-orphans
health "${BACKUP_BACKEND_HEALTH_URL:-http://127.0.0.1:8181/api/auth/csrf}"
health "${BACKUP_FRONTEND_HEALTH_URL:-http://127.0.0.1/}"
stopped=false
