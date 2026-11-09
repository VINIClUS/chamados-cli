#!/usr/bin/env bash
set -euo pipefail

# Roda no CT do ambiente, chamado por scripts/deploy.sh já sob o flock.
# Argumentos: <diretório incoming do envio> <revisão> <URL pública> <modo dos dados> <ambiente>
# O incoming traz app.tar (código a publicar) e tooling/ (scripts de deploy da working tree local),
# separados para que um --ref antigo, sem estes arquivos, também possa ser publicado.
#
# Modos dos dados:
#   reset       build, depois apaga banco e anexos; o Init semeia de novo.
#   keep        build, dump do banco e mantém os volumes atuais. É o único modo aceito em produção.
#   reset-only  sem build nem release nova: apaga os volumes e sobe as imagens já construídas.
incoming="$1"
revision="$2"
public_url="$3"
data_mode="$4"
environment="$5"

deploy_dir=/opt/chamados

case "$environment" in
    staging)
        project=chamados-stg
        admin_name="Administrador do Staging"
        ;;
    production)
        project=chamados-prod
        admin_name="Administrador do Sistema"
        ;;
    *)
        echo "ERRO: ambiente desconhecido: $environment" >&2
        exit 2
        ;;
esac

case "$data_mode" in
    reset | keep | reset-only) ;;
    *)
        echo "ERRO: modo de dados desconhecido: $data_mode" >&2
        exit 2
        ;;
esac
if [[ "$incoming" != "$deploy_dir"/incoming/?* || "$incoming" == *..* ]]; then
    echo "ERRO: diretório de envio inválido: $incoming" >&2
    exit 2
fi
# Um deploy que falha no meio não deixa o envio acumulado no CT.
trap 'rm -rf "$incoming"' EXIT

# Última trava antes do down -v: produção nunca apaga volumes, venha o pedido de onde vier.
if [[ "$environment" == production && "$data_mode" != keep ]]; then
    echo "ERRO: produção só aceita o modo keep (recebido: $data_mode)" >&2
    exit 2
fi

ensure_packages() {
    local missing=()
    for bin in curl jq openssl python3 git; do
        command -v "$bin" >/dev/null || missing+=("$bin")
    done
    if ((${#missing[@]})); then
        echo "==> Instalando ${missing[*]}"
        apt-get update -qq
        DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${missing[@]}" >/dev/null
    fi
}

# Docker CE do repositório oficial. No pve-01 (lxc-pve 6.0.5-3, CT unprivileged com
# nesting=1,keyctl=1) funciona sem workaround de AppArmor.
ensure_docker() {
    if command -v docker >/dev/null && docker compose version >/dev/null 2>&1; then
        return
    fi
    echo "==> Instalando Docker CE"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq ca-certificates curl >/dev/null
    install -m 0755 -d /etc/apt/keyrings
    curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
    chmod a+r /etc/apt/keyrings/docker.asc
    # shellcheck disable=SC1091
    . /etc/os-release
    printf 'Types: deb\nURIs: https://download.docker.com/linux/debian\nSuites: %s\nComponents: stable\nSigned-By: /etc/apt/keyrings/docker.asc\n' \
        "$VERSION_CODENAME" >/etc/apt/sources.list.d/docker.sources
    apt-get update -qq
    apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin >/dev/null
}

# Os segredos são gerados aqui e nunca saem do CT.
ensure_env() {
    [[ -f "$deploy_dir/.env" ]] && return
    echo "==> Gerando $deploy_dir/.env"
    local host="${public_url#https://}"
    host="${host%%/*}"
    (
        umask 077
        cat >"$deploy_dir/.env" <<EOF
POSTGRES_USER=chamados
POSTGRES_PASSWORD=$(openssl rand -hex 24)
POSTGRES_DB=chamados
JWT_SECRET=$(openssl rand -hex 48)
JWT_ACCESS_TOKEN_MINUTES=30
COOKIE_SECURE=true
COOKIE_SAME_SITE=Strict
FRONTEND_URL=$public_url
MAIL_ENABLED=false
# O Init só semeia com o banco sem usuários. No staging precisa ficar ligado, porque todo deploy zera o banco
# (exceto com --keep-data). Na produção, serve ao primeiro boot e depois é desligado.
APP_INITIALIZER_ENABLED=true
APP_INITIALIZER_ADMIN_EMAIL=admin@$host
APP_INITIALIZER_ADMIN_NAME=$admin_name
APP_INITIALIZER_ADMIN_PASSWORD=$(openssl rand -base64 24 | tr -d '/+=')
EOF
    )
    echo "    Senha inicial do admin em $deploy_dir/.env (APP_INITIALIZER_ADMIN_PASSWORD)."
}

stage_release() {
    rm -rf "$deploy_dir/app.new"
    mkdir -p "$deploy_dir/app.new"
    tar -xf "$incoming/app.tar" -C "$deploy_dir/app.new"
    printf '%s\n' "$revision" >"$deploy_dir/app.new/REVISION"
}

# Só roda depois de um build bem-sucedido, para que app/REVISION nunca aponte para uma release que não compilou.
# Se a versão nova não subir depois disso, app/REVISION já é a dela: veja "Voltar uma versão" em docs/ci-cd.md.
install_release() {
    rm -rf "$deploy_dir/app" "$deploy_dir/tooling"
    mv "$deploy_dir/app.new" "$deploy_dir/app"
    mv "$incoming/tooling" "$deploy_dir/tooling"
}

use_compose() {
    local app="$1" tooling="$2"
    local compose_file="$app/infrastructure/application/docker-compose.yml"
    [[ -f "$compose_file" ]] || compose_file="$app/docker-compose.yml"
    compose=(docker compose -p "$project" --env-file "$deploy_dir/.env"
        -f "$compose_file"
        -f "$tooling/deploy/docker-compose.$environment.yml")
}

wait_for() {
    local name="$1" url="$2" deadline=$((SECONDS + 240))
    until curl -fs -o /dev/null "$url"; do
        if ((SECONDS >= deadline)); then
            echo "ERRO: $name não respondeu em $url" >&2
            "${compose[@]}" ps >&2
            "${compose[@]}" logs --tail 80 chamados-backend >&2
            exit 1
        fi
        sleep 3
    done
    echo "OK: $name"
}

env_value() {
    grep -m1 "^$1=" "$deploy_dir/.env" | cut -d= -f2-
}

# Zerar sem o Init deixaria o ambiente sem dados e sem admin, e o wait_for_seed passaria direto.
assert_seed_enabled() {
    [[ "$(env_value APP_INITIALIZER_ENABLED)" == true ]] && return
    echo "ERRO: zerar os dados exige APP_INITIALIZER_ENABLED=true em $deploy_dir/.env (ou use --keep-data)" >&2
    exit 1
}

# Ponto de restauração antes de a versão nova rodar os changesets, que não têm volta. Os anexos ficam de fora, porque o
# deploy não os altera. Roda depois do build: um dump que falha aborta o deploy com a release atual ainda no ar.
dump_database() {
    # Só o primeiro deploy, sem volume, não tem o que guardar. Um banco parado sobe aqui, ou a versão nova rodaria sobre os
    # dados reais sem ponto de restauração.
    if ! docker volume inspect "${project}_postgres_data" >/dev/null 2>&1; then
        echo "==> Primeiro deploy: sem banco para guardar"
        return
    fi
    if [[ -z "$("${compose[@]}" ps -q --status running chamados-postgresql)" ]]; then
        echo "==> Subindo o banco para o dump"
        "${compose[@]}" up -d --wait chamados-postgresql
    fi
    local dir="$deploy_dir/backups" file
    file="$dir/pre-deploy-$(date +%Y%m%d%H%M%S)-$(cat "$deploy_dir/app/REVISION" 2>/dev/null || echo sem-revisao).sql.gz"
    echo "==> Dump do banco"
    (
        umask 077
        mkdir -p "$dir"
        rm -f "$dir"/*.tmp
        "${compose[@]}" exec -T chamados-postgresql pg_dump -U "$(env_value POSTGRES_USER)" \
            -d "$(env_value POSTGRES_DB)" --no-owner --no-privileges | gzip >"$file.tmp"
    )
    mv "$file.tmp" "$file"
    # O nome começa pela data: a ordem alfabética é a cronológica.
    find "$dir" -maxdepth 1 -name 'pre-deploy-*.sql.gz' | sort -r | tail -n +6 | xargs -r rm -f --
    echo "OK: $file ($(du -h "$file" | cut -f1))"
}

# Apaga banco e anexos. O .env fica: as senhas do Postgres e do admin valem para os volumes novos.
wipe_data() {
    echo "==> Zerando banco e anexos"
    "${compose[@]}" down -v --remove-orphans
    # Volumes do projeto que o compose atual não declara mais (por exemplo, renomeados) também saem.
    docker volume ls -q --filter "label=com.docker.compose.project=$project" | xargs -r docker volume rm >/dev/null
}

# O Init grava tudo numa única transação (~1 min num banco novo): a API já responde antes de o
# admin existir, então o deploy só termina quando a carga inicial foi commitada.
wait_for_seed() {
    [[ "$(env_value APP_INITIALIZER_ENABLED)" == true ]] || return 0
    local deadline=$((SECONDS + 300)) users
    while :; do
        users="$("${compose[@]}" exec -T chamados-postgresql psql -U "$(env_value POSTGRES_USER)" \
            -d "$(env_value POSTGRES_DB)" -Atc 'select count(*) from users' 2>/dev/null || true)"
        [[ "$users" =~ ^[1-9] ]] && break
        if ((SECONDS >= deadline)); then
            echo "ERRO: a carga inicial não terminou em 5 min" >&2
            "${compose[@]}" logs --tail 80 chamados-backend >&2
            exit 1
        fi
        sleep 5
    done
    echo "OK: carga inicial ($users usuário(s))"
}

if [[ "$data_mode" == reset-only && ! -d "$deploy_dir/app" ]]; then
    echo "ERRO: não há deploy anterior para zerar; rode um deploy" >&2
    exit 1
fi

ensure_packages
ensure_docker
mkdir -p "$deploy_dir"
ensure_env
[[ "$data_mode" == keep ]] || assert_seed_enabled

if [[ "$data_mode" == reset-only ]]; then
    use_compose "$deploy_dir/app" "$deploy_dir/tooling"
else
    stage_release
    use_compose "$deploy_dir/app.new" "$incoming/tooling"
    # O build vem antes de qualquer troca: se falhar, a release e os dados atuais ficam intactos.
    # Builds em sequência: Maven e npm juntos estouram os 3 GB do CT.
    echo "==> Build do backend ($revision)"
    "${compose[@]}" build chamados-backend
    echo "==> Build do frontend"
    "${compose[@]}" build chamados-frontend
    [[ "$data_mode" != keep ]] || dump_database
    install_release
    use_compose "$deploy_dir/app" "$deploy_dir/tooling"
fi

[[ "$data_mode" == keep ]] || wipe_data

echo "==> Subindo containers"
"${compose[@]}" up -d --remove-orphans

wait_for backend http://127.0.0.1:8181/api/auth/csrf
wait_for frontend http://127.0.0.1/
wait_for_seed

docker image prune -f >/dev/null
docker builder prune -f --filter until=168h >/dev/null
"${compose[@]}" ps --format "table {{.Service}}\t{{.Status}}\t{{.Ports}}"
