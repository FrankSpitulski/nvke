<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Lock files

One `<version>.json` per Kubernetes version in `K8S_VERSIONS`, for example
`v1.36.3.json`. Each pins, by digest, everything the bundle for that version is
built from:

```json
{
  "kubernetes": "v1.36.3",
  "artifacts": {
    "kubeadm": { "url": "https://dl.k8s.io/release/v1.36.3/bin/linux/amd64/kubeadm", "sha256": "…" }
  },
  "images": [
    { "ref": "registry.k8s.io/pause:3.10", "digest": "sha256:…" }
  ]
}
```

These files are **generated, reviewed and committed** — never hand-written:

```bash
make lock          # Linux host with network access
git diff -- lock/  # read every changed URL and digest
```

`make debs` refuses to build a version with no lock, checks every downloaded
artifact against the locked `sha256` on top of the publisher's own checksum, and
pulls each image by `ref@digest` rather than by tag. A mutable tag, a
compromised CDN, or a silently rebuilt upstream release therefore cannot change
what ships without the change showing up as a diff here first. It also means the
`kubeadm` binary is verified against a reviewed digest *before* the build host
executes it to list the images.

A version listed in `K8S_VERSIONS` with no file here has not been locked yet; run `make lock` for it.
