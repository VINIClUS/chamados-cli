# chamados-cli

Standalone `chamadosctl` client and Linux executor for Sistema de Chamados.
The public module is `github.com/VINIClUS/chamados-cli`; it has no dependency
on a backend checkout at build time.

Public commands use the GitLab API and do not need Go, Docker, SSH, or LAN
access on the operator machine. Protected CI jobs use the same binary as the
executor and provide the backend checkout and deployment tooling separately.

```sh
go test -race ./...
go vet ./...
python3 scripts/cli/check-contracts.py
VERSION=0.1.0 CI_COMMIT_SHA="$(git rev-parse HEAD)" scripts/cli/build.sh
```

The build emits Linux, macOS, and Windows binaries for amd64 and arm64 plus
`SHA256SUMS`. GitHub Actions publishes immutable `cli-vX.Y.Z` releases. The
operation schemas and executor manifest are in `contracts/backend/`; the backend
OpenAPI snapshot is synchronized through `scripts/update-backend-contracts.py`.

See [CLI usage](docs/chamadosctl.md), [CI/CD](docs/ci-cd.md), and the
[disposable executor verification suite](local/executor-verification/README.md).
