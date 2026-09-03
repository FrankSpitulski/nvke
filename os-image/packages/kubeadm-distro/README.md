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
plugins, crictl and Helm, every artifact verified against its publisher's
SHA-256, plus the control-plane container images for that release, so a node
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

dpkg owns every file in the package: nothing is unpacked, generated or
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
| `/usr/lib/nke/k8s-matrix/<version>/amd64/` | the control-plane image tarball and its `.sha256` sidecar, and nothing else |

`postinst` only does what a file list cannot express: OS-level state (swap,
sysctls, kernel modules), service enablement, and importing the bundled
control-plane images with `ctr -n k8s.io images import`. Removal is split
across two maintainer scripts because dpkg runs `postrm remove` *after* it has
already deleted the package's files, including the units under
`/lib/systemd/system/`, where `systemctl disable` has nothing left to read and
does not reliably remove the `multi-user.target.wants` symlinks. `prerm` runs
the disable step first, while the units still exist; `postrm` then does
`daemon-reload` and removes the sysctl/modules-load drop-ins. Neither script
stops the services, and neither touches `/etc/fstab` or containerd's image
store. See the switch-window comment in `packaging/prerm`.

`/etc/containerd/config.toml` and `/etc/crictl.yaml` are package-managed
rather than dpkg conffiles: `config.toml`'s sandbox image has to track the
bundled images on every matrix-row switch, which a preserved local edit would
defeat, so both are rewritten on every install. containerd's config loader
supports an `imports` list of files merged over the main one
(https://github.com/containerd/containerd/blob/main/docs/man/containerd-config.toml.5.md),
and the rendered `config.toml` sets `imports =
['/etc/containerd/conf.d/*.toml']`, so an operator drops a local override
fragment there instead of editing the package-managed file.

The bundled images are the ones kubeadm asks for by default, which means
`registry.k8s.io`. A controller that sets `KUBEADM_IMAGE_REPOSITORY` to a mirror
makes kubeadm ask for different names, and the node pulls from that mirror
instead: the bundled images are then unused, and the node is no longer offline.

The package `Depends` on `iptables, iproute2, conntrack, socat, ethtool`.
kubeadm's preflight check errors without `conntrack` and warns when `socat` or
`ethtool` are missing.

## Lock files

Everything a bundle is built from is pinned by digest in `lock/<version>.lock`,
committed and reviewed: the `sha256` of every upstream artifact and the
linux/amd64 manifest digest of every container image. `make debs` refuses to
build a version that has no lock, verifies each download against the locked
digest as well as against the publisher's checksum, and pulls images by
`ref@digest` rather than by tag. See [lock/README.md](lock/README.md).

## Parity with the production package

The package is a drop-in replacement for the `nke-kubeadm-distro-<x.y.z>`
package served from the internal S3 apt repository. The intended differences
from it: dpkg owns every file (no `nke-k8s-installer`, no raw tarballs under
`/usr/lib/nke/k8s-matrix`), `/etc/containerd/config.toml` and
`containerd.service` are rendered at build time, `Depends` adds `conntrack`,
`socat` and `ethtool`, `Conflicts` adds `containerd` and `containerd.io`, and
`/etc/default/kubelet` is a conffile. Package names, `Provides`, peer
`Conflicts`, the binaries and their paths, the bundled image archive and the
systemd units are unchanged.

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
every `.deb` in both directions, so a package present in only one build is
caught too. The `Packages` index is sorted first, since `dpkg-scanpackages`
guarantees its content but not its listing order. It builds its own
`OUT_DIR`. `nfpm.yaml`'s pinned `mtime` and gzip compression exist to make
this pass; a regression here usually means a new content entry carrying
build-host-local metadata (a timestamp, a path, file ownership) that nfpm
isn't pinning down. Not wired into CI, so run it by hand after touching
`packaging/` or `build-deb.sh`.

`OUT_DIR` is required and must be absolute. Other knobs:

| Variable | Default | Purpose |
|---|---|---|
| `K8S_VERSIONS` | `v1.36.3 v1.35.3 v1.34.2` | Kubernetes releases to build |
| `DEB_REVISION` | `1` | Debian packaging revision |
| `LOCK_DIR` | `./lock` | The committed lock files |
| `DIST_DIR` | `./dist` | Artifact cache. A cached artifact whose bytes already hash to what the lock pins is reused without touching the network, so a second `make debs` is offline; anything else is re-downloaded and re-verified against the publisher's checksum |

### Build host requirements

**Linux, amd64 only, always.** The build runs the downloaded `kubeadm` binary
to ask it which control-plane images this release needs, and stages the
downloaded containerd/CNI/crictl/helm tarballs into the FHS tree the `.deb`
installs, both of which only work on Linux. Packages are built with
[nfpm](https://nfpm.goreleaser.com/), which writes `root:root` ownership
straight into the archive metadata without `fakeroot` or `dpkg-deb
--root-owner-group`.

```bash
apt install dpkg-dev gnupg
```

`make debs` fails before it downloads anything if the host is not Linux or if
any of these is missing: `curl`, `gpg`, `gzip`, `sha256sum` and `tar` (what
`bundle.sh` fetches, verifies and unpacks with, checked by `check-host`), plus
`dpkg-scanpackages` and `nfpm` (what turns the bundle into a package and an
apt index, checked by `check-deb-tools`).

`dpkg-dev` is needed for `dpkg-scanpackages`, which writes the flat apt index;
`gpg` is needed because runc publishes its checksums as an OpenPGP clearsigned
list, verified against `packaging/keyrings/runc.keyring`. The `.deb` itself is
built by the pinned `nfpm` (`NFPM_VERSION` in
[`../../../make/tool-versions.mk`](../../../make/tool-versions.mk)), which
`make debs` `go install`s into `./bin` like every other Go tool here.
`dpkg-deb` is only for inspecting the result, which is what this package's
tests do with it.

`make test` needs nfpm too, since `TestBuildDeb` and its siblings build a real
.deb from a stubbed bundle, so it builds the same pinned `nfpm` first, both
here and in `make -C os-image test`, which is what the merge gate runs.

## Bumping versions

1. Edit `K8S_VERSIONS` at the top of the [Makefile](Makefile). It holds the
   newest patch of each of the three newest Kubernetes minors: add the new one,
   drop the oldest. Entries may be written `v1.36.3` or `1.36.3`; the package
   name always drops the `v`.
2. If the bump crosses a **minor**, add a row to the `pins_for` table in
   [`bundle.sh`](bundle.sh) with the containerd, runc and Helm releases for it.
   There is no fallback row: an unlisted minor is an error, so someone has to
   read containerd's Kubernetes support matrix and Helm's version skew policy.
3. Run `make lock` on a Linux host with network access. It downloads and
   verifies every artifact, runs the verified `kubeadm` for its image list,
   resolves each tag to a manifest digest, and rewrites `lock/<version>.lock`.
   It prints what changed.
4. **Review the lock diff**, every URL and every digest, with
   `git diff -- lock/`.
5. Commit `lock/` together with the `K8S_VERSIONS` and `bundle.sh` changes.

`make debs` refuses to build a version with no matching lock, so steps 3 to 5
are not optional.

## Layout

```text
bundle.sh                 # build host: pin table, download, verify, pull images, stage the FHS tree
lock/                     # committed digest pins, one file per Kubernetes version
packaging/                # build-deb.sh, nfpm.yaml, maintainer scripts, systemd units, runc keyring
test/                     # Go tests over bundle.sh and build-deb.sh; no product Go here
```

postinst never downloads anything and never calls a package manager: `socat`,
`conntrack`, `ipset`, `ebtables` and the rest of the kubeadm preflight
checklist belong to the OS image.

## Provenance

Ported from NVIDIA's internal `nke/packages/builder/kubeadm` builder, trimmed to
what the NKE Packer image build needs: amd64 only, no S3 delivery, no manual
tarball, no Osmo flavor, and container images pulled here rather than fetched
from a separate registry project.
