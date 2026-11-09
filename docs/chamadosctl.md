# Deploy & Operations com chamadosctl

`chamadosctl` usa a API do GitLab para executar as operações no runner do projeto.
O computador do operador não precisa de Go, Git, SSH, Bash, Docker nem acesso à LAN.
O mesmo binário contém o executor Go interno usado pelo CI e pelos CTs Linux.
Os comandos públicos continuam usando somente a API do GitLab no computador do operador.

## Instalação e autenticação

Baixe o executável do seu sistema/arquitetura no Release `cli-vX.Y.Z` de
[GitHub Releases](https://github.com/VINIClUS/chamados-cli/releases). Há versões Linux, macOS e Windows, amd64 e arm64.
Compare o SHA-256 com `SHA256SUMS`. Em Linux/macOS, conceda permissão de execução;
no Windows, use o arquivo `.exe`. Renomeie para `chamadosctl` se desejar.

Cada operador fornece seu próprio token GitLab com escopo `api` e permissão para
iniciar pipelines na `main` protegida. Forneça `GITLAB_TOKEN` no ambiente ou use
`--token-file /caminho/arquivo`. O token não vai para a configuração, o executável,
os inputs da pipeline nem o Google Drive. Mantenha o arquivo de token privado.

```text
chamadosctl config show
chamadosctl doctor
chamadosctl version
```

A configuração fica em `chamadosctl/config.json` dentro do diretório de
configuração do usuário do sistema operacional. Exemplo sem credenciais:

```json
{
  "gitlab_url": "https://gitlab.com",
  "project": "portosoftware/SistemaDeChamados",
  "operation_ref": "main"
}
```

## Operações

```text
chamadosctl deploy staging --ref main
chamadosctl deploy staging --ref minha-branch
chamadosctl deploy production --ref v1.2.3
chamadosctl status production
chamadosctl health production
chamadosctl logs production --service chamados-backend --tail 200
chamadosctl restart staging
chamadosctl smoke staging
chamadosctl reset staging --confirm staging
```

Os deploys preservam os dados. Produção aceita somente tags protegidas `vX.Y.Z`
em commits de `main`. `reset` só existe em staging; `smoke` autenticado também,
pois a senha inicial do administrador pode ter sido alterada em produção.
`restart` sem serviço reinicia backend e frontend; o banco exige seleção explícita.
Os logs são uma captura finita: não deixam um job ocupando o runner indefinidamente.

Todas as operações aguardam conclusão. `--detach` retorna a identificação da
pipeline para acompanhar depois. Interromper a CLI desliga apenas o acompanhamento;
a operação continua no GitLab. Use os comandos `pipeline` para consultar jobs e logs.
`--json` mantém resultados estruturados na saída padrão e progresso na saída de erro.

O GitLab registra o SHA real da aplicação, separado da revisão do executor.
O resultado do job fica em `operation-result.json`. O schema 1 inclui campos opcionais
`operation_id`, `phase`, `applied` e `error` (código, fase, código de saída e indicação
de que somente o registro pode ser repetido). `applied=true` pode acompanhar uma
falha posterior no registro do deployment; isso não solicita outro deploy/restore. Para voltar apenas o código,
solicite deploy da tag anterior; migrations não são revertidas por esse comando.

## Backups e restauração

```text
chamadosctl backup create production
chamadosctl backup list production
chamadosctl backup download production --backup ID --output backup.tar.gz
chamadosctl restore staging --backup ID --confirm staging
```

O backup contém PostgreSQL em formato custom e os anexos, com manifesto e
checksums. O backend é pausado durante a captura e retomado ao final. O `.env`
permanece no servidor e não entra no pacote. Os arquivos ficam no Package Registry
privado, em `chamados-backup-staging` ou `chamados-backup-production`. O manifesto
é publicado por último; somente pacotes completos devem ser usados.

A limpeza diária conserva 30 dias e o backup válido mais recente. Não há criação
automática de backups: o operador solicita a captura. Os cinco dumps SQL de
pré-deploy já existentes continuam separados e não incluem anexos.

A restauração aceita apenas o mesmo ambiente e uma versão compatível do PostgreSQL.
Ela valida os arquivos e prepara a revisão original antes de substituir dados.
Produção exige uma tag protegida para o SHA do snapshot. Um snapshot legado sem
essa associação é recusado; não há exceção que permita contornar a política de produção.

Antes de restaurar, o executor publica um backup de segurança. O backend permanece
pausado desde essa captura até o término da restauração. Se houver falha depois da
substituição, ele permanece parado e o resultado informa o ID do backup de segurança.
Consulte o job antes de escolher qual snapshot restaurar. A configuração `.env` do
destino é preservada e a aplicação volta na revisão associada ao backup.

## CI/CD e publicação da CLI

A pipeline de operações parte de `main` protegida, recebe inputs validados e
reutiliza o runner `chamados-deploy`. O código solicitado é resolvido para um SHA
imutável e testado antes do deploy; o executor e os overlays Compose vêm da referência confiável.
Os testes do backend usam esses mesmos overlays, e não os da referência solicitada.
Os ambientes são `sistema/staging` e `sistema/production`, separados dos escopos
de assinatura do Android. Cada ambiente tem uma fila e o bloqueio remoto
`/opt/chamados/.deploy.lock` para serializar operações.

Merges em `main` do Backend continuam publicando staging automaticamente; tags
`vX.Y.Z` publicam produção. Uma operação explícita usa somente seus próprios
jobs. Tags `cli-vX.Y.Z` publicam a CLI no GitHub; o Android tem CI e tags
`android-vX.Y.Z` em [chamados-mobile](https://github.com/VINIClUS/chamados-mobile).

Para compilar localmente (somente para desenvolvimento):

```bash
go test -race ./...
go vet ./...
VERSION=0.1.0 bash scripts/cli/build.sh
```

O build produz seis executáveis e `SHA256SUMS` em `.cli-dist/`. Os binários usam
`CGO_ENABLED=0` e versão/commit/data fixados no build. Somente os comandos internos
do executor Linux invocam ferramentas externas: Docker Compose, Git e ferramentas
PostgreSQL. Os comandos públicos não exigem essas dependências locais.
A pipeline publica arquivos imutáveis no pacote `chamadosctl` e vincula os assets
ao Release. Um retry reutiliza arquivos idênticos e recusa substituir bytes publicados.

## Executor interno e diagnóstico

O job `ops:executor` compila somente Linux amd64, sem chaves de deploy. Entrega
binário, SHA-256 e manifesto com protocolo, arquitetura e revisão do executor.
Os jobs `ops:resolve`, `ops:run` e `ops:cleanup:*` recebem esse artefato por `needs`.
O runner protegido verifica o checksum e envia o mesmo binário e os overlays
confiáveis ao CT. Uma inspeção remota verifica sua identidade antes de transmitir
o envelope com `JOB-TOKEN` por stdin. O runner e os CTs não precisam instalar Go.

A coordenação tem fronteiras separadas: o runner valida o artefato e monta as
dependências; o workflow usa contratos pequenos para GitLab e execução remota;
o transporte SSH transfere, verifica e reconcilia resultados. No destino, o host
valida e mantém o lock, o journal decide sobre repetição, e cada operação recebe
seu estado tipado. O restore explicita preparo, backup de segurança, substituição
e ativação. Builds usam um alvo Compose imutável, separado do alvo da aplicação
atual usado para guardar o banco antes do deploy.

Os comandos `internal resolve`, `internal execute` e `internal inspect` ficam
ocultos na ajuda pública. Não carregam a configuração nem o token do operador.
O executor usa um prazo de 25 minutos; recuperação/limpeza tem até 30 segundos.
O lock `/opt/chamados/.deploy.lock` cobre a execução inteira. O journal atômico
`/opt/chamados/operations/<pipeline-id>-<job-id>.json` guarda fase e resultado,
sem envelope ou credenciais. `internal inspect ID` consulta esse journal sem
executar novamente a operação. IDs já concluídos não repetem efeitos; IDs
inacabados exigem inspeção. Perda de SSH gera `state_unknown` quando o resultado
não pode ser recuperado. Nunca repita um restore automaticamente nesse estado.

Deploy/restore reservam um deployment `running` antes da alteração. Se o POST
ficar incerto, a operação é abortada antes de alterar os dados, sem repetir o
POST. O ID confirmado é atualizado ao final; uma falha nessa atualização mantém
o ID e `applied` no resultado, para distinguir execução de registro.

### Qualidade de Go

`bash scripts/cli/quality.sh check` verifica formatação e executa golangci-lint
v2.15.0; `bash scripts/cli/quality.sh fmt` aplica gofmt/goimports e organiza linhas longas com golines (120 colunas).
O script confere o checksum da distribuição antes de instalar a ferramenta local.
O conjunto inclui a análise de segurança `gosec`. A execução local de `scripts/cli/quality.sh` produz Code Climate, diff e logs mesmo com
avisos. Não é dependência de release. Testes e checks existentes continuam como
antes. Complexidade ciclomática/cognitiva avisa a partir de 15; manutenibilidade
abaixo de 80 (limiar rigoroso); tamanho acima de 200 linhas/80 instruções; duplicação usa 300 tokens.

Os testes SMT e de integração são exclusivamente locais: veja
[`local/executor-verification/README.md`](../local/executor-verification/README.md). Os comandos próprios aguardam
3 GiB disponíveis e carga de um minuto até 4, serializam a execução e aplicam
baixa prioridade, uma CPU e até 1,5 GiB. Os contêineres usam no máximo 544 MiB somados.
Não há inclusão no CI nem descoberta pelo módulo Go ou pelos testes Python da raiz.

Timeout de restore após início da mutação também para o PostgreSQL, impedindo
que um `docker exec` continue substituindo dados depois da saída do cliente.
O helper de anexos recebe identidade própria e é removido após falha. O backend
fica parado, e `data.safety_backup_id` informa o backup para recuperação explícita.
Um timeout apenas do health check mantém PostgreSQL disponível para inspeção.

## Administração do runner

O operador da CLI usa somente a API do GitLab. Chaves SSH e ferramentas de
administração permanecem com os administradores do Backend e não são distribuídas
com o cliente. A reconstrução do runner e os acessos de recuperação estão no
[runbook privado do Backend](https://gitlab.com/portosoftware/SistemaDeChamados/-/blob/main/docs/ci-cd.md).
