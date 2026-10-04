#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

error() {
    echo "Error: $1" >&2
    exit 1
}

is_chrony_installed() {
    dpkg -l chrony &> /dev/null
}

install_chrony() {
    if is_chrony_installed; then
        echo "chrony is already installed"
        return 0
    fi

    echo "Installing chrony..."
    if ! apt-get update -o DPkg::Lock::Timeout=600 &> /dev/null; then
        error "Failed to update package lists"
    fi

    if ! apt-get install -y -o DPkg::Lock::Timeout=600 chrony &> /dev/null; then
        error "Failed to install chrony"
    fi

    echo "chrony installed successfully"
}

main() {
    install_chrony
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main
fi
