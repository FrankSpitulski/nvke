<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# `runc-release.gpg`

Binary GPG keyring used by `internal/bundle.VerifyClearsignedChecksumDoc` to
verify the clearsigned `runc.sha256sum` checksum document published with
each runc release. It is the trust anchor for the runc binary staged into
the `nke-kubeadm-distro-<x.y.z>` deb: whatever key is in this file is a key
that can make the bundler accept a runc build as authentic.

## Provenance of this file

Rebuilt verbatim from the runc repository's `runc.keyring`, pinned to a
specific commit so the rebuild is reproducible:

- Source: `runc.keyring` at commit
  [`68fbaabf63efc4ff2ed913c7df53d18c87be1432`](https://github.com/opencontainers/runc/blob/68fbaabf63efc4ff2ed913c7df53d18c87be1432/runc.keyring)
  (`main` as of 2026-09-03).
- SHA-256 of that fetched, still-armored file:
  `9bd9742cb79b215cf5f7f71ea71a51a2a95ab573412d54c2905be072d8fcab7c`
- SHA-256 of the committed binary (`gpg --dearmor` output, this file):
  `cdec7014cf714a452a6da3f7db8dbf24495db8d466e0b6b7d4f9ab7888c65e68`

## Keys in this keyring

Listed with:

```bash
gpg --no-default-keyring --keyring ./runc-release.gpg \
    --list-keys --with-fingerprint --with-colons
```

Run against a scratch `--homedir` (see "Review rule" below for why that
matters). The `--with-colons` date fields are epoch seconds; the table below
converts them to UTC calendar dates.

Primary (`pub`) keys, parsed from the `pub`/`fpr`/`uid` records:

| Fingerprint | UID | Created (UTC) | Expires (UTC) |
|---|---|---|---|
| `C9C3 70B2 46B0 9F6D BCFC  744C 3440 1015 D1D2 D386` | Aleksa Sarai \<cyphar@cyphar.com\> | 2019-06-21 | does not expire |
| `C242 8CD7 5720 FACD CF76  B6EA 17DE 5ECB 75A1 100E` | Kir Kolyshkin \<kolyshkin@gmail.com\> | 2020-04-28 | 2028-04-18 |
| `C020 EA87 6CE4 E06C 7AB9  5AEF 4952 4C6F 9F63 8F1A` | Akihiro Suda \<suda.kyoto@gmail.com\>, Akihiro Suda \<akihiro.suda.cz@hco.ntt.co.jp\> | 2019-07-25 | 2026-07-31 |

Notes:

- Sarai's primary key (row one) carries several signing/encryption/auth
  subkeys; three of them show `r` (revoked) in the `--with-colons` listing.
  The primary key itself is not revoked. Subkeys aren't reproduced in this
  table — `VerifyClearsignedChecksumDoc` checks `GOODSIG`/`VALIDSIG` against
  the primary key, and `gpg` resolves the correct signing subkey on its own.
- Suda's key (row three) shows **expired** as of ~2026-07-31 upstream. This
  is deliberate and not a bug in this keyring: our `checkGpgStatus` hard-fails
  on `EXPKEYSIG`, so a checksum document signed by Suda's key will be
  rejected until upstream rotates it (or we intentionally re-pin an updated
  `runc.keyring`, which would need its own README diff). If a future runc
  release is signed only by Suda's key, bundling will fail until that key is
  refreshed upstream.

## Upstream source

These are the maintainer keys published in the runc repository's own
`runc.keyring` file:

<https://github.com/opencontainers/runc/blob/main/runc.keyring>

That file is canonical and kept ASCII-armored upstream; this directory
carries the binary (`gpg --dearmor`) form so `gpg --keyring` can load it
directly with no preprocessing at runtime.

## Rebuilding the binary keyring

To reproduce `runc-release.gpg` from the upstream keyring, pinned to the
commit this file was regenerated from:

```bash
curl -fsSL \
  https://raw.githubusercontent.com/opencontainers/runc/68fbaabf63efc4ff2ed913c7df53d18c87be1432/runc.keyring \
  | gpg --dearmor -o runc-release.gpg
```

To pick up a newer upstream `runc.keyring`, replace the commit SHA with the
current tip of `runc`'s `main` branch (`git ls-remote
https://github.com/opencontainers/runc main`), then re-run the
`--list-keys --with-fingerprint --with-colons` command above **against a
scratch `--homedir`** and diff the fingerprints/uids/expiry against the
table here before committing. Also record the new commit SHA and the
SHA-256 of both the fetched armored file and the dearmored binary.

## Review rule

**Any change to `runc-release.gpg` must ship with a diff of this README**
showing exactly which key(s) were added, removed, or changed (fingerprint,
uid, or expiry) and why, plus the new pinned commit SHA and SHA-256 values
above. A pull request that touches the binary keyring without updating this
table should be rejected on sight — the binary file is not human-reviewable
on its own.

When inspecting or rebuilding this file, always use an explicit, scratch
`--homedir` (for example `gpg --homedir "$(mktemp -d)" ...`), never your own
default `~/.gnupg`. If your default GNUPGHOME has `use-keyboxd` set in
`common.conf` (an opt-in in GnuPG 2.4, the default in GnuPG 2.5+), a bare
`gpg --keyring ./runc-release.gpg --list-keys` silently ignores the
`--keyring` flag and lists keys from your own keybox instead of this file,
which will make a stale or tampered `runc-release.gpg` look correct.
