# MR !22: verificação exclusivamente local

Esta suíte não participa do CI. O módulo Go aninhado é excluído de `go test ./...`;
os programas Python não usam nomes descobertos por pytest e ficam fora de `tests/`.
Z3/Paramiko são dependências de desenvolvimento, instaladas somente em `.venv/`.
Os executáveis usados são construídos a partir do checkout atual. Nenhum servidor real é acessado.
O registro da última validação está em [RESULTS.md](RESULTS.md).

Execute da raiz:

```sh
python3 local/executor-verification/run.py setup
python3 local/executor-verification/run.py smt
python3 local/executor-verification/run.py integration
python3 local/executor-verification/run.py unit
python3 local/executor-verification/run.py contracts
python3 local/executor-verification/run.py checks
python3 local/executor-verification/run.py python-checks
```

Todos os comandos compartilham um lock, esperam `MemAvailable >= 3 GiB` e `load1 <= 4`
em intervalos de 30 s e executam em scope systemd de uma CPU/1536 MiB, MemoryHigh=1 GiB,
nice 19, ionice classe 3, GOMAXPROCS=1, GOFLAGS=-p=1, GOMEMLIMIT=512MiB e
COMPOSE_PARALLEL_LIMIT=1. Os testes Go usam -parallel=1. O host precisa de Docker Compose,
systemd de usuário e da imagem local `postgres:16-alpine`; não há pull implícito.

`reports/smt.json` contém os casos reproduzíveis (seed 22), expectativas e resultados,
efeitos, fase, journal e caminhos inviáveis. O Solver primeiro gera testemunhas
com entradas livres para objetivos de fase; elas são executadas contra Go junto
das regressões de fronteira. Os caminhos inviáveis usam esse mesmo sistema de
transições. Fases persistidas são observadas nos limites de I/O e comparadas com
uma sequência esperada independente; o estado terminal também é comparado com o oráculo.
O modelo independente cobre ambiente,
confirmação, serviço, referência protegida/ancestralidade, integridade, backup de
segurança e falhas em cada transição. Cada caso chama `Host.Execute` do executor Go real;
somente as dependências são controladas. Máximo 256 casos, timeout de 2000 ms por consulta
e orçamento de 60 s incluindo a execução dos casos. Timeout/unknown geram inconclusivo,
com exit 2; divergências geram exit 1. Casos não percorridos por orçamento também são inconclusivos.
A enumeração é de classes de equivalência e obrigações de cobertura, não uma prova exaustiva.

`reports/integration.json` registra SQL/anexos reais, manifestos interno/externo,
checksums, `.env`, reinício seletivo, corrupção, publicação parcial, locks, replay,
falhas após substituição/ativação, timeout dentro do daemon, retenção e deployment.
O servidor SSH autentica uma chave efêmera e o cliente OpenSSH verifica a host key;
o namespace `/opt/chamados` é traduzido para o diretório temporário, inclusive inspect.
O runner e o comando remoto usam o binário CLI real. HTTP GitLab serve apenas loopback.
Um repositório Git temporário prova que OPS_REF seleciona só a aplicação e não fornece
executor, scripts, overlays, `.env` ou configuração do pipeline.

Contêineres: PostgreSQL 384 MiB, backend/frontend leves 48 MiB cada, helper 64 MiB;
máximo simultâneo 544 MiB. Cada serviço tem quota de CPU e cada helper também.
Projetos, portas, chaves e diretórios são exclusivos. A limpeza usa apenas o projeto
e labels desta execução; nunca `prune`. Falhas de limpeza aparecem como falhas da suíte.
Os IDs de backup informados são evidências das fixtures descartáveis, não backups de produção.

Os testes de timeout injetam `sleep` dentro de `docker exec` e do helper, esperam além
do atraso e verificam que não houve substituição tardia. Cancelamento após início da
mutação para também PostgreSQL, além de manter o backend parado. O helper mutável tem
nome e label da operação e é removido por uma conexão Docker nova após falha.
O resultado mantém `data.safety_backup_id`; a recuperação exige inspeção e ação explícita.
Se o daemon Docker estiver inacessível, a limpeza pode falhar: o erro é preservado e
não representa garantia de cancelamento de um daemon inacessível.

Referências primárias consultadas (Context7 respondeu `Monthly quota exceeded` nesta sessão):

- [Z3 Python, implementação oficial de Solver/reason_unknown](https://github.com/Z3Prover/z3/blob/master/src/api/python/z3/z3.py)
- [Docker exec: ciclo de vida do processo no contêiner](https://docs.docker.com/reference/cli/docker/container/exec/)
- [PostgreSQL 16 pg_restore](https://www.postgresql.org/docs/16/app-pgrestore.html)
- [Paramiko ServerInterface, fonte oficial](https://github.com/paramiko/paramiko/blob/main/paramiko/server.py)

As referências Bash/Python legadas permanecem até migração completa dos seus testes
de contrato e comprovação de paridade. Esta suíte não declara o lint limpo; os avisos
informativos conhecidos da sessão 1 continuam registrados em `.quality/`.

`python-checks` executa Ruff (lint e verificação de formato) e ty sobre todo o Python
do repositório, usando o ambiente virtual da suíte. `python-fmt` aplica os ajustes de
Ruff e executa ty. As versões estão fixadas em `requirements.txt`. A configuração
geral está em `pyproject.toml`; esta suíte tem sua própria configuração
local. As referências legadas mantêm sintaxe compatível com Python 3.10.
Essas verificações também não são adicionadas ao CI.
