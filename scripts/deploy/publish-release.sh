#!/usr/bin/env bash
set -euo pipefail

# Cria o Release da tag da pipeline, depois do deploy em produção (job release:publish do .gitlab-ci.yml). Quem cria o
# Release pela UI já cria a tag junto, e a pipeline chega aqui com ele pronto: nesse caso não há o que fazer. Por isso o
# job não usa o release: do GitLab, que falha quando o Release já existe.
# As notas são os títulos dos MRs desde a tag anterior. Precisa do histórico inteiro (GIT_DEPTH: 0).

: "${CI_COMMIT_TAG:?Este script roda na pipeline de uma tag}"
api="$CI_API_V4_URL/projects/$CI_PROJECT_ID/releases"

# O token vai por um arquivo de cabeçalho (process substitution), nunca pela linha de comando.
gitlab() {
    curl -sS --retry 5 --retry-delay 3 -o /dev/null -w '%{http_code}' -H @<(printf 'JOB-TOKEN: %s\n' "$CI_JOB_TOKEN") "$@"
}

status="$(gitlab "$api/$CI_COMMIT_TAG")"
case "$status" in
    200)
        echo "OK: o Release $CI_COMMIT_TAG já existia."
        exit 0
        ;;
    404) ;;
    *)
        echo "ERRO: não consegui consultar o Release $CI_COMMIT_TAG (HTTP $status)" >&2
        exit 1
        ;;
esac

# O DNS da LAN deixa de responder por alguns segundos de vez em quando.
for attempt in 1 2 3; do
    git fetch --quiet --tags origin && break
    ((attempt < 3)) || exit 1
    sleep 10
done

# O corpo do commit de merge do GitLab começa pelo título do MR.
merge_titles() {
    git log --merges --first-parent --format=%b "$1" | grep -v '^See merge request ' | sed '/^$/d' | sed 's/^/- /' || true
}

previous="$(git describe --tags --abbrev=0 --match 'v[0-9]*' "$CI_COMMIT_TAG^" 2>/dev/null || true)"
if [[ -n "$previous" ]]; then
    changes="$(merge_titles "$previous..$CI_COMMIT_TAG")"
    notes="Mudanças desde $previous:"$'\n\n'"${changes:-Nenhum merge desde a versão anterior.}"
else
    notes="Primeira versão publicada pela pipeline."
fi

payload="$(jq -n --arg tag "$CI_COMMIT_TAG" --arg notes "$notes" '{tag_name: $tag, name: $tag, description: $notes}')"
status="$(gitlab -X POST -H 'Content-Type: application/json' --data "$payload" "$api")"
# 409: uma repetição do curl encontrou o Release criado pela tentativa anterior.
if [[ "$status" != 201 && "$status" != 409 ]]; then
    echo "ERRO: não consegui criar o Release $CI_COMMIT_TAG (HTTP $status)" >&2
    exit 1
fi
echo "OK: $CI_PROJECT_URL/-/releases/$CI_COMMIT_TAG"
