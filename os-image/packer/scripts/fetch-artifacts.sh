#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Run on the Packer host. Builds the nke-agent binary from this checkout's
# source (agent/), stages the nke-kubeadm-distro debs (offline kubeadm
# install at cluster create, staged into the image by provision.sh as a
# local apt repo), and fetches SPIRE agent from GitHub Releases into
# ARTIFACTS_DIR.
#
# The kubeadm-distro debs are built by nix, which the bake host does not have
# to have: point KUBEADM_DISTRO_DEBS_DIR at debs built on a host that does
# (`make -C os-image/packages/kubeadm-distro debs OUT_DIR=...`, or a CI job)
# and they are copied in as plain artifacts. Building them here is the
# convenience path for a developer whose workstation already has nix.

set -euo pipefail

ARTIFACTS_DIR="${ARTIFACTS_DIR:?ARTIFACTS_DIR is required}"

# Pinned: same SPIRE release CAPD e2e already vendors (controller/hack/ci).
SPIRE_VERSION="${SPIRE_VERSION:-1.14.1}"
SPIRE_TARBALL="spire-${SPIRE_VERSION}-linux-amd64-musl.tar.gz"
SPIRE_URL="https://github.com/spiffe/spire/releases/download/v${SPIRE_VERSION}/${SPIRE_TARBALL}"
SPIRE_SHA256="${SPIRE_SHA256:-0278af1e083fda58a6dfd09f97d41b5c34b011dca2fd53dc79d0e418996ed0ba}"

mkdir -p "${ARTIFACTS_DIR}"

if ! command -v go >/dev/null 2>&1; then
  echo "go is required to build nke-agent from source (agent/)" >&2
  echo "https://go.dev/dl/" >&2
  exit 1
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

# build-amd64 cross-compiles (CGO off) on non-linux/amd64 hosts and stamps
# main.VERSION from `git describe --match 'agent/v*'`, same as a release build.
echo "building nke-agent from source (${repo_root}/agent)"
make -C "${repo_root}/agent" build-amd64
install -m 0755 "${repo_root}/agent/bin/linux_amd64/nke-agent" "${ARTIFACTS_DIR}/nke-agent"

# One deb per pinned Kubernetes version plus a flat apt index, which
# provision.sh stages as a local repo for an offline cluster-create install.
#
# Two hosts, on purpose. The debs are a nix build; the image is not. A machine
# with nix produces them, this one only copies them in, and the node that
# installs one has never heard of nix -- dpkg puts down kubeadm, kubelet,
# kubectl, containerd, runc, the CNI plugins, crictl and helm, and the
# postinst does the rest. Set KUBEADM_DISTRO_DEBS_DIR to that build's OUT_DIR
# (a directory, an artifact download, an NFS mount -- anything readable).
debs_dir="${ARTIFACTS_DIR}/debs"
if [[ -n "${KUBEADM_DISTRO_DEBS_DIR:-}" ]]; then
  echo "staging prebuilt nke-kubeadm-distro debs from ${KUBEADM_DISTRO_DEBS_DIR}"
  if [[ ! -f "${KUBEADM_DISTRO_DEBS_DIR}/Packages" ]]; then
    echo "no Packages index in ${KUBEADM_DISTRO_DEBS_DIR}" >&2
    echo "  it must be the OUT_DIR of 'make -C os-image/packages/kubeadm-distro debs'" >&2
    exit 1
  fi
  shopt -s nullglob
  prebuilt=("${KUBEADM_DISTRO_DEBS_DIR}"/nke-kubeadm-distro-*_amd64.deb)
  shopt -u nullglob
  if [[ ${#prebuilt[@]} -eq 0 ]]; then
    echo "no nke-kubeadm-distro-*_amd64.deb in ${KUBEADM_DISTRO_DEBS_DIR}" >&2
    exit 1
  fi
  mkdir -p "${debs_dir}"
  install -m 0644 "${prebuilt[@]}" "${debs_dir}/"
  install -m 0644 "${KUBEADM_DISTRO_DEBS_DIR}/Packages" "${debs_dir}/Packages"
  # The index and the debs come from the same directory but not necessarily
  # from the same build: a stale Packages describes debs that are no longer
  # here, and apt on the node would then install nothing or the wrong digest.
  # Every staged deb has to be named by the index it was copied in with.
  for deb in "${prebuilt[@]}"; do
    if ! grep -qF "$(basename "${deb}")" "${debs_dir}/Packages"; then
      echo "$(basename "${deb}") is not listed in ${KUBEADM_DISTRO_DEBS_DIR}/Packages" >&2
      echo "  the index does not describe these debs; rebuild both together" >&2
      exit 1
    fi
  done
  # The bake log is the only provenance record for what went into the image,
  # so record what was actually staged, not just how many.
  echo "staged ${#prebuilt[@]} deb(s):"
  (cd "${debs_dir}" && sha256sum ./*.deb)
else
  # No prebuilt debs given, so build them here. K8S_VERSIONS passes through if
  # set; the target pins its own default otherwise. The target's own check-nix
  # fails with a clear message before downloading anything if nix is missing.
  echo "building nke-kubeadm-distro debs (${repo_root}/os-image/packages/kubeadm-distro)"
  echo "  (set KUBEADM_DISTRO_DEBS_DIR to use debs built on another host instead)"
  kubeadm_distro_args=(OUT_DIR="${debs_dir}")
  if [[ -n "${K8S_VERSIONS:-}" ]]; then
    kubeadm_distro_args+=(K8S_VERSIONS="${K8S_VERSIONS}")
  fi
  make -C "${repo_root}/os-image/packages/kubeadm-distro" debs "${kubeadm_distro_args[@]}"
fi

spire_tar="${ARTIFACTS_DIR}/${SPIRE_TARBALL}"
echo "fetching ${SPIRE_URL}"
curl --fail --silent --show-error --location \
  --connect-timeout 10 --max-time 60 \
  --retry 3 --retry-delay 5 --retry-connrefused \
  -o "${spire_tar}" "${SPIRE_URL}"
printf '%s  %s\n' "${SPIRE_SHA256}" "${spire_tar}" | sha256sum -c -

extract="${ARTIFACTS_DIR}/spire-extract"
rm -rf "${extract}"
mkdir -p "${extract}"
tar -xzf "${spire_tar}" -C "${extract}"
spire_bin="$(find "${extract}" -type f -name spire-agent -print -quit)"
if [[ -z "${spire_bin}" ]]; then
  echo "spire tarball did not contain spire-agent" >&2
  exit 1
fi
install -m 0755 "${spire_bin}" "${ARTIFACTS_DIR}/spire-agent"

files_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../files" && pwd)"
install -m 0644 "${files_dir}/nke-agent.service" "${ARTIFACTS_DIR}/nke-agent.service"
install -m 0644 "${files_dir}/spire-agent.service" "${ARTIFACTS_DIR}/spire-agent.service"
install -m 0644 "${files_dir}/nke-spire-configure.service" "${ARTIFACTS_DIR}/nke-spire-configure.service"
install -m 0644 "${files_dir}/spire-agent.conf.tmpl" "${ARTIFACTS_DIR}/spire-agent.conf.tmpl"
install -m 0755 "${files_dir}/spire-configure.sh" "${ARTIFACTS_DIR}/spire-configure.sh"
install -m 0755 "${files_dir}/wait-spiffe-socket.sh" "${ARTIFACTS_DIR}/wait-spiffe-socket.sh"
