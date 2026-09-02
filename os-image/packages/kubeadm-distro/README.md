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
plugins, crictl and Helm — every one of them the **upstream release binary**,
pinned by SHA-256 — plus the control-plane container images for that release, so
a node can come up without reaching a registry.

Nothing here is built from source. `nix` is the whole toolchain: `fetchurl`
downloads each upstream artifact as a fixed-output derivation,
`dockerTools.pullImage` fetches each image by its linux/amd64 digest, and `fpm`
turns the staged tree into a `.deb`.

## What it builds

```text
<OUT_DIR>/nke-kubeadm-distro-1.36.3_1_amd64.deb
<OUT_DIR>/nke-kubeadm-distro-1.35.3_1_amd64.deb
<OUT_DIR>/nke-kubeadm-distro-1.34.2_1_amd64.deb
<OUT_DIR>/Packages                                  # flat apt index
```

The Kubernetes patch is part of the **package name**; the Debian `Version` field
is only the packaging revision — `debRevision` in [`deb.nix`](deb.nix), default
`1`, bumped there when the package content changes but the Kubernetes patch does
not. It is not a Makefile knob. Every package
`Provides: nke-kubeadm-distro` and `Conflicts` with every other row in
`versions.json`, so exactly one matrix row can be installed at a time.

Inside a package:

| Path | Contents |
|---|---|
| `/usr/bin/{kubeadm,kubectl,kubelet,crictl,helm}`, `/usr/sbin/runc` | pkgs.k8s.io-style binary paths |
| `/usr/local/bin/{containerd,containerd-shim-runc-v2,ctr}` | the containerd release tarball's binaries, where its own unit expects them |
| `/opt/cni/bin/*` | the CNI plugins |
| `/etc/containerd/config.toml`, `/etc/crictl.yaml` | containerd config version 3, sandbox image pinned to this release's `pause`, `SystemdCgroup = true` |
| `/etc/default/kubelet` | the one conffile: operator `KUBELET_EXTRA_ARGS` survives a version switch |
| `/lib/systemd/system/containerd.service` | containerd's own recommended unit |
| `/lib/systemd/system/kubelet.service{,.d/10-kubeadm.conf}` | vendor unit plus the kubeadm drop-in |
| `/etc/kubernetes/manifests`, `/var/lib/kubelet` | empty; kubeadm's `DirAvailable` preflight check treats *any* entry, hidden or not, as "not empty" |
| `/usr/lib/nke/k8s-matrix/<version>/manifest.json` | what this row was built from: component versions, artifact URLs and digests, image digests |
| `/usr/lib/nke/k8s-matrix/<version>/amd64/k8s-base-container-images_<x.y.z>_linux_amd64.tgz` | one gzipped docker-archive of every control-plane image, with a `.sha256` sidecar |

The binaries are shipped once, by dpkg. There is no second copy under
`k8s-matrix/` and no installer binary: dpkg lays the files down, and `postinst`
only does the part a file list cannot express — swap, kernel modules, sysctls,
starting containerd, and importing the bundled images with
`ctr -n k8s.io images import`.

The bundled images are the ones kubeadm asks for by default, which means
`registry.k8s.io`. A controller that sets `KUBEADM_IMAGE_REPOSITORY` to a mirror
makes kubeadm ask for different names, and the node pulls from that mirror
instead: the bundled images are then unused, and the node is no longer offline.

The package `Depends` on `iptables, iproute2, conntrack, socat, ethtool`.
`conntrack` is a hard requirement — kubeadm's preflight check errors without it
— and kubeadm warns when `socat` or `ethtool` are missing.

## The lock

Everything a package is built from is pinned in `versions.json`, committed and
reviewed: the `sha256` of every upstream artifact, and for every container image
its linux/amd64 **manifest digest** plus the nix output `hash` `pullImage`
needs. Those two are not the same thing: `digest` names the image in the
registry, `hash` covers the archive skopeo writes for it.

```json
{
  "v1.36.3": {
    "components": { "containerd": "v2.3.0", "helm": "v4.1.4", "…": "…" },
    "artifacts": {
      "kubeadm": { "url": "https://dl.k8s.io/release/v1.36.3/bin/linux/amd64/kubeadm", "sha256": "…" }
    },
    "images": [
      { "ref": "registry.k8s.io/pause:3.10.2", "digest": "sha256:…", "hash": "sha256-…" }
    ]
  }
}
```

It is **generated, reviewed and committed** — never hand-written:

```bash
make lock                  # Linux host with network access
git diff -- versions.json  # read every changed URL and digest
```

`prefetch.sh` takes each artifact's SHA-256 from the *publisher's own* checksum
document (`dl.k8s.io`'s `.sha256`, containerd's `.sha256sum`, runc's
`runc.sha256sum`, `get.helm.sh`'s `.sha256`) rather than from whatever happened
to download, verifies the download against it, then runs the verified `kubeadm`
to list the images.

There is exactly **one signature check**, and it is runc's. runc is the only one
of these publishers that signs its checksum list, so `runc.sha256sum` is
verified as an OpenPGP clearsigned document against the committed release
keyring in [`keyrings/runc-release.gpg`](keyrings) before any digest is read out
of it; an unsigned or unverifiable document fails the lock. The others publish a
bare `.sha256` next to the artifact, where whoever can rewrite one can rewrite
the other, so for those the checksum document buys integrity against a partial
download, not provenance — which is why the lock diff is reviewed by hand. A mutable tag, a compromised CDN, or a silently rebuilt
upstream release therefore cannot change what ships without the change showing
up as a diff here first. `nix flake check` fails if `versions.json` is not the
canonical `jq -S --indent 2` form, which is how a hand edit gets caught.

## Makefile interface

```bash
make -C os-image/packages/kubeadm-distro debs OUT_DIR=/abs/path/to/apt-repo
make -C os-image/packages/kubeadm-distro debs OUT_DIR=/abs/path K8S_VERSIONS="v1.36.3 v1.34.2"
make -C os-image/packages/kubeadm-distro lock
make -C os-image/packages/kubeadm-distro check
make -C os-image/packages/kubeadm-distro clean
```

`OUT_DIR` is required and must be absolute. Other knobs:

| Variable | Default | Purpose |
|---|---|---|
| `K8S_VERSIONS` | `v1.36.3 v1.35.3 v1.34.2` | Which rows of `versions.json` to build |
| `LOCK` | `./versions.json` | The committed lock |
| `NIX` | `nix` | The nix binary to use |

`K8S_VERSIONS` selects which packages to *build*; it does not change what any of
them declares about its peers. Every package conflicts with every other row in
`versions.json`, so a partial build cannot produce a deb that would coexist with
one from a full build.

### Build host requirements

**x86_64 Linux with nix, and nothing else.** No `dpkg-dev`, no `gnupg`, no Go
toolchain installed on the host: `dpkg-scanpackages` and `fpm` come from the
flake's own pinned nixpkgs, and so does the `gnupg` `make lock` verifies runc's
signed checksum list with. Install nix with the standard installer from
[nixos.org/download](https://nixos.org/download) -- not the Determinate
installer, whose defaults differ. Flakes are enabled per invocation with
`--extra-experimental-features 'nix-command flakes'`, so `nix.conf` needs no
change.

**This is not the Packer bake host.** A `.deb` is a plain artifact, so the two
are deliberately separate machines: build here, then hand the output directory
to the bake through `KUBEADM_DISTRO_DEBS_DIR`.

```bash
# Here, on the nix host:
make -C os-image/packages/kubeadm-distro debs OUT_DIR=/abs/path/to/debs

# There, on the bake host, which needs no nix:
KUBEADM_DISTRO_DEBS_DIR=/abs/path/to/debs make -C os-image/packer build
```

Nothing nix-shaped survives into the product. The node that installs one of
these packages has never heard of nix: dpkg lays down kubeadm, kubelet,
kubectl, containerd, runc, the CNI plugins, crictl and helm, and `postinst`
prepares the node and imports the bundled images.

`make debs` refuses to run without nix, or on a non-Linux host, before
downloading anything. Building on a machine that is neither means a
linux/amd64 container or a remote nix builder:

```bash
docker run --rm --platform linux/amd64 \
  -e NIX_CONFIG=$'experimental-features = nix-command flakes\nfilter-syscalls = false' \
  -v "$PWD":/src -v nke-nix-store:/nix -w /src nixos/nix:latest \
  nix shell --inputs-from path:/src nixpkgs#gnumake \
    --command make debs OUT_DIR=/src/out
```

CI wiring is a follow-up: no workflow builds these debs yet, so today they are
built by hand on a nix host and passed to the bake.


## Bumping versions

1. Edit `K8S_VERSIONS` at the top of the [Makefile](Makefile). It holds the
   newest patch of each of the three newest Kubernetes minors: add the new one,
   drop the oldest. Entries may be written `v1.36.3` or `1.36.3`; the package
   name always drops the `v`.
2. If the bump crosses a **minor**, add a row to the pin tables at the top of
   [`prefetch.sh`](prefetch.sh) with the containerd, runc and Helm releases for
   it. There is deliberately no fallback: an unlisted minor is an error, so
   someone has to read containerd's Kubernetes support matrix and Helm's version
   skew policy rather than inherit whatever the previous newest line was.
3. Run `make lock` on a Linux host with network access. It downloads and
   verifies every artifact, runs the verified `kubeadm` for its image list,
   resolves each tag to a linux/amd64 manifest digest, and rewrites
   `versions.json`. It prints what changed.
4. **Review the lock diff** — every URL and every digest — with
   `git diff -- versions.json`.
5. Commit `versions.json` together with the `K8S_VERSIONS` and `prefetch.sh`
   changes.

`make debs` builds from `versions.json` alone, so a version listed in
`K8S_VERSIONS` with no row there fails to evaluate; steps 3 to 5 are not
optional.

## Layout

```text
flake.nix       # inputs (pinned nixpkgs), one package per row, cheap checks
flake.lock      # the nixpkgs pin
versions.json   # the lock: artifact URLs + sha256, image refs + digests
deb.nix         # version -> .deb: fetchurl, pullImage, merge, render, fpm
postinst        # the node-level state dpkg cannot express, plus the image import
prefetch.sh     # `make lock`: regenerates versions.json from upstream
keyrings/       # runc's OpenPGP release keys, to verify runc.sha256sum
Makefile        # debs / lock / check / fmt / clean
```

## Provenance

Ported from NVIDIA's internal `nke/packages/builder/kubeadm` builder, trimmed to
what the NKE Packer image build needs: amd64 only, no S3 delivery, no manual
tarball, no Osmo flavor, and container images pulled here rather than fetched
from a separate registry project.
