# Getting started

This guide walks you through setting up a local development environment
for nke and verifying that both modules build and test cleanly.

## Prerequisites

| Tool | Version | Install |
|---|---|---|
| Go | 1.24+ | https://go.dev/dl/ |
| Docker | 24+ | https://docs.docker.com/get-docker/ |
| Flox (optional) | latest | https://flox.dev/ — used for reproducible controller toolchain |

## Clone the repository

```bash
git clone https://github.com/NVIDIA/nke.git
cd nke
```

## controller (Cloud Workflow Engine)

The controller uses [Flox](https://flox.dev/) to pin its toolchain. For
day-to-day work, activate the Flox environment first:

```bash
cd controller
flox activate
```

### Build

```bash
make build          # → ./bin/cloud-workflow-engine
```

### Lint

```bash
make lint           # golangci-lint (5 min timeout)
make fmt            # gofmt + goimports
```

### Test

```bash
make test           # unit tests with coverage
```

### Code generation

If you change `api/openapi.yaml`, regenerate Go bindings:

```bash
flox activate -- make generate
```

Never hand-edit `*.gen.go` files — they will be overwritten.

### Run locally

Start dependencies:

```bash
# PostgreSQL
docker run -d --name=nke-postgres -p 5432:5432 -e POSTGRES_PASSWORD=postgres postgres:17.4

# Temporal (dev mode)
temporal server start-dev
```

Then run CWE:

```bash
FWE_API_KEY=<your-NGC-api-key> make run
# UI:      http://localhost:8080
# Swagger: http://localhost:8080/api/docs
```

## node-agent

```bash
cd node-agent
go build ./cmd/...
go test ./...
```

The node-agent is typically deployed via the site controller and not run
directly on a laptop. For integration testing, see the internal test
harness in `node-agent/integration-test/` (requires a running site
controller and Forge environment).

## Next steps

- Read `docs/architecture.md` for a system overview
- Read `controller/AGENTS.md` for AI-assisted development tips
- See `CONTRIBUTING.md` for the PR workflow and code standards
