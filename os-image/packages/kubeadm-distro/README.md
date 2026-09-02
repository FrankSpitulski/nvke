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
which control-plane images this release needs, which only works on Linux, and
packages are produced with `dpkg-deb --root-owner-group` rather than `fakeroot`.
`make bundle SKIP_IMAGES=1` skips the part that needs a Linux kernel and so runs
anywhere — but the lock is still required, so it only works for a version that
already has a committed `lock/<version>.json`. `make debs` always needs Linux.

```bash
apt install dpkg-dev gnupg
```

`make debs` checks for `dpkg-deb`, `dpkg-scanpackages` and `gpg` and fails with
that message before downloading anything. `gpg` is needed because runc publishes
its checksums as an OpenPGP clearsigned list, verified against
`packaging/keyrings/runc-release.gpg`.

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
internal/bundle/          # manifest and checksum helpers shared by both
packaging/                # build-deb.sh, control, maintainer scripts, systemd units, runc keyring
```

The installer only reads from `NKE_BUNDLE_ROOT`. It never downloads anything and
never calls a package manager: `socat`, `conntrack`, `ipset`, `ebtables` and the
rest of the kubeadm preflight checklist belong to the OS image.

## Provenance

Ported from NVIDIA's internal `nke/packages/builder/kubeadm` builder, trimmed to
what the NKE Packer image build needs: amd64 only, no S3 delivery, no manual
tarball, no Osmo flavor, and container images pulled here rather than fetched
from a separate registry project.
