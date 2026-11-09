# CI/CD da CLI

O GitHub Actions testa a CLI em pushes e pull requests, usando apenas o snapshot
versionado em `contracts/backend/`. O CI comum não recebe credenciais do GitLab.

Os checks validam o lock, checksums, estrutura e referências do OpenAPI; executam
os testes Python e Go com detector de corridas, `go vet`, os testes dos payloads
contra os schemas operacionais e os seis builds. O verificador descartável em
`local/executor-verification/` exercita falhas e aplicações históricas sem operar
sobre staging ou produção.

Tags `cli-vX.Y.Z` executam os checks antes de publicar um GitHub Release com
binários Linux, macOS e Windows em amd64 e arm64, acompanhados de `SHA256SUMS`.
A publicação não sobrescreve assets existentes.

## Contratos do Backend

O Backend publica um pacote privado `backend-contracts/<SHA completo>` contendo
OpenAPI, respostas sintéticas, quatro schemas operacionais e manifesto. O lock
fixa projeto, SHA, URL e checksums. A atualização é explícita e validada antes de
substituir o snapshot; veja [contratos](../contracts/README.md).

## Operações no GitLab

Os comandos públicos da CLI solicitam operações ao projeto Backend no GitLab.
O executor é compilado em um runner comum a partir do commit público completo
fixado pelo Backend. Binário, manifesto e checksums seguem como artifacts para
o runner protegido, que fornece seu próprio checkout e `scripts/deploy/`.

`executor_sha` identifica a CLI, `backend_sha` identifica esse checkout protegido
e `revision` identifica a aplicação. O executor valida essas identidades antes
de executar a operação. A aplicação solicitada nunca fornece os Compose ou o
bootstrap protegido. O layout histórico da aplicação continua compatível com
a validação descartável.

Uma operação explícita executa somente seus próprios jobs. O fluxo automático
do sistema continua no Backend: merges em `main` publicam staging e tags
`vX.Y.Z` publicam produção. Tags da CLI não executam deploy do sistema.
