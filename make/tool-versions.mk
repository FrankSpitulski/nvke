# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Canonical tool versions. This file contains values only so module Makefiles
# can include it without adding root targets to their help output.

GOVULNCHECK_GO_VERSION ?= 1.26.6
GOVULNCHECK_VERSION    ?= v1.6.0
GOVULNCHECK_WAIVED_MODULES ?= controller/nico-vms

ACTIONLINT_VERSION ?= 1.7.12
SHELLCHECK_VERSION ?= 0.11.0
GITLEAKS_VERSION   ?= 8.30.1

KUBECONFORM_VERSION ?= v0.7.0
HELM_VERSION         ?= v4.2.4

KIND_VERSION    ?= v0.32.0
KUBECTL_VERSION ?= v1.36.0
# clusterctl and the CAPI core/kubeadm providers ship as one release, so one
# pin covers the binary and everything `clusterctl init` installs.
CAPI_VERSION    ?= v1.12.3

ORAS_VERSION        ?= 1.3.3
PACKER_VERSION      ?= 1.14.2
GO_LICENSES_VERSION ?= v2.0.1

# os-image/packages/kubeadm-distro/Makefile: resolves and pulls the base
# container images with crane instead of go-containerregistry as a library.
# The sha256 pins live here too, next to the version, so a version bump moves
# both in the same change. Darwin has no pin: a developer's own `crane` (for
# example via `brew install crane`) is used instead.
CRANE_VERSION ?= 0.22.0
CRANE_LINUX_AMD64_SHA256 ?= edb74d53fad9a596860f59d1c5d04a43dfb5f441dc71f57060dd0bf39483c833
CRANE_LINUX_ARM64_SHA256 ?= 971e8e2de7e09172330527a9dfb10689e344b160fd81bac7fb91adcb109bad37
