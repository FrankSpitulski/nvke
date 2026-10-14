#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Run the boot-tier integration suite in any Docker daemon backed by a Linux
# kernel: one container installs QEMU + UEFI firmware + the
# seed/ssh tools, fetches a stock Ubuntu cloud image for the guest arch
# (cached in a named volume), and runs the tagged tests under TCG.
#
# TCG is the expected accelerator when the daemon's kernel has no nested
# KVM; a measured spike showed boot as wait-dominated, so this stays in
# single-digit minutes. Shares the mount lessons of
# test-guestfs-container.sh: repo mounted from $HOME, nothing bind-mounted
# from /tmp (VM-backed daemons often do not share it).
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GO_IMAGE="${GO_IMAGE:-golang:1.27-bookworm}"

docker run --rm \
  -v "${REPO_ROOT}:/src" \
  -v nvke-validate-gomod:/go/pkg/mod \
  -v nvke-validate-gocache:/root/.cache/go-build \
  -v nvke-validate-bootimg:/images \
  -w /src/validate \
  "${GO_IMAGE}" bash -euxc '
    export DEBIAN_FRONTEND=noninteractive
    arch="$(dpkg --print-architecture)"
    apt-get update -q >/dev/null
    apt-get install -yq qemu-system-arm qemu-system-x86 qemu-efi-aarch64 ovmf \
      cloud-image-utils genisoimage openssh-client curl ca-certificates > /tmp/apt.log 2>&1 || { tail -5 /tmp/apt.log; apt-get install -yq qemu-system-arm qemu-system-x86 qemu-efi-aarch64 ovmf cloud-image-utils genisoimage openssh-client curl ca-certificates > /tmp/apt2.log 2>&1 || { tail -20 /tmp/apt2.log; exit 1; }; }
    case "${arch}" in
      arm64) img=ubuntu-24.04-server-cloudimg-arm64.img ;;
      amd64) img=ubuntu-24.04-server-cloudimg-amd64.img ;;
      *) echo "unsupported arch ${arch}"; exit 1 ;;
    esac
    if [ ! -f "/images/${img}" ]; then
      curl -sSL -o "/images/${img}.part" "https://cloud-images.ubuntu.com/releases/noble/release/${img}"
      mv "/images/${img}.part" "/images/${img}"
    fi
    VALIDATE_BOOT_IMAGE="/images/${img}" go test -tags boot -count=1 -timeout 30m -v ./pkg/inspect/qemu/
  '
