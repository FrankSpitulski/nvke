<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Package contract golden files

`s3-<version>.manifest` is a normalized text manifest of the production
`nke-kubeadm-distro-<version>` Debian package as it is actually served today
from the internal S3 apt repository — the control fields, conffiles,
maintainer scripts, and file listing (with a sha256 for every regular file
under `/usr/bin`, `/usr/sbin`, `/usr/local/bin` or `/opt/cni/bin`). It is the
ground truth this repository's own build is checked against, so that
replacing the internal S3-delivered package with the one this repository
builds is a reviewed, deliberate change rather than an accident.

Two normalizations make the format deterministic regardless of how the
package was built: the comma-list control fields (`Provides`, `Depends`,
`Conflicts`, `Replaces`) are sorted, and `Version` is excluded entirely —
same as `Installed-Size`, `Maintainer` and `Description` — because it is
just the Debian packaging revision (`DEB_REVISION`), not part of the
contract, and is not expected to match the S3 package's own revision.

`make contract-check` (wired into `make debs`, see the module
[README.md](../../README.md#package-contract)) builds a manifest for each
`.deb` this build produces with `debmanifest` and compares it against the
matching `s3-<version>.manifest` here, allowing only the differences listed
in [`deviations.txt`](deviations.txt).

## Rule: goldens are never edited

These three files are captured once, from the packages actually being
replaced, and are not touched again. If this build's package legitimately
needs to differ from production — a dependency added, a placeholder file
dropped, a field's peer list restructured — that difference is recorded as a
reviewed line in [`deviations.txt`](deviations.txt) with a `#` reason, not by
editing the golden file to match. A golden file changes only if a new
production S3 package is captured to replace it (a new `debmanifest` capture,
following the same procedure below), which is itself a decision to record in
a commit message, not something that happens silently alongside an unrelated
change.

## Capture procedure

1. List the production apt index to find the exact package row (name,
   `Version`, and pool path) for the version being captured:

   ```text
   https://nke-prod-assets.s3.us-west-2.amazonaws.com/nke-apt-noble/dists/nke-noble/main/binary-amd64/Packages
   ```

   Search it for the `Package: nke-kubeadm-distro-<version>` stanza. At
   capture time (2026-09-03) the three rows captured here were all
   `Version: 7`.

   **The repository is source-IP restricted.** Fetching `Packages` or a
   pool `.deb` requires running from a host inside NVIDIA's network (or
   through a proxy that is); it is not reachable from an arbitrary CI runner
   or a contributor's laptop off-network.

2. Download the `.deb` named in the `Filename:` field of that stanza (under
   `pool/main/n/nke-kubeadm-distro-<version>/`) and verify it against the
   stanza's `SHA256:` field before doing anything else with it:

   | Version | sha256 of the captured `.deb` |
   |---|---|
   | 1.34.2 | `6e3852fe8a502d16c16e39fc9a9f43ba54ab1fbcd56a66934e3518ff5e026d89` |
   | 1.35.3 | `6a8a9c96efae06f833a0cde5a3d2f776d308bc555cd18e303525af582762a3a4` |
   | 1.36.3 | `af62835891d0778329b2bdf0473affe3e148e5d1b27c111876a30c0f4b821023` |

3. Run this package's `cmd/debmanifest` and commit its output verbatim as
   `s3-<version>.manifest`:

   ```bash
   debmanifest <deb> > s3-<version>.manifest
   ```

   **Do not** commit the `.deb` itself, or any of the other files a capture
   session produces (`postinst`, `manifest.json`, `configs.txt`, and
   similar) — none of those are needed to keep the contract enforced, and
   the `.deb` alone is ~200-400 MB.

4. Delete the downloaded `.deb` and every other captured artifact once the
   manifest is committed. Nothing from the capture besides the
   `s3-<version>.manifest` text file belongs in this repository.

## Known drift (not yet in `deviations.txt`)

As of this package's initial commit, `make debs` with the **current**
package layout (the installer binary still shipped, see
[`packages/kubeadm-distro/README.md`](../../README.md)) does not match these
goldens exactly. The expected differences are:

- `Depends` — this build adds `conntrack, socat, ethtool` to the S3
  package's `iptables, iproute2`.
- `Conflicts` — this build adds `containerd, containerd.io` (this package
  ships containerd's unit and config, the S3 package does not), and the peer
  list differs because the S3 index lists more historical rows than
  `K8S_VERSIONS` builds today.
- `Replaces` — same peer-list difference as `Conflicts`.
- The `.kubelet-keep` placeholder files under
  `/etc/kubernetes/manifests/` and `/var/lib/kubelet/` — the S3 package
  ships them, this build ships the directories empty instead.

(`Version` is not in this list: it is excluded from the manifest entirely,
not compared at all, so it can never show up as drift.)

These are not yet recorded in `deviations.txt`: `make contract-check` prints
the diff as an advisory warning (see the module README) rather than failing
`make debs`, because they have not been reviewed as a deliberate, permanent
contract yet. They are expected to be reviewed and allowlisted together with
the layout change that drops the `nke-k8s-installer` binary (tracked
separately), at which point `deviations.txt` stops being empty and
`contract-check` becomes strict.
