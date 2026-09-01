#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# setup-efi-build-host.sh -- install what mkosi needs to build qcow-imager.efi.
#
# Called by `make -C os-image/efi setup-host` and by the EFI workflow, so a
# developer preparing a box runs exactly what CI runs.
#
# The package list is the mkosi-relevant subset of NVIDIA/infra-controller's
# .github/actions/setup-mkosi-environment, the only place the real
# requirements are written down. Its Rust-build packages are dropped: the
# qcow-imager task compiles no Rust, cargo-make only sequences shell.
#
# Ubuntu/Debian only; nothing has exercised a non-apt host.

set -euo pipefail

if [[ "$(uname -s)" != "Linux" ]]; then
	echo "ERROR: the EFI build is Linux-only (mkosi needs namespaces); this is $(uname -s)." >&2
	exit 1
fi

if ! command -v apt-get > /dev/null 2>&1; then
	echo "ERROR: no apt-get. See ../README.md for the package list to translate." >&2
	exit 1
fi

SUDO=""
if [[ "$(id -u)" -ne 0 ]]; then
	command -v sudo > /dev/null 2>&1 || {
		echo "ERROR: not root and no sudo. Re-run as root." >&2
		exit 1
	}
	SUDO="sudo"
fi

export DEBIAN_FRONTEND=noninteractive

# debian-archive-keyring bites first and least legibly: mkosi bootstraps a
# Debian tools image before it touches the Ubuntu profile, and without it the
# build dies minutes in naming a .gpg path rather than a package.
echo "=== Installing mkosi host packages ==="
$SUDO apt-get update
$SUDO apt-get install -y \
	apt-utils \
	btrfs-progs \
	bubblewrap \
	cpio \
	debian-archive-keyring \
	debootstrap \
	dosfstools \
	e2fsprogs \
	erofs-utils \
	mtools \
	python3-pefile \
	qemu-utils \
	squashfs-tools \
	systemd-container \
	systemd-ukify \
	ubuntu-keyring \
	uidmap \
	xfsprogs \
	xz-utils \
	zstd

# Only for building the aarch64 EFI on an x86_64 host.
echo "=== Installing cross-architecture emulation (optional) ==="
$SUDO apt-get install -y binfmt-support qemu-user-static ||
	echo "WARNING: no cross-arch emulation; build-efi-arm64 needs an arm64 host."

# Ubuntu 24.04 restricts unprivileged user namespaces through AppArmor, not the
# older kernel.unprivileged_userns_clone sysctl, so a box can report namespaces
# enabled and still fail inside mkosi's sandbox. A missing key is not an error:
# these do not exist on non-Ubuntu kernels.
echo "=== Relaxing AppArmor's unprivileged-namespace restrictions ==="
for knob in kernel.apparmor_restrict_unprivileged_userns kernel.apparmor_restrict_unprivileged_unconfined; do
	if [[ -e "/proc/sys/${knob//./\/}" ]]; then
		$SUDO sysctl -w "${knob}=0"
	fi
done

# cargo has to exist because upstream drives its build with cargo-make, even
# though nothing here compiles Rust. Skipped when already present: replacing a
# developer's toolchain would be rude.
echo "=== Installing the Rust toolchain and cargo-make ==="
if ! command -v cargo > /dev/null 2>&1; then
	curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --no-modify-path --profile minimal
fi
export PATH="${HOME}/.cargo/bin:${PATH}"
if ! cargo make --version > /dev/null 2>&1; then
	cargo install cargo-make --locked
fi

echo
echo "Host prepared. If rustup was just installed, add ~/.cargo/bin to PATH."
echo "Then: make -C os-image/efi check-deps && make -C os-image/efi build-efi-x86"
