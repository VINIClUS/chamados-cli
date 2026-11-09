#!/usr/bin/env bash
set -euo pipefail

# Integration smoke test for the deployed containers. Credentials are read as
# literal values because Docker .env accepts spaces without shell quoting.
read_env_value() {
    local key="$1"
    grep -m1 "^${key}=" .env | cut -d= -f2-
}

admin_email="$(read_env_value APP_INITIALIZER_ADMIN_EMAIL)"
admin_password="$(read_env_value APP_INITIALIZER_ADMIN_PASSWORD)"

api_url="${1:-http://127.0.0.1:8181/api}"
cookie_jar="$(mktemp)"
trap 'rm -f "$cookie_jar"' EXIT
curl_wait=(--retry 20 --retry-all-errors --retry-delay 1)

csrf_token="$(curl --fail --silent --show-error "${curl_wait[@]}" --cookie-jar "$cookie_jar" "$api_url/auth/csrf" | jq -r '.token')"
login_payload="$(jq -n \
    --arg email "$admin_email" \
    --arg password "$admin_password" \
    '{email: $email, password: $password}')"

curl --fail --silent --show-error \
    --cookie "$cookie_jar" \
    --cookie-jar "$cookie_jar" \
    --header 'Content-Type: application/json' \
    --header "X-XSRF-TOKEN: $csrf_token" \
    --data "$login_payload" \
    "$api_url/auth/login" >/dev/null

secretary_id="$(curl --fail --silent --show-error --cookie "$cookie_jar" "$api_url/auth/me" | jq -er '.secretaries[0].id')"
common_headers=(--cookie "$cookie_jar" --header "X-Active-Secretary-Id: $secretary_id")

dashboard_response="$(curl --fail --silent --show-error "${common_headers[@]}" "$api_url/dashboard/calls")"
echo "$dashboard_response" | jq -e '
    (.totalActive | type == "number") and
    (.urgent | type == "number") and
    (.recentCalls | type == "array")' >/dev/null

filter_response="$(curl --fail --silent --show-error "${common_headers[@]}" \
    --get "$api_url/calls/filter" \
    --data-urlencode 'search=' \
    --data-urlencode 'status=OPENED' \
    --data-urlencode 'priority=NORMAL' \
    --data-urlencode 'unassigned=true' \
    --data-urlencode 'startDate=2020-01-01' \
    --data-urlencode 'endDate=2035-12-31' \
    --data-urlencode 'page=0' \
    --data-urlencode 'size=10')"
echo "$filter_response" | jq -e '
    (.content | type == "array") and
    (.page.number | type == "number") and
    (.page.totalElements | type == "number")' >/dev/null

printf 'Dashboard de chamados: OK\nConsulta combinada de chamados: OK\n'
