# Verificação local — 2026-11-07

Resultados no layout final com `devops/`, `infrastructure/` e workflows em `.gitlab/`:

| Verificação | Resultado |
| --- | --- |
| Go, vet e race detector | 4 pacotes passaram |
| Contratos Python e ShellCheck | 51 testes e 5 subtestes passaram; ShellCheck passou |
| Ruff 0.17.4 e ty 0.0.99 | lint, formato e tipos passaram nos 24 arquivos Python |
| SMT, seed 22 | 119 casos, incluindo 12 gerados pelo solver; 3 obrigações unsat; nenhuma divergência ou inconclusão |
| Integração local | 10 grupos passaram; recursos próprios removidos |
| Compose | base, staging e produção validados sem subir a aplicação |
| Contrato estático de CI | inputs, variáveis, workflow, stages, IDs e propriedades críticas preservados |
| Qualidade Go | formatação passou; 367 achados informativos de lint permanecem |

Os testes de integração exercitam o executor e a CLI reais com PostgreSQL 16,
serviços mínimos, HTTP GitLab e SSH autenticado em loopback. Incluem corrupção,
backup de segurança, falhas de transporte/registro, retenção e cancelamento de
processos no daemon sem substituição tardia dos dados. Nenhum servidor real foi acessado.

As execuções pesadas foram seriais, iniciadas com `load1 <= 4` e pelo menos 3 GiB
disponíveis, em scope de uma CPU/1536 MiB, nice 19 e ionice 3. Os contêineres têm
limites próprios, com máximo simultâneo de 544 MiB.

Os relatórios detalhados em `reports/` são gerados localmente e ignorados pelo Git.
Para repetir, use os comandos do [README](README.md). SMT e integração continuam
exclusivamente locais; o modelo cobre classes limitadas, sem afirmar prova exaustiva.
