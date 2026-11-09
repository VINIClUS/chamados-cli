#!/usr/bin/env bash
set -euo pipefail
[[ "${CI_COMMIT_BRANCH:-}" == main && "${CI_COMMIT_REF_PROTECTED:-}" == true ]] || {
    echo 'ERRO: operações exigem main protegida' >&2; exit 2;
}
ref="${OPS_REF:-main}"
environment="${OPS_ENVIRONMENT:-staging}"
case "$environment" in staging | production) ;; *) exit 2 ;; esac
# -- separates options, but refs must also have an unambiguous branch/tag/SHA spelling.
[[ "$ref" != -* && "$ref" != *..* && "$ref" != *'@{'* ]] || exit 2
if [[ ! "$ref" =~ ^[0-9a-fA-F]{40}$ ]]; then git check-ref-format "refs/heads/$ref"; fi
if [[ "$environment" == production ]]; then
    [[ "$ref" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'ERRO: produção exige tag vX.Y.Z' >&2; exit 2; }
fi
mkdir -p .ops-target
git fetch --no-tags origin "$ref"
revision="$(git rev-parse --verify 'FETCH_HEAD^{commit}')"
if [[ "$environment" == production ]]; then
    git fetch --no-tags origin '+refs/heads/main:refs/remotes/origin/main'
    git merge-base --is-ancestor "$revision" refs/remotes/origin/main
    python3 "$(dirname "${BASH_SOURCE[0]}")/metadata.py" protected-tag "$ref" "$revision"
fi
# Executable operational tooling is excluded: only the protected main checkout can supply it.
repository="$(git rev-parse --show-toplevel)"
git -C "$repository" archive --format=tar "$revision" -- . ':(exclude)android' ':(exclude)scripts' ':(exclude)devops' ':(exclude)cicd' \
    ':(exclude)infrastructure/environments' ':(exclude)infrastructure/nginx' ':(exclude)infrastructure/scripts' \
    ':(exclude)infrastructure/legacy' ':(exclude)infrastructure/README.md' \
    ':(exclude).gitlab-ci.yml' ':(exclude).gitlab' >.ops-target/source.tar
python3 "$(dirname "${BASH_SOURCE[0]}")/metadata.py" write "$revision" "$ref"
