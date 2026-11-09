#!/usr/bin/env bash
set -euo pipefail

# Deploy nos CTs do pve-01: nginx do CT 110 (TLS com Let's Encrypt) -> frontend do CT na porta 80.
#   staging     CT 180 (chamados-stg)   https://stg-chamados.portosoftware.com.br
#   production  CT 181 (chamados-prod)  https://chamados.portosoftware.com.br
#
# Uso:
#   scripts/deploy.sh staging [deploy] [--ref <ref>] [--keep-data] [--smoke]
#   scripts/deploy.sh staging reset
#   Produção: use chamadosctl deploy production --ref vX.Y.Z (execução exclusiva pelo CI)
#   scripts/deploy.sh <ambiente> status
#   scripts/deploy.sh <ambiente> logs [serviço]
#
# Staging: sem --ref, publica a working tree; com --ref, publica o commit indicado.
# Deploy preserva banco e anexos. Reset exige --confirm staging e é exclusivo de staging.
# Produção: somente a pipeline protegida pode enviar o arquivo de fonte testado,
# com SHA completo e tag vX.Y.Z protegida. Produção nunca aceita reset nem smoke autenticado.
#
# Os scripts de deploy sempre vêm da working tree local, então um --ref antigo também funciona.
# --smoke roda scripts/verify-call-endpoints.sh (login e consultas, sem gravar chamados).
#
# Variáveis: DEPLOY_SSH, DEPLOY_SSH_KEY e DEPLOY_URL sobrescrevem o alvo do ambiente. DEPLOY_EDGE_IP manda os checks da
# URL pública para esse IP, a borda: quem está na LAN dos CTs, como o runner, não alcança a URL pública (sem hairpin NAT).

environment="${1:-}"
case "$environment" in
    staging)
        DEPLOY_SSH="${DEPLOY_SSH:-root@10.10.10.180}"
        DEPLOY_SSH_KEY="${DEPLOY_SSH_KEY:-$HOME/.ssh/chamados-stg_ed25519}"
        DEPLOY_URL="${DEPLOY_URL:-https://stg-chamados.portosoftware.com.br}"
        DEPLOY_PROJECT=chamados-stg
        ;;
    production)
        DEPLOY_SSH="${DEPLOY_SSH:-root@10.10.10.181}"
        DEPLOY_SSH_KEY="${DEPLOY_SSH_KEY:-$HOME/.ssh/chamados-prod_ed25519}"
        DEPLOY_URL="${DEPLOY_URL:-https://chamados.portosoftware.com.br}"
        DEPLOY_PROJECT=chamados-prod
        ;;
    -h | --help)
        sed -n '4,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
        exit 0
        ;;
    *)
        echo "Use: scripts/deploy.sh <staging|production> [comando] (detalhes em --help)" >&2
        exit 2
        ;;
esac
shift
DEPLOY_DIR=/opt/chamados

repo_root="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
scripts_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
infrastructure_dir="$repo_root/infrastructure"

ssh_opts=(-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new)
if [[ -f "$DEPLOY_SSH_KEY" ]]; then
    ssh_opts+=(-i "$DEPLOY_SSH_KEY" -o IdentitiesOnly=yes)
fi

remote() {
    # shellcheck disable=SC2029 # os comandos são montados aqui de propósito
    ssh "${ssh_opts[@]}" "$DEPLOY_SSH" "$@"
}

remote_compose="compose_file=$DEPLOY_DIR/app/infrastructure/application/docker-compose.yml; \
[ -f \"\$compose_file\" ] || compose_file=$DEPLOY_DIR/app/docker-compose.yml; \
docker compose -p $DEPLOY_PROJECT --env-file $DEPLOY_DIR/.env \
-f \"\$compose_file\" -f $DEPLOY_DIR/tooling/deploy/docker-compose.$environment.yml"

# O app Android não entra na imagem; .idea e .env nunca vão para o servidor.
is_excluded() {
    case "$1" in
        infrastructure/application/*) return 1 ;;
        infrastructure/*) return 0 ;;
        scripts/deploy/* | android/* | devops/* | cicd/* | .idea/* | .claude/* | .env) return 0 ;;
        *) return 1 ;;
    esac
}

package_app() {
    local ref="$1"
    if [[ -n "$ref" ]]; then
        git -C "$repo_root" archive --format=tar "$ref" -- . ':(exclude)android' ':(exclude)devops' ':(exclude)cicd' ':(exclude).idea' \
            ':(exclude)infrastructure/environments' ':(exclude)infrastructure/nginx' \
            ':(exclude)infrastructure/scripts' ':(exclude)infrastructure/legacy' ':(exclude)infrastructure/README.md' \
            ':(exclude)scripts/deploy'
        return
    fi
    local file
    git -C "$repo_root" ls-files -z --cached --others --exclude-standard |
        while IFS= read -r -d '' file; do
            if is_excluded "$file"; then
                continue
            fi
            # Arquivos versionados mas apagados na working tree também aparecem no ls-files.
            if [[ -e "$repo_root/$file" || -L "$repo_root/$file" ]]; then
                printf '%s\0' "$file"
            fi
        done |
        tar -C "$repo_root" --null -T - -cf -
}

revision_of() {
    local ref="$1"
    if [[ -n "$ref" ]]; then
        git -C "$repo_root" rev-parse --short=12 "$ref^{commit}"
        return
    fi
    local sha
    sha="$(git -C "$repo_root" rev-parse --short=12 HEAD)"
    if [[ -n "$(git -C "$repo_root" status --porcelain -- . ':(exclude)android')" ]]; then
        sha="$sha-dirty"
    fi
    printf '%s\n' "$sha"
}

check_public() {
    local path="$1" code resolve=() host="${DEPLOY_URL#https://}"
    if [[ -n "${DEPLOY_EDGE_IP:-}" ]]; then
        resolve=(--resolve "${host%%/*}:443:$DEPLOY_EDGE_IP")
    fi
    code="$(curl -sS "${resolve[@]}" -o /dev/null -w '%{http_code}' --retry 5 --retry-all-errors --retry-delay 3 "$DEPLOY_URL$path" || true)"
    if [[ "$code" != 200 ]]; then
        echo "ERRO: $DEPLOY_URL$path respondeu HTTP $code" >&2
        exit 1
    fi
    echo "OK: $DEPLOY_URL$path"
}

new_incoming() {
    printf '%s/incoming/%s-%s\n' "$DEPLOY_DIR" "$(date +%Y%m%d%H%M%S)" "$$"
}

send_tooling() (
    local incoming="$1" stage
    stage="$(mktemp -d)"
    trap 'rm -rf -- "$stage"' EXIT
    mkdir -p "$stage/deploy"
    cp "$scripts_dir/deploy/"*.sh "$stage/deploy/"
    for pattern in "$scripts_dir/deploy/"*.yml "$scripts_dir/deploy/"*.conf; do
        [[ -e "$pattern" ]] && cp "$pattern" "$stage/deploy/"
    done
    # Historical protected checkouts kept these files under infrastructure/.
    if [[ -d "$infrastructure_dir" ]]; then
        for pattern in "$infrastructure_dir/scripts/"*.sh "$infrastructure_dir/environments/"*.yml "$infrastructure_dir/nginx/"*.conf; do
            [[ -e "$pattern" ]] && cp "$pattern" "$stage/deploy/"
        done
    fi
    cp "$scripts_dir/verify-call-endpoints.sh" "$stage/"
    remote "mkdir -p $incoming/tooling"
    tar -C "$stage" -cf - deploy verify-call-endpoints.sh |
        remote "tar -xf - -C $incoming/tooling"
)

# Roda o remote-deploy.sh do envio sob o lock do CT: deploy e reset nunca correm juntos.
# flock -E 75 separa "outro deploy em andamento" de uma falha do próprio deploy.
run_locked() {
    local incoming="$1" revision="$2" data_mode="$3" source_ref="${4:-}" rc=0 quoted
    # The fixed wrapper writes REF under the same lock as deployment. The source ref is a positional argument.
    printf -v quoted '%q ' "$incoming" "$revision" "$DEPLOY_URL" "$data_mode" "$environment" "$source_ref"
    remote "flock -n -E 75 $DEPLOY_DIR/.deploy.lock bash -c \
        'bash \"\$1/tooling/deploy/remote-deploy.sh\" \"\$1\" \"\$2\" \"\$3\" \"\$4\" \"\$5\" && \
        { [ \"\$4\" = reset-only ] || printf \"%s\\n\" \"\$6\" >/opt/chamados/app/REF; }' _ $quoted </dev/null" || rc=$?
    if ((rc == 75)); then
        remote "rm -rf $incoming"
        echo "ERRO: outro deploy está em andamento em $environment." >&2
        exit 1
    elif ((rc != 0)); then
        exit "$rc"
    fi
}

cmd_deploy() {
    local ref="" smoke=false data_mode=keep archive="" exact_revision="" source_ref=""
    if [[ "$environment" == production ]]; then
        data_mode=keep
    fi
    while (($#)); do
        case "$1" in
            --ref)
                ref="${2:?--ref precisa de um valor}"
                shift 2
                ;;
            --keep-data)
                data_mode=keep
                shift
                ;;
            --archive) archive="${2:?--archive precisa de um arquivo}"; shift 2 ;;
            --revision) exact_revision="${2:?--revision precisa de um SHA}"; shift 2 ;;
            --source-ref) source_ref="${2:?--source-ref precisa de uma ref}"; shift 2 ;;
            --smoke)
                smoke=true
                shift
                ;;
            *)
                echo "Opção desconhecida: $1" >&2
                exit 2
                ;;
        esac
    done

    if [[ "$environment" == production ]]; then
        if $smoke; then
            echo 'ERRO: smoke autenticado só é permitido em staging' >&2; exit 2
        fi
        local production_ref="${source_ref:-$ref}"
        [[ "$production_ref" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
            echo 'ERRO: produção exige uma tag vX.Y.Z' >&2; exit 2;
        }
        [[ -n "$archive" && "${CI_COMMIT_BRANCH:-}" == main && "${CI_COMMIT_REF_PROTECTED:-}" == true && -n "${CI_JOB_TOKEN:-}" ]] || {
            echo 'ERRO: deploy de produção só é permitido pelo fluxo validado do GitLab; use chamadosctl' >&2; exit 2;
        }
    fi

    if [[ -n "$archive" ]]; then
        [[ -f "$archive" && "$exact_revision" =~ ^[0-9a-f]{40}$ && -n "$source_ref" && -z "$ref" ]] || {
            echo 'ERRO: archive exige arquivo, SHA completo e source-ref, sem --ref' >&2; exit 2;
        }
        [[ "$source_ref" != -* ]] && git check-ref-format "refs/heads/$source_ref" || exit 2
        if [[ "$environment" == production ]]; then
            python3 "$scripts_dir/operations/metadata.py" protected-tag "$source_ref" "$exact_revision"
        fi
    elif [[ -n "$exact_revision" || -n "$source_ref" ]]; then
        echo 'ERRO: --revision e --source-ref exigem --archive' >&2; exit 2
    fi

    if [[ "$environment" == production && -z "$archive" ]]; then
        git -C "$repo_root" fetch --quiet origin "+refs/tags/$ref:refs/tags/$ref" '+refs/heads/main:refs/remotes/origin/main'
        git -C "$repo_root" merge-base --is-ancestor "$ref^{commit}" origin/main || {
            echo 'ERRO: a tag de produção não pertence a main' >&2; exit 2;
        }
    fi

    local revision incoming
    if [[ -n "$archive" ]]; then
        revision="$exact_revision"
    else
        revision="$(revision_of "$ref")"
        source_ref="${ref:-working-tree}"
    fi
    incoming="$(new_incoming)"

    if [[ "$data_mode" == keep ]]; then
        echo "==> Enviando $revision para $DEPLOY_SSH (mantendo os dados atuais)"
    else
        echo "==> Enviando $revision para $DEPLOY_SSH (banco e anexos serão zerados após o build)"
    fi
    send_tooling "$incoming"
    if [[ -n "$archive" ]]; then
        remote "cat > $incoming/app.tar" <"$archive"
    else
        package_app "$ref" | remote "cat > $incoming/app.tar"
    fi
    run_locked "$incoming" "$revision" "$data_mode" "$source_ref"

    echo "==> Verificando pela URL pública"
    check_public /
    check_public /api/auth/csrf

    if $smoke; then
        echo "==> Smoke test"
        # O CT não alcança a própria URL pública (sem hairpin NAT). Em "localhost" o curl aceita os cookies Secure.
        remote "cd $DEPLOY_DIR && bash tooling/verify-call-endpoints.sh http://localhost:8181/api"
    fi
    echo "Publicado em $environment: $DEPLOY_URL ($revision)"
}

cmd_reset() {
    if [[ "$environment" == production ]]; then
        echo "ERRO: produção não tem reset; os dados nunca são apagados por este script." >&2
        exit 2
    fi
    [[ $# == 2 && "$1" == --confirm && "$2" == "$environment" ]] || {
        echo 'ERRO: reset exige --confirm staging' >&2; exit 2;
    }
    local incoming
    incoming="$(new_incoming)"
    echo "==> Zerando banco e anexos do staging em $DEPLOY_SSH"
    send_tooling "$incoming"
    run_locked "$incoming" "" reset-only

    echo "==> Verificando pela URL pública"
    check_public /
    check_public /api/auth/csrf
    echo "Dados do staging zerados: $DEPLOY_URL"
}

cmd_status() {
    remote "printf 'Revisão: '; cat $DEPLOY_DIR/app/REVISION 2>/dev/null || echo '(nenhum deploy)'; \
        printf 'Dados desde: '; docker volume inspect -f '{{.CreatedAt}}' ${DEPLOY_PROJECT}_postgres_data 2>/dev/null || echo '(sem banco)'; \
        $remote_compose ps --format 'table {{.Service}}\t{{.Status}}\t{{.Ports}}'"
}

cmd_logs() {
    ssh -t "${ssh_opts[@]}" "$DEPLOY_SSH" "$remote_compose logs --tail 200 -f ${1:-}"
}

command="${1:-deploy}"
[[ $# -gt 0 ]] && shift
case "$command" in
    deploy) cmd_deploy "$@" ;;
    reset) cmd_reset "$@" ;;
    status) cmd_status ;;
    logs) cmd_logs "$@" ;;
    --ref | --keep-data | --smoke) cmd_deploy "$command" "$@" ;;
    *)
        echo "Comando desconhecido: $command (use deploy, reset, status ou logs)" >&2
        exit 2
        ;;
esac
