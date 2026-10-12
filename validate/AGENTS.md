# AGENTS.md — `validate/`

The node-image validator: an offline CLI that checks a built NVKE node image
against its release evidence and a versioned check registry, plus the
verification library the management plane uses to check the resulting verdict.

**Module path:** `github.com/dsx-ai-factory/nvke/validate`

## What this is

Operators build node images with the NKD pipeline and run
`nvke-image-validate` against the result before registering it. The tool is a
batch CLI, not a controller and not a webhook: validation happens once, offline,
in the build pipeline; the management plane only ever re-checks the signed
verdict (milliseconds), never the image.

Three inspection tiers, cheapest first; every check declares its tier in
`checks.yaml`:

- **static** — no image open: digests, release-checksums membership, identity.
- **guestfs** — read-only filesystem facts via libguestfs (planned; follows
  `os-image/packer/scripts/extract-image-sbom.sh`'s conventions).
- **boot** — behavioral assertions from inside a throwaway QEMU boot (planned).

## Layout

```text
checks.yaml                  # the versioned check registry: what + why, never how
cmd/nvke-image-validate/     # CLI entrypoint; exit 0 pass / 1 fail / 2 error
pkg/checks/                  # registry loader + engine + implementations per tier
pkg/report/                  # report.json schema v1, human renderer, exit policy
```

## Constraints

- **Offline is a contract.** No package in this module may open a network
  connection; air-gapped operator pipelines are the primary environment. Tests
  must run hermetically from generated fixtures.
- **Exit codes are API**: 0 pass, 1 blocking-check fail, 2 could-not-evaluate
  (usage errors included). Error always outranks fail — "could not evaluate"
  must never read as "evaluated, non-compliant".
- **checks.yaml entries say what and why, never how.** Implementations are
  bound by ID in the engine; an entry with no implementation reports an error,
  not a silent pass. Every entry needs `verifies` and `why`; the registry
  version travels into every report.
- The `--checksums` input is the release's `checksums.txt`. Its authenticity
  (the sigstore attestation over it) is verified by the bundle layer when that
  lands; until then the flag trusts its caller, and says so in its help text.

## Commands

```bash
make build      # stamped bin/nvke-image-validate
make compile    # type-check every package, no binary
make test       # unit tests (hermetic, no network, no KVM)
make lint       # golangci-lint at the repository pin
make license    # SPDX headers for new files
```

## Verification — run before submitting a pull request

```bash
make fmt
make vet
make lint
make test
```

From the repository root after adding/moving test files or packages:

```bash
make coverage-check
```
