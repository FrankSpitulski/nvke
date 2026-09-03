<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# kubeadm-distro

Builds `nke-kubeadm-distro-<x.y.z>` Debian packages: one self-contained
Kubernetes node stack per Kubernetes patch release. The Packer build stages the
packages and their apt index inside the image; a node installs the one it needs
at cluster create, from that local repository, with no network access.

Each package carries kubelet, kubeadm, kubectl, containerd, runc, the CNI
plugins, crictl and Helm — every artifact verified against its publisher's
SHA-256 — plus the control-plane container images for that release, so a node
can come up without reaching a registry.

## What it builds

```text
<OUT_DIR>/nke-kubeadm-distro-1.36.3_1_amd64.deb
<OUT_DIR>/nke-kubeadm-distro-1.35.3_1_amd64.deb
<OUT_DIR>/nke-kubeadm-distro-1.34.2_1_amd64.deb
<OUT_DIR>/Packages                                  # flat apt index
```

The Kubernetes patch is part of the **package name**; the Debian `Version` field
is only the packaging revision (`DEB_REVISION`, default `1`). Every package
`Provides: nke-kubeadm-distro` and `Conflicts` with its peers, so exactly one
matrix row can be installed at a time.

Inside a package:

| Path | Contents |
|---|---|
| `/usr/bin/{kubeadm,kubectl,kubelet}`, `/usr/sbin/runc` | pkgs.k8s.io-style binary paths |
| `/lib/systemd/system/kubelet.service{,.d/10-kubeadm.conf}` | vendor unit plus the kubeadm drop-in |
| `/usr/lib/nke/k8s-matrix/<version>/` | `manifest.json`, the installer, and `amd64/` with every artifact and its `.sha256` sidecar |
| `/usr/bin/nke-k8s-installer` | symlink to the bundled installer |

`postinst` verifies the manifest and the installer against their checksums, runs
the installer with `NKE_BUNDLE_ROOT` pointing at the bundle, then imports the
bundled images with `ctr -n k8s.io images import`.

The bundled images are the ones kubeadm asks for by default, which means
`registry.k8s.io`. A controller that sets `KUBEADM_IMAGE_REPOSITORY` to a mirror
makes kubeadm ask for different names, and the node pulls from that mirror
instead: the bundled images are then unused, and the node is no longer offline.

The package `Depends` on `iptables, iproute2, conntrack, socat, ethtool`.
`conntrack` is a hard requirement — kubeadm's preflight check errors without it
— and kubeadm warns when `socat` or `ethtool` are missing.

## Lock files

Everything a bundle is built from is pinned by digest in `lock/<version>.json`,
committed and reviewed: the `sha256` of every upstream artifact and the
linux/amd64 manifest digest of every container image. `make debs` refuses to
build a version that has no lock, verifies each download against the locked
digest as well as against the publisher's checksum, and pulls images by
`ref@digest` rather than by tag. See [lock/README.md](lock/README.md).

## Package contract

The internal `nke-kubeadm-distro-<x.y.z>` package this repository builds is
meant to be a drop-in replacement for the one previously served from an
internal S3 apt repository. `make debs` proves that with a golden test: it
builds a normalized manifest of every `.deb` it produces — control fields,
conffiles, maintainer scripts, and a sha256-verified file listing — and
compares it against a golden manifest of the production package in
[`testdata/golden/`](testdata/golden/), allowing only the differences
recorded and reasoned about in
[`testdata/golden/deviations.txt`](testdata/golden/deviations.txt).

```bash
make -C os-image/packages/kubeadm-distro contract-check OUT_DIR=/abs/path/to/apt-repo
```

`contract-check` runs as the last step of `debs`; set `SKIP_CONTRACT_CHECK=1`
to skip it during development. It is **advisory** (a warning, not a build
failure) while `deviations.txt` is empty, and becomes a **hard failure** the
moment `deviations.txt` records its first reviewed deviation — see
[`testdata/golden/README.md`](testdata/golden/README.md) for why it starts
empty, the capture procedure for the goldens, and the deviation line format.

`cmd/debmanifest` is the tool behind this: `debmanifest <deb>` prints a
package's manifest, `debmanifest -golden <file> -deviations <file> <deb>`
checks one `.deb` against one golden file, and `debmanifest check -out-dir
<dir> -golden-dir <dir> -deviations <file>` is what `contract-check` actually
runs — it walks every `nke-kubeadm-distro-<version>_*_*.deb` in `-out-dir`,
skips (and logs) any version with no matching golden, and applies the
strict/advisory rule from `-deviations`. Diagnostics go to stderr via
`log/slog`; only the plain `debmanifest <deb>` form writes to stdout.

## Makefile interface

```bash
make -C os-image/packages/kubeadm-distro debs OUT_DIR=/abs/path/to/apt-repo
make -C os-image/packages/kubeadm-distro debs OUT_DIR=/abs/path K8S_VERSIONS="v1.36.3 v1.34.2"
make -C os-image/packages/kubeadm-distro lock
make -C os-image/packages/kubeadm-distro test
make -C os-image/packages/kubeadm-distro clean
```

`OUT_DIR` is required and must be absolute. Other knobs:

| Variable | Default | Purpose |
|---|---|---|
| `K8S_VERSIONS` | `v1.36.3 v1.35.3 v1.34.2` | Kubernetes releases to build |
| `DEB_REVISION` | `1` | Debian packaging revision |
| `LOCK_DIR` | `./lock` | The committed lock files |
| `DIST_DIR` | `./dist` | Artifact cache. A cached upstream artifact is re-checked against the publisher's checksum document every run, so this still needs the network; the `.sha256` sidecar is an output, not the thing that is trusted. Only the container image tarball is reused on the strength of its own sidecar |
| `SKIP_IMAGES` | unset | `1` skips the container image pull. Development only — the package then has no images to import |

### Build host requirements

**Linux, amd64 only.** The build runs the downloaded `kubeadm` binary to ask it
which control-plane images this release needs, which only works on Linux.
Packages are built with [nfpm](https://nfpm.goreleaser.com/), which writes
`root:root` ownership straight into the archive metadata without needing
`fakeroot` or `dpkg-deb --root-owner-group` to get there. `make bundle
SKIP_IMAGES=1` skips the part that needs a Linux kernel and so runs
anywhere — but the lock is still required, so it only works for a version that
already has a committed `lock/<version>.json`. `make debs` always needs Linux.

```bash
apt install dpkg-dev gnupg
```

`dpkg-dev` is still needed for `dpkg-scanpackages`, which writes the flat apt
index; `gpg` is needed because runc publishes its checksums as an OpenPGP
clearsigned list, verified against `packaging/keyrings/runc-release.gpg`.
Building the `.deb` itself no longer needs `dpkg-deb` — `make debs` downloads
the pinned `nfpm` (`NFPM_VERSION` in
[`../../../make/tool-versions.mk`](../../../make/tool-versions.mk)) into
`./bin` for Linux or Darwin, amd64 or arm64, verified against a pinned
SHA-256, or falls back to `nfpm` on `PATH` on any other host/arch. `dpkg-deb`
is still useful for inspecting the result by hand, and is what this package's
tests use to do so. `make debs` checks for `dpkg-scanpackages`, `gpg` and
`nfpm` and fails with a clear message before downloading anything.

`make test` needs nfpm too — `TestBuildDeb` and its siblings build a real
.deb from a stubbed bundle — so it downloads the same pinned `nfpm` before
running `go test`, both here and in `make -C os-image test`, which is what the
merge gate runs.

## Bumping versions

1. Edit `K8S_VERSIONS` at the top of the [Makefile](Makefile). It holds the
   newest patch of each of the three newest Kubernetes minors: add the new one,
   drop the oldest. Entries may be written `v1.36.3` or `1.36.3`; the package
   name always drops the `v`.
2. If the bump crosses a **minor**, add a row to `minorPins` in
   [`cmd/kubeadm-bundle/pins.go`](cmd/kubeadm-bundle/pins.go) with the
   containerd, runc and Helm releases for it. There is deliberately no fallback:
   an unlisted minor is an error, so someone has to read containerd's Kubernetes
   support matrix and Helm's version skew policy rather than inherit whatever
   the previous newest line was.
3. Run `make lock` on a Linux host with network access. It downloads and
   verifies every artifact, runs the verified `kubeadm` for its image list,
   resolves each tag to a manifest digest, and rewrites `lock/<version>.json`.
   It prints what changed.
4. **Review the lock diff** — every URL and every digest — with
   `git diff -- lock/`.
5. Commit `lock/` together with the `K8S_VERSIONS` and `pins.go` changes.

`make debs` refuses to build a version with no matching lock, so steps 3 to 5
are not optional.

## Layout

```text
lock/                     # committed digest pins, one file per Kubernetes version
cmd/kubeadm-bundle/       # build host: download, verify, pull images, write manifest.json
cmd/nke-k8s-installer/    # node: install from the bundle on disk (cross-compiled for linux/amd64)
cmd/debmanifest/          # build host: print or golden-check a .deb's package contract
internal/bundle/          # manifest and checksum helpers shared by both
internal/debmanifest/     # manifest generation, parsing and comparison behind cmd/debmanifest
packaging/                # build-deb.sh, nfpm.yaml, maintainer scripts, systemd units, runc keyring
testdata/golden/          # production-package golden manifests and the reviewed deviations allowlist
```

The installer only reads from `NKE_BUNDLE_ROOT`. It never downloads anything and
never calls a package manager: `socat`, `conntrack`, `ipset`, `ebtables` and the
rest of the kubeadm preflight checklist belong to the OS image.

## Provenance

Ported from NVIDIA's internal `nke/packages/builder/kubeadm` builder, trimmed to
what the NKE Packer image build needs: amd64 only, no S3 delivery, no manual
tarball, no Osmo flavor, and container images pulled here rather than fetched
from a separate registry project.
