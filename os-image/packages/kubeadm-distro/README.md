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

dpkg owns every file in the package -- nothing is unpacked, generated or
downloaded by a maintainer script. Inside a package:

| Path | Contents |
|---|---|
| `/usr/bin/{kubeadm,kubectl,kubelet,crictl,helm}`, `/usr/sbin/runc` | pkgs.k8s.io-style binary paths |
| `/usr/local/bin/{containerd,containerd-shim-runc-v2,ctr}` | containerd, staged from its release tarball |
| `/opt/cni/bin/` | the CNI plugins, staged from their release tarball |
| `/etc/containerd/config.toml` | containerd 2.x config, rendered at build time with the sandbox image pinned to this release's pause image |
| `/etc/containerd/conf.d/` | empty; the supported override path (see below) |
| `/etc/cni/net.d/` | empty; CNI config is the OS image's job, not this package's |
| `/etc/crictl.yaml` | crictl pointed at the containerd CRI socket |
| `/etc/default/kubelet` | conffile carrying operator `KUBELET_EXTRA_ARGS` |
| `/lib/systemd/system/kubelet.service{,.d/10-kubeadm.conf}`, `containerd.service` | vendor units, plus the kubeadm drop-in |
| `/usr/lib/nke/k8s-matrix/<version>/` | `manifest.json` and `manifest.json.sha256`, and `amd64/` holding only the control-plane image tarball and its `.sha256` sidecar |

`postinst` only does what a file list cannot express: OS-level state (swap,
sysctls, kernel modules), service enablement, and importing the bundled
control-plane images with `ctr -n k8s.io images import`. Removal is split
across two maintainer scripts because dpkg runs `postrm remove` *after* it has
already deleted the package's files, including the unit files under
`/lib/systemd/system/` -- `systemctl disable` at that point has nothing left
to read and does not reliably remove the `multi-user.target.wants` symlinks.
`prerm` runs the disable step first, while the units still exist; `postrm`
then does `daemon-reload` and removes the sysctl/modules-load drop-ins.
Neither script stops the services, on a real removal or otherwise -- see the
switch-window comment in `packaging/prerm`; it leaves `/etc/fstab` and
containerd's image store alone.

`/etc/containerd/config.toml` and `/etc/crictl.yaml` are package-managed, not
dpkg conffiles: `config.toml`'s sandbox image has to track the bundled images
on every matrix-row switch, which a preserved local edit would defeat, so
both are rewritten on every install. containerd's config loader supports an
`imports` list of additional files merged over the main one
(https://github.com/containerd/containerd/blob/main/docs/man/containerd-config.toml.5.md),
and the rendered `config.toml` sets `imports =
['/etc/containerd/conf.d/*.toml']` -- an operator drops a local override
fragment there instead of editing the package-managed file.

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

## Parity with the production package

The package is a drop-in replacement for the `nke-kubeadm-distro-<x.y.z>`
package previously served from an internal S3 apt repository. The intended
differences from that package are: dpkg owns every file (no
`nke-k8s-installer`, no raw tarballs under `/usr/lib/nke/k8s-matrix`),
`/etc/containerd/config.toml` and `containerd.service` are rendered at build
time, `Depends` adds `conntrack`, `socat` and `ethtool`, `Conflicts` adds
`containerd` and `containerd.io`, and `/etc/default/kubelet` is a conffile.
Everything else — package names, `Provides`, peer `Conflicts`, kubeadm,
kubelet, kubectl and runc binaries and their paths, the bundled image
archive, systemd units — is unchanged.

## Makefile interface

```bash
make -C os-image/packages/kubeadm-distro debs OUT_DIR=/abs/path/to/apt-repo
make -C os-image/packages/kubeadm-distro debs OUT_DIR=/abs/path K8S_VERSIONS="v1.36.3 v1.34.2"
make -C os-image/packages/kubeadm-distro lock
make -C os-image/packages/kubeadm-distro test
make -C os-image/packages/kubeadm-distro repro-check
make -C os-image/packages/kubeadm-distro clean
```

`repro-check` runs `debs` twice into two throwaway `OUT_DIR`s (reusing the
same `DIST_DIR`, so the network download happens at most once) and `cmp`s
every `.deb` in both directions (so a package present in only one build is
also caught, not just a content mismatch) plus the `Packages` index sorted
first (`dpkg-scanpackages`'s listing order isn't guaranteed stable run to
run, only its content), failing with the names of whatever differs. No
`OUT_DIR` needed on the command line -- this target builds its own. `nfpm.yaml`'s
pinned `mtime` and gzip compression exist to make this pass; a regression
here usually means a new content entry that carries build-host-local
metadata (a timestamp, a path, file ownership) nfpm isn't pinning down. Not
wired into CI — run it by hand after touching `packaging/` or
`build-deb.sh`.

`OUT_DIR` is required and must be absolute. Other knobs:

| Variable | Default | Purpose |
|---|---|---|
| `K8S_VERSIONS` | `v1.36.3 v1.35.3 v1.34.2` | Kubernetes releases to build |
| `DEB_REVISION` | `1` | Debian packaging revision |
| `LOCK_DIR` | `./lock` | The committed lock files |
| `DIST_DIR` | `./dist` | Artifact cache. A cached upstream artifact is re-checked against the publisher's checksum document every run, so this still needs the network; the `.sha256` sidecar is an output, not the thing that is trusted. Only the container image tarball is reused on the strength of its own sidecar |

### Build host requirements

**Linux, amd64 only, always.** The build runs the downloaded `kubeadm` binary
to ask it which control-plane images this release needs, and stages the
downloaded containerd/CNI/crictl/helm tarballs into the FHS tree the `.deb`
installs, both of which only work on Linux. There is no development shortcut
that skips this: a `.deb` without the images or without the staged runtime
tree is not a product this build produces. Packages are built with
[nfpm](https://nfpm.goreleaser.com/), which writes `root:root` ownership
straight into the archive metadata without needing `fakeroot` or `dpkg-deb
--root-owner-group` to get there.

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
cmd/kubeadm-bundle/       # build host: download, verify, pull images, stage the FHS tree, write manifest.json
internal/bundle/          # manifest and checksum helpers
packaging/                # build-deb.sh, nfpm.yaml, maintainer scripts, systemd units, runc keyring
```

postinst never downloads anything and never calls a package manager: `socat`,
`conntrack`, `ipset`, `ebtables` and the rest of the kubeadm preflight
checklist belong to the OS image.

## Provenance

Ported from NVIDIA's internal `nke/packages/builder/kubeadm` builder, trimmed to
what the NKE Packer image build needs: amd64 only, no S3 delivery, no manual
tarball, no Osmo flavor, and container images pulled here rather than fetched
from a separate registry project.
