#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Build one nke-kubeadm-distro-<x.y.z>_<rev>_amd64.deb from a bundle tree written
# by kubeadm-bundle.
#
# The Kubernetes patch is part of the package name; the Debian Version field is
# only the packaging revision. Paths follow the pkgs.k8s.io layout:
# /usr/bin/{kubeadm,kubectl,kubelet}, /usr/sbin/runc, the kubelet unit and its
# kubeadm drop-in under /lib/systemd/system, and the bundle itself under
# /usr/lib/nke/k8s-matrix/<version>/.
#
# Usage:
#   build-deb.sh <k8s-version> <bundle-dir> <installer-binary> <out-dir>
#
# Environment:
#   DEB_REVISION   Debian packaging revision (default 1).
#   PEER_VERSIONS  Other Kubernetes versions in this build, space- or
#                  comma-separated. They become Conflicts entries so apt cannot
#                  install two matrix rows at once.
set -euo pipefail

ARCH=amd64
PACKAGE_PREFIX=nke-kubeadm-distro
MATRIX_SUBDIR=k8s-matrix

usage() {
  echo "usage: $(basename "$0") <k8s-version> <bundle-dir> <installer-binary> <out-dir>" >&2
  exit 2
}

[[ $# -eq 4 ]] || usage
K8S_VER="$1"
SRC="$2"
INSTALLER_BIN="$3"
OUT_DIR="$4"
DEB_REVISION="${DEB_REVISION:-1}"

if ! command -v dpkg-deb >/dev/null 2>&1; then
  echo "build-deb.sh: dpkg-deb not found (apt install dpkg-dev)" >&2
  exit 1
fi

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

# sha256sum is GNU coreutils; macOS only has shasum. The .deb is built on Linux,
# but the packaging test may run anywhere dpkg-deb exists.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# Every matrix row owns /usr/bin/kubeadm, so each package conflicts with its peers.
PEER_CONFLICTS="${PACKAGE_PREFIX}"
for peer in ${PEER_VERSIONS//,/ }; do
  peer="${peer#v}"
  [[ "${peer}" == "${K8S_PATCH}" ]] && continue
  PEER_CONFLICTS="${PEER_CONFLICTS}, ${PACKAGE_PREFIX}-${peer}"
done

STAGE="$(mktemp -d "${TMPDIR:-/tmp}/nke-kubeadm-distro-XXXXXX")"
trap 'rm -rf "${STAGE}"' EXIT
# mktemp -d creates 0700, and dpkg-deb records the stage root as the package's
# own "./" entry: without this the installed package root would be drwx------.
chmod 0755 "${STAGE}"

mkdir -p "${STAGE}/DEBIAN"

LIBMATRIX="${STAGE}/usr/lib/nke/${MATRIX_SUBDIR}/${K8S_VER}"
mkdir -p "${LIBMATRIX}/${ARCH}"

install -m 0644 "${SRC}/manifest.json" "${LIBMATRIX}/manifest.json"
install -m 0644 "${SRC}/manifest.json.sha256" "${LIBMATRIX}/manifest.json.sha256"

install -m 0755 "${INSTALLER_BIN}" "${LIBMATRIX}/installer"
printf '%s  installer\n' "$(sha256_of "${LIBMATRIX}/installer")" >"${LIBMATRIX}/installer.sha256"
chmod 0644 "${LIBMATRIX}/installer.sha256"

cp -a "${SRC}/${ARCH}/." "${LIBMATRIX}/${ARCH}/"

MARCH="${LIBMATRIX}/${ARCH}"
mkdir -p "${STAGE}/usr/bin" "${STAGE}/usr/sbin"
for binary in kubeadm kubectl kubelet; do
  chmod 0755 "${MARCH}/${binary}"
  install -m 0755 "${MARCH}/${binary}" "${STAGE}/usr/bin/${binary}"
done
chmod 0755 "${MARCH}/runc.${ARCH}"
install -m 0755 "${MARCH}/runc.${ARCH}" "${STAGE}/usr/sbin/runc"

# ../lib, not ../../lib: from /usr/bin one level up is /usr, and two is /.
ln -sf "../lib/nke/${MATRIX_SUBDIR}/${K8S_VER}/installer" "${STAGE}/usr/bin/nke-k8s-installer"

# systemd: the vendor kubelet unit plus the kubeadm drop-in, same split as the
# upstream kubelet and kubeadm packages.
mkdir -p "${STAGE}/lib/systemd/system/kubelet.service.d"
install -m 0644 "${PACKAGING_DIR}/systemd/kubelet.service" "${STAGE}/lib/systemd/system/kubelet.service"
install -m 0644 "${PACKAGING_DIR}/systemd/10-kubeadm.conf" "${STAGE}/lib/systemd/system/kubelet.service.d/10-kubeadm.conf"

# Placeholders the upstream kubelet package also ships.
# The directories ship empty on purpose: kubeadm's DirAvailable preflight check
# reads one name and calls any entry, hidden or not, "not empty".
mkdir -p "${STAGE}/etc/default" "${STAGE}/etc/kubernetes/manifests" "${STAGE}/var/lib/kubelet"
printf '%s\n' '# Optional kubelet flags; kubeadm uses /var/lib/kubelet/kubeadm-flags.env.' 'KUBELET_EXTRA_ARGS=' \
  >"${STAGE}/etc/default/kubelet"
chmod 0644 "${STAGE}/etc/default/kubelet"

mkdir -p "${STAGE}/usr/share/doc/${PACKAGE_NAME}"
printf '%s\n' "Bundled Kubernetes ${K8S_VER} node stack (NKE)." \
  >"${STAGE}/usr/share/doc/${PACKAGE_NAME}/README.Debian"

# /etc/default/kubelet carries operator KUBELET_EXTRA_ARGS. As a conffile, dpkg
# keeps local edits when the node moves between matrix rows instead of silently
# restoring the packaged default.
printf '%s\n' /etc/default/kubelet >"${STAGE}/DEBIAN/conffiles"
chmod 0644 "${STAGE}/DEBIAN/conffiles"

# Templating: {{PLACEHOLDER}} substitution with sed, so packaging needs no python3.
render() {
  sed \
    -e "s|{{PACKAGE_NAME}}|${PACKAGE_NAME}|g" \
    -e "s|{{DEB_REVISION}}|${DEB_REVISION}|g" \
    -e "s|{{ARCH}}|${ARCH}|g" \
    -e "s|{{K8S_VER}}|${K8S_VER}|g" \
    -e "s|{{MATRIX_SUBDIR}}|${MATRIX_SUBDIR}|g" \
    -e "s|{{VIRTUAL_PROVIDER}}|${PACKAGE_PREFIX}|g" \
    -e "s|{{PEER_CONFLICTS}}|${PEER_CONFLICTS}|g" \
    -e "s|{{INSTALLED_SIZE}}|${INSTALLED_SIZE:-0}|g" \
    "$1" >"$2"
}

for f in postinst prerm postrm; do
  render "${PACKAGING_DIR}/${f}" "${STAGE}/DEBIAN/${f}"
done
chmod 0755 "${STAGE}/DEBIAN/postinst" "${STAGE}/DEBIAN/prerm" "${STAGE}/DEBIAN/postrm"

# Installed-Size is the payload in KiB, excluding DEBIAN/. It is optional in the
# format but apt uses it for its disk-space check, and this package is ~1 GB.
stage_kib="$(du -sk "${STAGE}" | awk '{print $1}')"
control_kib="$(du -sk "${STAGE}/DEBIAN" | awk '{print $1}')"
INSTALLED_SIZE=$((stage_kib - control_kib))

render "${PACKAGING_DIR}/control" "${STAGE}/DEBIAN/control"
chmod 0644 "${STAGE}/DEBIAN/control"

mkdir -p "${OUT_DIR}"
DEB="${OUT_DIR}/${PACKAGE_NAME}_${DEB_REVISION}_${ARCH}.deb"
# --root-owner-group instead of fakeroot: the staged tree is owned by the build
# user, and dpkg-deb can record root:root without a fakeroot environment.
dpkg-deb --root-owner-group --build "${STAGE}" "${DEB}" >/dev/null
echo "wrote ${DEB}"
