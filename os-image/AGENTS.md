# AGENTS.md — `os-image/`

Reference node OS image (spec + Packer recipe) and the `nke-os-image-check`
connectivity binary.

**Module path:** `github.com/NVIDIA/nke/os-image`

The directory is `os-image/` on purpose. `image.yml` / `image-nvcr.yml` already
publish **container** images; this module is the Ubuntu qcow2 a node boots.

## What to read first

- [SPEC.md](SPEC.md) — bake vs cloud-init, ports, SPIFFE rules
- [README.md](README.md) — how to build the qcow2 and run the checker

## Layout

```text
SPEC.md                   # contract
packer/                   # QEMU golden-image build
files/                    # units + SPIRE template + first-boot script
packages/kubeadm-distro/  # builds the staged nke-kubeadm-distro-<x.y.z> debs
cmd/nke-os-image-check/   # checker entrypoint
internal/check/           # dial + gRPC reflection / GetKubeadmStatus
```

## Versioning

OS-image releases use component-scoped git tags, same shape as `agent/v*`:

```bash
git tag -s os-image/v0.1.0 -m "os-image v0.1.0"
git push origin os-image/v0.1.0
```

Pushing the tag runs `.github/workflows/os-image-artifact.yml`: Packer bakes the
qcow2, `make boot-smoke` must pass, then the disk is published to
`ghcr.io/nvidia/nke/os-image` and the GitHub Release is created. A failed bake
or smoke test does not publish.

`make` stamps `nke-os-image-check` from:

```bash
git describe --match 'os-image/v[0-9]*' --tags
```

The `os-image/` prefix is stripped for the binary ldflag.

## Commands

```bash
make build              # stamped bin/nke-os-image-check, and asserts the stamp took
make compile            # type-check every package, no binary
make test
make lint
```

From the repository root, `make build` links the same binary into the
repository's `bin/`, and `make version-check` runs it with `--version`.

## Constraints

- Do not bake SPIRE server address, trust domain, trust bundle, or join token.
- Do not default `nke-agent` to `--disable-mtls`.
- Pin SPIRE (version + sha256) in `packer/scripts/fetch-artifacts.sh`; do not
  float `latest` for SPIRE.
- Do not hard-require `/dev/kvm`. `packer/Makefile` and `boot-smoke.sh` probe
  for it and fall back to `tcg`, because the CI runners are containers without
  it (https://github.com/NVIDIA/nke/issues/219).
- `nke-agent` is built from this checkout's `agent/` source at bake time
  (`make -C agent build-amd64` in `fetch-artifacts.sh`); no prebuilt artifact
  is pulled. Its version is the checkout's `git describe --match 'agent/v*'`.
- Checker is a SPIFFE **client**. Do not embed a SPIRE server in the binary.
- New files need the Apache-2.0 SPDX header (`make license`). Cloud-init
  user-data cannot: `#cloud-config` must be line 1, so examples live in
  README/SPEC rather than `*.yaml`.
- `packages/kubeadm-distro/` builds the staged `nke-kubeadm-distro-<x.y.z>`
  debs with **nix**, on an x86_64 Linux host that is *not* the bake host.
  Keep it that way: a `.deb` is a plain artifact, so the nix host builds and
  the bake host consumes. `fetch-artifacts.sh` takes prebuilt debs through
  `KUBEADM_DISTRO_DEBS_DIR` and only falls back to building them in place, as
  a developer convenience, when it is unset. Nothing nix-shaped may reach the
  image or the node.
- Nothing in `packages/kubeadm-distro/` is built from source: every binary is
  an upstream release artifact, pinned by sha256 in
  `packages/kubeadm-distro/versions.json`. That file is the committed lock,
  regenerated only by `make lock` and never hand-edited (`nix flake check`
  fails on non-canonical formatting). `K8S_VERSIONS` in
  `packages/kubeadm-distro/Makefile` selects which of its rows to build; docs
  may quote the current values. Nothing is installed at bake time: the debs
  land under `/usr/lib/nke/debs/` and stay uninstalled until a cluster create
  picks one. Its local, unsigned `file:` apt source is on purpose; see
  `provision.sh` for why.
