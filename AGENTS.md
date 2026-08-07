# AGENTS.md

This file tells AI coding agents (Claude Code, Codex, Copilot Workspace,
Cursor, etc.) how to work effectively in this repository.

## Repository layout

nke is a Go monorepo with two independently versioned modules:

| Directory | Module path | Description |
|---|---|---|
| `controller/` | `github.com/NVIDIA/nke/controller` | Cloud Workflow Engine — cluster lifecycle orchestration via Temporal |
| `node-agent/` | `github.com/NVIDIA/nke/node-agent` | Per-node agent — kubelet bootstrap, SPIFFE attestation |

Each module has its own `go.mod`, `Makefile`, and module-level `AGENTS.md`
with deeper guidance. **Read those files before editing code in that module.**

- `controller/AGENTS.md` — CWE-specific setup, code generation rules, lint config
- `node-agent/` — see `node-agent/go.mod` and `node-agent/Makefile` for entry points

## Common commands

### controller

```bash
cd controller
flox activate           # activate reproducible toolchain (required for make generate)
make build              # build binary
make test               # unit tests with coverage
make lint               # golangci-lint
make fmt                # format (gofmt + goimports)
flox activate -- make generate  # regenerate Go from OpenAPI spec
```

### node-agent

```bash
cd node-agent
go build ./cmd/...
go test ./...
go vet ./...
```

## Code conventions

- **Conventional Commits** for all commit messages (`feat:`, `fix:`, `docs:`, `ci:`, etc.)
- **Branch naming**: `<type>/<short-description>` (e.g., `fix/null-pointer-in-parser`)
- **License headers**: all new Go files need NVIDIA license header — run `make license` in `controller/`
- **No TODO comments** in controller/ code — they fail the linter. Use `FIXME: <jira-url>` instead.
- **Generated files**: never edit `*.gen.go` manually — run `make generate`

## Pull request checklist

Before opening a PR:

- [ ] `make lint` passes in the module you changed
- [ ] `make test` (or `go test ./...`) passes
- [ ] No new `TODO:` comments in controller/ code
- [ ] License headers present on new source files (`make license`)
- [ ] `go mod tidy` run and `go.mod`/`go.sum` committed if dependencies changed
- [ ] If OpenAPI spec changed: `flox activate -- make generate` run and `*.gen.go` committed

## Security

Do not hardcode credentials, API keys, or tokens. See [SECURITY.md](SECURITY.md)
for the vulnerability disclosure process.

## Further reading

- [docs/architecture.md](docs/architecture.md) — system overview
- [docs/getting-started.md](docs/getting-started.md) — local dev setup
- [CONTRIBUTING.md](CONTRIBUTING.md) — contribution workflow and PR process
