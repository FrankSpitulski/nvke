#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Run the guestfs-tier integration suite in any Docker daemon backed by a
# Linux kernel: one privileged container installs libguestfs and an appliance
# kernel, generates the fixtures, and runs the tagged tests.
#
# Lessons this script encodes (each cost a debugging round):
#  - With a VM-backed daemon (common on macOS), the host's /tmp is typically
#    not shared into the VM: a bind-mount from it silently materializes as an
#    empty directory. The repo lives under $HOME, which such daemons do
#    share, so mounting the repo works; nothing is mounted from /tmp.
#  - The container has no /boot kernel; libguestfs' appliance needs one
#    (linux-image-*) with the vmlinuz world-readable.
#  - guestfish must run --listen/--remote with the keys file as the daemon's
#    stdin; -f script mode does not deliver --keys-from-stdin keys.
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GO_IMAGE="${GO_IMAGE:-golang:1.27-bookworm}"

docker run --rm --privileged \
  -v "${REPO_ROOT}:/src" \
  -v nvke-validate-gomod:/go/pkg/mod \
  -v nvke-validate-gocache:/root/.cache/go-build \
  -w /src/validate \
  "${GO_IMAGE}" bash -euxc '
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -q >/dev/null
    apt-get install -yq libguestfs-tools qemu-utils cryptsetup lvm2 \
      "linux-image-$(dpkg --print-architecture)" >/dev/null 2>&1
    chmod 0644 /boot/vmlinuz-* || true
    export LIBGUESTFS_BACKEND=direct
    hack/gen-guestfs-fixture.sh /fixtures
    VALIDATE_GUESTFS_FIXTURE_DIR=/fixtures go test -tags guestfs -count=1 -v ./pkg/inspect/guestfs/
  '
