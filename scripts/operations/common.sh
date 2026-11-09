#!/usr/bin/env bash
# Shared validation and SSH transport. Inputs are never evaluated as shell code.
ops_error() { printf 'ERRO: %s\n' "$*" >&2; return 2; }

ops_validate() {
    case "$OPS_ENVIRONMENT" in staging | production) ;; *) ops_error 'ambiente inválido'; return 2 ;; esac
    case "$OPS_OPERATION" in deploy | status | logs | health | smoke | restart | reset | backup | restore) ;;
        *) ops_error 'operação inválida'; return 2 ;; esac
    case "${OPS_SERVICE:-}" in '' | chamados-backend | chamados-frontend | chamados-postgresql) ;;
        *) ops_error 'serviço inválido'; return 2 ;; esac
    if [[ ! "${OPS_TAIL:-200}" =~ ^[1-9][0-9]{0,4}$ ]] || ((10#${OPS_TAIL:-200} > 10000)); then
        ops_error 'tail deve ser de 1 a 10000'; return 2;
    fi
    if [[ "$OPS_OPERATION" == reset || "$OPS_OPERATION" == restore ]]; then
        [[ "${OPS_CONFIRM:-}" == "$OPS_ENVIRONMENT" ]] || { ops_error 'confirmação obrigatória'; return 2; }
    fi
    if [[ "$OPS_ENVIRONMENT" == production && ( "$OPS_OPERATION" == reset || "$OPS_OPERATION" == smoke ) ]]; then
        ops_error 'operação proibida em produção'; return 2
    fi
}

ops_target() {
    case "$OPS_ENVIRONMENT" in
        staging)
            DEPLOY_SSH="${DEPLOY_SSH:-root@10.10.10.180}"
            DEPLOY_SSH_KEY="${DEPLOY_SSH_KEY:-$HOME/.ssh/chamados-stg_ed25519}"
            ;;
        production)
            DEPLOY_SSH="${DEPLOY_SSH:-root@10.10.10.181}"
            DEPLOY_SSH_KEY="${DEPLOY_SSH_KEY:-$HOME/.ssh/chamados-prod_ed25519}"
            ;;
        *) ops_error 'ambiente inválido'; return 2 ;;
    esac
    ops_ssh_opts=(-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes)
    if [[ -f "$DEPLOY_SSH_KEY" ]]; then ops_ssh_opts+=(-i "$DEPLOY_SSH_KEY" -o IdentitiesOnly=yes); fi
}

ops_remote() {
    local quoted
    printf -v quoted '%q ' "$@"
    # Arguments are individually shell-escaped before the remote shell interprets them.
    # shellcheck disable=SC2029
    ssh "${ops_ssh_opts[@]}" "$DEPLOY_SSH" "$quoted"
}
