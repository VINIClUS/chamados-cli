# CI/CD do sistema

O GitLab é a autoridade de execução. A CLI [chamadosctl](chamadosctl.md) solicita
operações pela API; o runner executa os scripts Bash. Deploys de backend/web são
sob demanda e sempre preservam dados. O [Android](ci-e-release-android.md)
mantém seu fluxo separado.

## Eventos

| Evento | Comportamento |
|---|---|
| Merge request | Testes das áreas alteradas, incluindo a CLI quando aplicável; nenhum deploy. |
| Push em main | Testes do sistema; publicação Android conforme suas regras; nenhum deploy do sistema. |
| Tag vX.Y.Z | Validação da versão; o operador solicita o deploy pela CLI. |
| Tag cli-vX.Y.Z protegida | Testes, seis binários da CLI, checksums e GitLab Release; nenhum deploy do sistema. |
| Tag android-vX.Y.Z | Release Android; nenhum deploy do sistema. |
| API de operações em main protegida | Operação explicitamente indicada nos inputs. |
| Agendamento diário | Limpeza de pacotes Android e backups vencidos; não cria backups nem deploys. |

## Contrato de operações

A pipeline recebe inputs tipados: operation, environment, ref, service, tail,
backup_id e confirm. O padrão operation=none mantém o CI normal. Os executores
validam novamente os valores antes de tocar no ambiente.

Para deploy, a referência publicada é resolvida uma vez para SHA completo. Os
testes do backend e da web executam esse código em runners comuns, sem chaves de
deploy. O runner protegido recebe o código testado separado do executor Go e dos overlays Compose confiáveis
vindos de main. Produção aceita apenas tag protegida vX.Y.Z ancestral de main.

O código testado é só a aplicação da referência. Ficam de fora ``, `.gitlab*`, `android/`, `scripts/`
e as pastas operacionais de `infrastructure/`. A exceção é `scripts/export-openapi.sh`, cuja presença os testes
do backend conferem: nenhum job nem o servidor o executa, e as imagens não o copiam.

Os testes do backend rodam com os overlays Compose de main, os mesmos que o runner protegido envia. O `ops:resolve`
os publica como artefato em `.ops-target/environments`, e o `ops:backend` os põe na árvore testada. Assim, um
overlay que ligue a especificação OpenAPI ou mude o perfil do backend reprova o deploy antes do `ops:run`. A
conferência é feita pelos testes da referência: uma referência anterior a eles, como a `v0.1.0`, não confere
os overlays.

Parte dos testes do backend sobe a aplicação contra um Postgres descartável. O `backend:verify` e o `ops:backend`
o recebem como serviço do job (`postgres:16-alpine`, alias `test-db`), só com o seed de teste: nenhum dado de
staging ou produção passa por ele, e ele some com o job. Sem o serviço, esses testes falham, e o deploy para no
`ops:backend`.

O job final ops:run executa a operação solicitada e publica operation-result.json.
A CLI acompanha a pipeline e obtém esse resultado. Jobs de consulta não criam
histórico falso de deployment; deploy/restore registram o SHA efetivamente instalado
pela API de deployments. Os ambientes continuam sistema/staging e sistema/production,
sem acesso às variáveis de assinatura Android.

Cada ambiente compartilha resource_group deploy-staging ou deploy-production.
O bloqueio remoto /opt/chamados/.deploy.lock serializa também a execução do executor Go no destino.
As operações mutáveis não são interrompidas por um novo push ou pelo fechamento da CLI.

O job `ops:executor`, na etapa `prepare`, compila Linux amd64 sem chaves de deploy.
Resolve, execução e cleanups recebem seu binário, manifesto e checksums por `needs`.
O runner protegido não compila Go. Deploy/restore reservam um deployment `running`,
guardam o ID e atualizam esse registro após a execução; um POST incerto aborta
antes de mudar o ambiente. O resultado diferencia alteração aplicada de registro pendente.

`cli:quality` publica lint, formatação e complexidade como avisos (`allow_failure`).
Os novos testes SMT e de integração são locais e não têm jobs no CI. Os scripts
legados ficam como referência de regressão até a validação de paridade na sessão
local de integração; os jobs operacionais usam o executor Go.

## Dados, backup e retorno de versão

Deploy mantém os dados. Reset é uma operação explícita, com confirmação, exclusiva de
staging. A verificação de saúde não depende da senha inicial do administrador de
produção. Smoke autenticado fica restrito a staging.

Todo deploy que mantém dados conserva os cinco dumps SQL anteriores no próprio CT.
Esses dumps não incluem os anexos e não substituem um backup completo. A operação
backup captura banco e anexos com backend pausado, publica um pacote completo no
registry privado e pode ser baixada sem acesso à LAN. A limpeza mantém 30 dias e o
último backup válido.

Para voltar somente o código, solicite o deploy de uma tag anterior. O Liquibase
não reverte migrations automaticamente. Para voltar os dados, use restore: revisão
compatível, mesmo ambiente e PostgreSQL compatível, checksums validados e backup de
segurança publicado antes da substituição. O backend fica pausado durante a janela
de restauração; falhas preservam esse estado e identificam o backup de segurança.

## Runner

Os runners do GitLab.com não alcançam a rede interna, por isso os jobs de deploy rodam no CT 182 `runner-chamados` (`10.10.30.182`) do `pve-01`. Os testes continuam nos runners do GitLab.com.

- **Só orquestra:** o build acontece no CT do ambiente, então o CT 182 não tem Docker e tem só 1 core e 512 MB.
- **O que ele aceita:** só refs protegidas e só jobs com a tag `chamados-deploy`. Branches e tags não protegidas nunca chegam a ele.
- **Chaves SSH:** uma por ambiente, geradas no CT e que nunca saem de lá. Ficam em `/home/gitlab-runner/.ssh`, com os nomes padrão do `deploy.sh`, então nenhum segredo de deploy fica no GitLab. Cada CT de destino só aceita a chave vinda de `10.10.30.182`. As host keys ficam fixadas no `known_hosts`.
- **Quem alcança a produção:** qualquer job em `main` ou em tag protegida pode ler as duas chaves. Isso se limita aos Maintainers, que já são root nos CTs, porque o `remote-deploy.sh` vem do repositório e roda como root.
- **Ambientes:** os jobs usam `sistema/staging` e `sistema/production`, não `staging` e `production`. As variáveis `ANDROID_KEYSTORE_*` têm esses dois escopos, e a chave de assinatura chegaria aos jobs de deploy.

Para recriar ou completar a configuração, rode na máquina que alcança o `pve-01` e os CTs, com um `GITLAB_TOKEN` de Owner. O script é idempotente:

```bash
infrastructure/scripts/setup-runner.sh
```

Ele protege as tags, cria o CT, instala o runner, gera e autoriza as chaves, registra o runner e ajusta as filas de deploy. As filas só existem depois do primeiro deploy, então rode o script de novo depois dele. Se o token não puder criar runners, o script explica como usar `RUNNER_TOKEN_FILE`.

**Rede:** o runner fica na VNet `vnci` (`10.10.30.0/24`, para código de CI) e os CTs 180 e 181 na `vnsvc` (`10.10.10.0/24`), as duas atrás do roteador `rtr-01` (`192.168.1.249`). A `vnci` só sai para a internet. As exceções ficam no `infra-ansible` (`network_router_forward_rules`), não neste repositório: o SSH do runner para os CTs 180 e 181 e o HTTPS para a borda. Entre as VNets não há NAT, então os CTs veem a origem `10.10.30.182`, que é o `from=` das chaves. A LAN, os nodes e a VNet de operações ficam fora do alcance do runner. O MTU da VNet é 1450, e o `setup-runner.sh` grava o `mtu` no `eth0` do CT: sem isso, o CT volta para 1500 a cada boot e frames cheios se perdem.

O runner também não alcança a URL pública, porque não há hairpin NAT. Por isso os jobs definem `DEPLOY_EDGE_IP: 192.168.1.139`, o IP do CT 110 na LAN. Assim, os checks testam o certificado e a rota da borda de verdade. O `rtr-01` mascara esse acesso, então o nginx responde pela LAN e o fluxo fica simétrico. O IP do nginx na `vnsvc` (`10.10.10.139`) não serve: a resposta para a `vnci` sairia pela outra perna. Esse CT usa DHCP: se o IP mudar, atualize a variável em `.gitlab/` (incluído pelo `.gitlab-ci.yml`) e a regra do `rtr-01`.

**DNS:** o DNS do roteador da LAN (`192.168.1.1`) deixa de responder por alguns segundos de vez em quando. O CT 182 e os CTs 180 e 181 usam o DNS do `rtr-01` (`10.10.30.1` e `10.10.10.1`), que encaminha para esse roteador e para `1.1.1.1`, e os jobs tentam o fetch 3 vezes (`GET_SOURCES_ATTEMPTS`). Se o build falhar ao baixar dependências, a versão atual continua no ar e basta dar retry no job.


## Diagnóstico

Use chamadosctl doctor para conferir autenticação/projeto/runner e os comandos
status, health e logs para consultar o ambiente. Uma pipeline pending pode indicar
runner offline; uma falha SSH pode indicar chave não autorizada ou host recriado.
O setup-runner.sh reconcilia chaves e fingerprints a partir de acesso autenticado.
A configuração DEPLOY_EDGE_IP é necessária no runner por não haver hairpin NAT.

A implementação e os testes locais de backup usam ambientes descartáveis. Nunca
execute reset ou restore em produção para validar uma mudança de código.
