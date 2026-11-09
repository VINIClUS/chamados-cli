#!/usr/bin/env bash
set -euo pipefail

: "${CI_API_V4_URL:?}" "${CI_PROJECT_ID:?}" "${CI_JOB_TOKEN:?}" "${CI_COMMIT_TAG:?}"
[[ "$CI_COMMIT_TAG" =~ ^cli-v([0-9]+\.[0-9]+\.[0-9]+)$ ]] || { echo 'Tag de CLI inválida' >&2; exit 2; }
version="${BASH_REMATCH[1]}"
dist="${1:-.cli-dist}"
api="$CI_API_V4_URL/projects/$CI_PROJECT_ID"
package_url="$api/packages/generic/chamadosctl/$version"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl_args=(--fail-with-body --silent --show-error --header "JOB-TOKEN: $CI_JOB_TOKEN")
files=(SHA256SUMS)
for target_os in linux darwin windows; do
    for target_arch in amd64 arm64; do
        name="chamadosctl_${version}_${target_os}_${target_arch}"
        [[ "$target_os" != windows ]] || name+=.exe
        files+=("$name")
    done
done
(cd "$dist" && sha256sum --check SHA256SUMS)
printf '[]' > "$tmp/links.json"
for name in "${files[@]}"; do
    # A versão publicada é imutável. Retry confirma os mesmos bytes antes de reutilizar um arquivo.
    code="$(curl --silent --show-error --header "JOB-TOKEN: $CI_JOB_TOKEN" \
        --output "$tmp/existing" --write-out '%{http_code}' "$package_url/$name")"
    case "$code" in
        200) cmp -- "$dist/$name" "$tmp/existing" || { echo "Arquivo já publicado com conteúdo diferente: $name" >&2; exit 1; } ;;
        404) curl "${curl_args[@]}" --upload-file "$dist/$name" "$package_url/$name" > /dev/null ;;
        *) echo "Falha ao consultar pacote (HTTP $code)" >&2; exit 1 ;;
    esac
    jq --arg name "$name" --arg url "$package_url/$name" \
        '. + [{name:$name,url:$url,link_type:"package"}]' "$tmp/links.json" > "$tmp/next.json"
    mv "$tmp/next.json" "$tmp/links.json"
done
jq -n --arg tag "$CI_COMMIT_TAG" --arg name "chamadosctl $version" \
    --arg description 'CLI independente de Deploy & Operations. Confira SHA256SUMS antes de instalar. Token GitLab individual; operações executadas pelo CI/CD.' \
    --slurpfile links "$tmp/links.json" \
    '{tag_name:$tag,name:$name,description:$description,assets:{links:$links[0]}}' > "$tmp/release.json"
code="$(curl --silent --show-error --header "JOB-TOKEN: $CI_JOB_TOKEN" --output "$tmp/release" \
    --write-out '%{http_code}' "$api/releases/$CI_COMMIT_TAG")"
case "$code" in
    200)
        while IFS= read -r link; do
            name="$(jq -r .name <<< "$link")"
            url="$(jq -r .url <<< "$link")"
            existing="$(jq -r --arg name "$name" '.assets.links[]? | select(.name == $name) | .url' "$tmp/release")"
            if [[ -n "$existing" ]]; then
                [[ "$existing" == "$url" ]] || { echo "Link existente aponta para outro artefato: $name" >&2; exit 1; }
            else
                curl "${curl_args[@]}" --header 'Content-Type: application/json' \
                    --data "$link" "$api/releases/$CI_COMMIT_TAG/assets/links" > /dev/null
            fi
        done < <(jq -c '.[]' "$tmp/links.json")
        echo "Links do release conferidos: $CI_COMMIT_TAG"
        ;;
    404) curl "${curl_args[@]}" --header 'Content-Type: application/json' \
        --data-binary "@$tmp/release.json" "$api/releases" > /dev/null ;;
    *) echo "Falha ao consultar release (HTTP $code)" >&2; exit 1 ;;
esac
printf 'Release publicado: %s\n' "$CI_COMMIT_TAG"
