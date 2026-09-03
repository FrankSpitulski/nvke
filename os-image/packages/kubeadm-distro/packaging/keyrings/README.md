<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# `runc-release.gpg`

Binary GPG keyring used by `internal/bundle.VerifyClearsignedChecksumDoc` to
verify runc's clearsigned `runc.sha256sum` checksum document before
`nke-kubeadm-distro-<x.y.z>` bundles the runc binary.

## Provenance

Rebuilt verbatim from the runc repository's `runc.keyring` at commit
[`68fbaabf63efc4ff2ed913c7df53d18c87be1432`](https://github.com/opencontainers/runc/blob/68fbaabf63efc4ff2ed913c7df53d18c87be1432/runc.keyring)
(`main` as of 2026-09-03).

## Keys in this keyring

| Fingerprint | UID | Created (UTC) | Expires (UTC) |
|---|---|---|---|
| `C9C3 70B2 46B0 9F6D BCFC  744C 3440 1015 D1D2 D386` | Aleksa Sarai \<cyphar@cyphar.com\> | 2019-06-21 | does not expire |
| `C242 8CD7 5720 FACD CF76  B6EA 17DE 5ECB 75A1 100E` | Kir Kolyshkin \<kolyshkin@gmail.com\> | 2020-04-28 | 2028-04-18 |
| `C020 EA87 6CE4 E06C 7AB9  5AEF 4952 4C6F 9F63 8F1A` | Akihiro Suda \<suda.kyoto@gmail.com\>, Akihiro Suda \<akihiro.suda.cz@hco.ntt.co.jp\> | 2019-07-25 | 2026-07-31 |

## Rebuilding the binary keyring

```bash
curl -fsSL \
  https://raw.githubusercontent.com/opencontainers/runc/68fbaabf63efc4ff2ed913c7df53d18c87be1432/runc.keyring \
  | gpg --dearmor -o runc-release.gpg
```
