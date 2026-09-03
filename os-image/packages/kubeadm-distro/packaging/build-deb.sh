#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Build one nke-kubeadm-distro-<x.y.z>_<rev>_amd64.deb from a bundle tree
# written by kubeadm-bundle, using nfpm and packaging/nfpm.yaml.
#
# Usage:
#   build-deb.sh <k8s-version> <bundle-dir> <installer-binary> <out-dir>
#
# bundle-dir, installer-binary and out-dir may be given relative to the
# caller's cwd; they are canonicalised to absolute paths below. nfpm itself
# runs with PACKAGING_DIR as its cwd (see the `cd` before invoking it), so an
# uncanonicalised relative path would resolve against the wrong directory and
# fail with an opaque nfpm glob error instead of a clear one.
#
# Environment:
#   DEB_REVISION   Debian packaging revision (default 1).
#   PEER_VERSIONS  Other Kubernetes versions in this build, space- or
#                  comma-separated. They become Conflicts entries so apt cannot
#                  install two matrix rows at once.
#   NFPM_BIN       Path to the nfpm binary (default: nfpm on PATH).
set -euo pipefail

ARCH=amd64
PACKAGE_PREFIX=nke-kubeadm-distro
MATRIX_SUBDIR=k8s-matrix
NFPM_BIN="${NFPM_BIN:-nfpm}"

usage() {
  echo "usage: $(basename "$0") <k8s-version> <bundle-dir> <installer-binary> <out-dir>" >&2
  exit 2
}

[[ $# -eq 4 ]] || usage
K8S_VER="$1"
SRC="$(cd "$2" 2>/dev/null && pwd)" || { echo "build-deb.sh: no such bundle directory: $2" >&2; exit 1; }
INSTALLER_BIN_DIR="$(cd "$(dirname "$3")" 2>/dev/null && pwd)" || { echo "build-deb.sh: no such installer directory: $(dirname "$3")" >&2; exit 1; }
INSTALLER_BIN="${INSTALLER_BIN_DIR}/$(basename "$3")"
mkdir -p "$4"
OUT_DIR="$(cd "$4" && pwd)"
DEB_REVISION="${DEB_REVISION:-1}"

command -v "${NFPM_BIN}" >/dev/null 2>&1 || {
  echo "build-deb.sh: nfpm not found (${NFPM_BIN}); run 'make nfpm' or set NFPM_BIN" >&2
  exit 1
}

PACKAGING_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

[[ "${K8S_VER}" == v* ]] || K8S_VER="v${K8S_VER}"
K8S_PATCH="${K8S_VER#v}"
PACKAGE_NAME="${PACKAGE_PREFIX}-${K8S_PATCH}"

for required in "${SRC}/manifest.json" "${SRC}/manifest.json.sha256" "${INSTALLER_BIN}"; do
  if [[ ! -f "${required}" ]]; then
    echo "build-deb.sh: missing ${required}" >&2
    exit 1
  fi
done

# The container images are what makes a node come up without a registry. A
# bundle without them still builds, but only deliberately: postinst refuses to
# configure a node from such a package.
BASE_TGZ="${SRC}/${ARCH}/k8s-base-container-images_${K8S_PATCH}_linux_${ARCH}.tgz"
if [[ ! -f "${BASE_TGZ}" || ! -f "${BASE_TGZ}.sha256" ]]; then
  if [[ "${ALLOW_NO_IMAGES:-}" != "1" ]]; then
    echo "build-deb.sh: ${BASE_TGZ} and its .sha256 sidecar are missing." >&2
    echo "  The .deb would install without the control-plane images and could not bring up a node offline." >&2
    echo "  Rebuild the bundle without SKIP_IMAGES, or set ALLOW_NO_IMAGES=1 to build a development-only package." >&2
    exit 1
  fi
  echo "build-deb.sh: WARNING: building ${PACKAGE_NAME} without container images (ALLOW_NO_IMAGES=1)." >&2
  echo "  This package is DEVELOPMENT ONLY: its postinst will refuse to run on a node." >&2
fi

# sha256sum is GNU coreutils; macOS only has shasum. The .deb is built on
# Linux, but the packaging test may run anywhere nfpm exists.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# Every matrix row owns /usr/bin/kubeadm, so each package conflicts with its
# peers. A single comma-separated string: nfpm.yaml explains why.
PEER_CONFLICTS="${PACKAGE_PREFIX}"
for peer in ${PEER_VERSIONS//,/ }; do
  peer="${peer#v}"
  [[ "${peer}" == "${K8S_PATCH}" ]] && continue
  PEER_CONFLICTS="${PEER_CONFLICTS}, ${PACKAGE_PREFIX}-${peer}"
done

# Everything nfpm.yaml cannot take straight from SRC or INSTALLER_BIN: the
# installer checksum sidecar and README.Debian, both of which need runtime
# values nfpm's env expansion cannot compute on its own.
#
# TMPDIR_NO_SLASH: macOS sets TMPDIR with a trailing slash, which doubles the
# "/" before the mktemp suffix below; nfpm's globbing (used to resolve every
# `expand: true` content src) then fails to match that path at all.
TMPDIR_NO_SLASH="${TMPDIR:-/tmp}"
SCRATCH="$(mktemp -d "${TMPDIR_NO_SLASH%/}/nke-kubeadm-distro-XXXXXX")"
# SCRIPTS_DIR is not assigned until later (it needs PACKAGING_DIR); initialise
# it empty here so the trap does not fail under `set -u` if something between
# here and there exits first, which would otherwise skip the SCRATCH cleanup
# too and leak its ~1 GB copy of the arch directory.
SCRIPTS_DIR=""
trap 'rm -rf "${SCRATCH}" "${SCRIPTS_DIR}"' EXIT

printf '%s  installer\n' "$(sha256_of "${INSTALLER_BIN}")" >"${SCRATCH}/installer.sha256"
printf '%s\n' "Bundled Kubernetes ${K8S_VER} node stack (NKE)." >"${SCRATCH}/README.Debian"

# nfpm.yaml's top-level `mtime` does not reach files pulled in through a
# `type: tree` content entry (they otherwise keep kubeadm-bundle's download
# time), so the arch directory gets a touched copy here rather than being
# read from SRC directly -- otherwise two builds of the same locked bundle,
# downloaded at different times, would not produce byte-identical .debs.
mkdir -p "${SCRATCH}/${ARCH}"
cp -a "${SRC}/${ARCH}/." "${SCRATCH}/${ARCH}/"
(export TZ=UTC && find "${SCRATCH}/${ARCH}" -exec touch -t 202601010000 {} +)

# {{PLACEHOLDER}} substitution with sed, so packaging needs no python3.
render() {
  sed \
    -e "s|{{PACKAGE_NAME}}|${PACKAGE_NAME}|g" \
    -e "s|{{ARCH}}|${ARCH}|g" \
    -e "s|{{K8S_VER}}|${K8S_VER}|g" \
    -e "s|{{MATRIX_SUBDIR}}|${MATRIX_SUBDIR}|g" \
    "$1" >"$2"
}
# nfpm's `scripts:` fields get no env-var expansion at all (unlike `contents:`
# with `expand: true`), so nfpm.yaml points at this fixed, package-relative
# path instead of SCRATCH. Safe to overwrite on every run: build-deb.sh runs
# are never concurrent (the Makefile's `debs` loop is a plain sequential
# shell `for`).
SCRIPTS_DIR="${PACKAGING_DIR}/../bin/.nfpm-scripts"
rm -rf "${SCRIPTS_DIR}"
mkdir -p "${SCRIPTS_DIR}"
for f in postinst prerm postrm; do
  render "${PACKAGING_DIR}/${f}" "${SCRIPTS_DIR}/${f}"
done
chmod 0755 "${SCRIPTS_DIR}/postinst" "${SCRIPTS_DIR}/prerm" "${SCRIPTS_DIR}/postrm"

mkdir -p "${OUT_DIR}"
export PACKAGE_NAME DEB_REVISION ARCH K8S_VER K8S_PATCH MATRIX_SUBDIR PEER_CONFLICTS SRC INSTALLER_BIN SCRATCH

# cd into PACKAGING_DIR: nfpm.yaml's systemd/*.conf, kubelet.default and
# scripts entries are given as paths relative to it.
(cd "${PACKAGING_DIR}" && "${NFPM_BIN}" package -f nfpm.yaml -p deb -t "${OUT_DIR}")
echo "wrote ${OUT_DIR}/${PACKAGE_NAME}_${DEB_REVISION}_${ARCH}.deb"
