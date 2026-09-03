#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Download, verify and stage the upstream Kubernetes node stack for one
# release, so packaging/build-deb.sh can turn it into an
# nke-kubeadm-distro-<x.y.z> .deb.
#
# Usage:
#   bundle.sh [--write-lock] --version vX.Y.Z --dist DIR --lock-dir DIR \
#             --crane PATH [--arch amd64]
#
# Default mode builds DIR/vX.Y.Z/ and enforces lock-dir/vX.Y.Z.lock.
# --write-lock resolves everything from upstream and rewrites that lock
# instead; it never stages a tree.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# URL bases, overridable so the tests can point every fetch at a local server.
# Nothing but the tests should ever set these.
DL_K8S_URL="${BUNDLE_DL_K8S_URL:-https://dl.k8s.io}"
GITHUB_URL="${BUNDLE_GITHUB_URL:-https://github.com}"
HELM_URL="${BUNDLE_HELM_URL:-https://get.helm.sh}"
RUNC_KEYRING="${BUNDLE_RUNC_KEYRING:-${SCRIPT_DIR}/packaging/keyrings/runc.keyring}"

# A single pin for every row; the CNI plugins are not skewed against the
# Kubernetes minor.
CNI_PLUGINS_VERSION=v1.6.0

COMPONENTS=(cni-plugins containerd crictl helm kubeadm kubectl kubelet runc)

die() { echo "bundle.sh: $*" >&2; exit 1; }

# pins_for sets CONTAINERD/RUNC/HELM for a Kubernetes minor.
#
# containerd follows the project's "Kubernetes support" matrix, pinned to the
# newest patch published on the line it lists:
# https://github.com/containerd/containerd/blob/main/RELEASES.md#kubernetes-support
#
# runc is not enumerated there, so these are pragmatic pairings with each
# containerd line, from https://github.com/opencontainers/runc/releases.
#
# Helm follows https://helm.sh/docs/v3/topics/version_skew/, newest patch on
# each supported line. Kubernetes 1.36 uses Helm v4.x because the published
# skew table has not caught up.
#
# There is deliberately no fallback row. A Kubernetes minor with no entry here
# is an error, so rotating K8S_VERSIONS forces someone to read both upstream
# matrices rather than inheriting whatever the newest line happened to be.
pins_for() {
  case "$1" in
    36) CONTAINERD=v2.3.0;  RUNC=v1.4.2; HELM=v4.1.4  ;;
    35) CONTAINERD=v2.2.3;  RUNC=v1.3.5; HELM=v3.20.2 ;;
    34) CONTAINERD=v2.1.7;  RUNC=v1.3.5; HELM=v3.20.2 ;;
    33) CONTAINERD=v2.1.7;  RUNC=v1.2.9; HELM=v3.20.2 ;;
    32) CONTAINERD=v2.1.7;  RUNC=v1.2.9; HELM=v3.20.2 ;;
    31) CONTAINERD=v1.7.31; RUNC=v1.2.9; HELM=v3.19.5 ;;
    30) CONTAINERD=v1.7.31; RUNC=v1.2.9; HELM=v3.18.6 ;;
    29) CONTAINERD=v1.7.31; RUNC=v1.2.9; HELM=v3.17.4 ;;
    *) die "no containerd, runc and helm pins for Kubernetes 1.$1: add a row to pins_for in bundle.sh, choosing containerd from its RELEASES.md Kubernetes support matrix and helm from its version skew policy" ;;
  esac
}

component_url() {
  case "$1" in
    kubeadm|kubectl|kubelet) echo "${DL_K8S_URL}/release/${VERSION}/bin/linux/${ARCH}/$1" ;;
    helm) echo "${HELM_URL}/helm-${HELM}-linux-${ARCH}.tar.gz" ;;
    cni-plugins) echo "${GITHUB_URL}/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}/cni-plugins-linux-${ARCH}-${CNI_PLUGINS_VERSION}.tgz" ;;
    crictl) echo "${GITHUB_URL}/kubernetes-sigs/cri-tools/releases/download/${CRI_TOOLS}/crictl-${CRI_TOOLS}-linux-${ARCH}.tar.gz" ;;
    containerd) echo "${GITHUB_URL}/containerd/containerd/releases/download/${CONTAINERD}/containerd-${CONTAINERD#v}-linux-${ARCH}.tar.gz" ;;
    runc) echo "${GITHUB_URL}/opencontainers/runc/releases/download/${RUNC}/runc.${ARCH}" ;;
    *) die "no URL for component $1" ;;
  esac
}

# fetch downloads url to a scratch path and moves it into place only once the
# caller is happy, so a partial body never appears under the artifact's name.
fetch() {
  curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 --max-time 600 -o "$2" "$1" ||
    die "download $1 failed"
}

file_sha() { sha256sum "$1" | cut -d' ' -f1; }

# checksum_from_doc reads the publisher's digest for one basename out of a
# checksum document: a GNU `sha256sum` list, or a single bare hex digest.
checksum_from_doc() {
  awk -v want="$2" '
    $1 ~ /^[0-9a-fA-F]{64}$/ {
      if (NF == 1) { bare = tolower($1); next }
      name = $2; sub(/^\*/, "", name); sub(/.*\//, "", name)
      if (name == want) matched = tolower($1)
    }
    END { if (matched != "") print matched; else print bare }
  ' "$1"
}

# verify_runc_doc checks runc's clearsigned checksum list against the bundled
# upstream keyring and writes its clear text to $2. gpg runs in a scratch
# GNUPGHOME that never sees the caller's own keys: --keyring is ignored
# outright by a homedir with use-keyboxd set, so importing into a throwaway
# home is the only way to be sure which keys were consulted.
verify_runc_doc() {
  [[ -f "$RUNC_KEYRING" ]] || die "runc keyring missing at ${RUNC_KEYRING}"
  GPG_HOME="$(mktemp -d)"
  chmod 0700 "$GPG_HOME"
  gpg --homedir "$GPG_HOME" --batch --quiet --import "$RUNC_KEYRING" ||
    die "importing ${RUNC_KEYRING} failed"
  local status="${WORK}/gpg-status"
  gpg --homedir "$GPG_HOME" --batch --quiet --status-fd 3 --verify "$1" 3>"$status" >/dev/null 2>&1 || true
  local rejected
  rejected="$(grep -E '^\[GNUPG:\] (EXPKEYSIG|REVKEYSIG|EXPSIG|BADSIG|NO_PUBKEY|ERRSIG)' "$status" || true)"
  [[ -z "$rejected" ]] || die "runc checksum signature rejected: ${rejected}"
  grep -q '^\[GNUPG:\] GOODSIG ' "$status" || die "runc checksum document has no GOODSIG; refusing to trust it"
  grep -q '^\[GNUPG:\] VALIDSIG ' "$status" || die "runc checksum document has no VALIDSIG; refusing to trust it"
  gpg --homedir "$GPG_HOME" --batch --quiet --yes --decrypt --output "$2" "$1" 2>/dev/null ||
    die "extracting the signed text of $1 failed"
}

# publisher_sha returns the digest the component's publisher advertises for
# file $2. runc's list is OpenPGP clearsigned; an unsigned one is a downgrade.
publisher_sha() {
  local component="$1" file="$2" url doc="${WORK}/checksum-doc"
  url="$(component_url "$component")"
  case "$component" in
    containerd) fetch "${url}.sha256sum" "$doc" ;;
    runc)
      fetch "${url%/*}/runc.sha256sum" "${doc}.asc"
      grep -q -- '-----BEGIN PGP SIGNED MESSAGE-----' "${doc}.asc" ||
        die "runc checksum document is not OpenPGP clearsigned; refusing to trust it"
      verify_runc_doc "${doc}.asc" "$doc"
      ;;
    *) fetch "${url}.sha256" "$doc" ;;
  esac
  local sum
  sum="$(checksum_from_doc "$doc" "$file")"
  [[ "$sum" =~ ^[0-9a-f]{64}$ ]] || die "no SHA-256 digest for ${file} in the checksum document for ${component}"
  echo "$sum"
}

# lock_field prints field $2 of the "artifact <component> <sha256> <url>"
# record for component $1.
lock_field() { awk -v c="$1" -v f="$2" '$1 == "artifact" && $2 == c { print $f }' "$LOCK_FILE"; }

# download_component fetches one artifact unless the cache already holds the
# bytes the lock pins, and verifies whatever it ends up with against the
# publisher's checksum and (in build mode) against the lock.
download_component() {
  local component="$1" url file dest tmp locked="" got published
  url="$(component_url "$component")"
  file="${url##*/}"
  dest="${ARCH_DIR}/${file}"
  if ((!WRITE_LOCK)); then
    locked="$(lock_field "$component" 3)"
    [[ -n "$locked" ]] || die "${component} is not in ${LOCK_FILE}: run make lock and review the diff"
    [[ "$(lock_field "$component" 4)" == "$url" ]] ||
      die "${component}: the pins would download ${url} but ${LOCK_FILE} has $(lock_field "$component" 4): run make lock and review the diff"
    if [[ -f "$dest" && "$(file_sha "$dest")" == "$locked" ]]; then
      ARTIFACT_SHA="$locked"
      return
    fi
  fi
  tmp="${WORK}/${file}"
  fetch "$url" "$tmp"
  got="$(file_sha "$tmp")"
  published="$(publisher_sha "$component" "$file")"
  if [[ "$got" != "$published" ]]; then
    rm -f "$tmp"
    die "${component}: ${file} hashes to ${got}, but the publisher's checksum for ${url} is ${published}"
  fi
  if ((!WRITE_LOCK)) && [[ "$got" != "$locked" ]]; then
    rm -f "$tmp"
    die "${component}: downloaded ${got} but ${LOCK_FILE} has ${locked}; upstream changed the bytes behind ${url}"
  fi
  mv "$tmp" "$dest"
  case "$component" in kubeadm|kubectl|kubelet|runc) chmod 0755 "$dest" ;; esac
  ARTIFACT_SHA="$got"
}

# resolve_images lists the control-plane images with the verified kubeadm and
# pins each to its linux/<arch> manifest digest, as sorted "image <ref> <digest>"
# records.
resolve_images() {
  local ref digest
  "${ARCH_DIR}/kubeadm" config images list --kubernetes-version "$VERSION" |
    awk 'NF == 1 && ($0 ~ /:/ || $0 ~ /@/) { print }' >"${WORK}/refs"
  [[ -s "${WORK}/refs" ]] || die "kubeadm config images list returned no images"
  : >"${WORK}/resolved"
  while read -r ref; do
    digest="$("$CRANE" digest --platform "linux/${ARCH}" "$ref")" ||
      die "crane digest ${ref} failed"
    [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die "crane digest ${ref} returned ${digest}"
    echo "image ${ref} ${digest}" >>"${WORK}/resolved"
  done <"${WORK}/refs"
  LC_ALL=C sort "${WORK}/resolved" >"${WORK}/images"
}

# write_images_tarball pulls every pinned image into the single gzipped
# docker-archive postinst feeds to `ctr images import`, plus the GNU-format
# sidecar postinst runs `sha256sum -c` on. The pull is skipped when the
# archive on disk was built from exactly this image list.
write_images_tarball() {
  local tgz="${ARCH_DIR}/k8s-base-container-images_${VERSION#v}_linux_${ARCH}.tgz"
  local digests="${tgz}.lock-digests" refs=() ref digest
  if [[ -f "$tgz" ]] && cmp -s "$digests" "${WORK}/images"; then
    return
  fi
  rm -f "$tgz" "${tgz}.sha256" "$digests"
  while read -r _ ref digest; do
    refs+=("${ref%:*}@${digest}")
  done <"${WORK}/images"
  "$CRANE" pull --platform "linux/${ARCH}" "${refs[@]}" "${WORK}/images.tar" ||
    die "crane pull failed"
  # crane writes the archive's members in Go map order, so a second pull of
  # the same images is not byte-identical. Repack the extracted tree instead
  # of teaching this script to merge image tarballs itself.
  mkdir -p "${WORK}/unpacked"
  tar -xf "${WORK}/images.tar" -C "${WORK}/unpacked"
  # crane pulled by digest, so it names each image "<repo>:i-was-a-digest" in
  # the archive's manifest.json. kubelet looks images up by the tag kubeadm
  # asked for, so put the real tags back before `ctr images import` sees them.
  # A crane that stops using that placeholder must fail here rather than ship
  # a package whose images import under a name nothing looks up.
  while read -r _ ref _; do
    grep -q "\"${ref%:*}:i-was-a-digest\"" "${WORK}/unpacked/manifest.json" ||
      die "crane did not name ${ref} as expected in the pulled archive; it would import under the wrong name"
    sed -i "s|\"${ref%:*}:i-was-a-digest\"|\"${ref}\"|g" "${WORK}/unpacked/manifest.json"
  done <"${WORK}/images"
  (cd "${WORK}/unpacked" && tar --sort=name --mtime='2026-01-01 00:00Z' \
    --owner=0 --group=0 --numeric-owner -cf "${WORK}/repacked.tar" -- *)
  gzip -n -9 -c "${WORK}/repacked.tar" >"${WORK}/images.tgz"
  mv "${WORK}/images.tgz" "$tgz"
  (cd "${ARCH_DIR}" && sha256sum "${tgz##*/}" >"${tgz##*/}.sha256")
  cp "${WORK}/images" "$digests"
}

# stage_tree lays out everything nfpm.yaml installs beyond the bare
# kubeadm/kubectl/kubelet/runc binaries and the image archive.
stage_tree() {
  local stage="${DIST}/${VERSION}/stage" pause
  # config.toml below is config version 3, which only containerd 2.x parses;
  # 1.x wants version 2 and a different plugin-id shape entirely.
  [[ "$CONTAINERD" == v2.* ]] ||
    die "containerd pin ${CONTAINERD} is below v2.0.0: config version 3 needs containerd 2.x"
  rm -rf "$stage"
  mkdir -p "${stage}/usr/local/bin" "${stage}/usr/bin" "${stage}/opt/cni/bin" "${stage}/etc/containerd"

  tar -xzf "${ARCH_DIR}/containerd-${CONTAINERD#v}-linux-${ARCH}.tar.gz" \
    -C "${stage}/usr/local/bin" --strip-components=1 \
    bin/containerd bin/containerd-shim-runc-v2 bin/ctr
  tar -xzf "${ARCH_DIR}/cni-plugins-linux-${ARCH}-${CNI_PLUGINS_VERSION}.tgz" -C "${stage}/opt/cni/bin"
  [[ -n "$(find "${stage}/opt/cni/bin" -mindepth 1 -maxdepth 1)" ]] ||
    die "the cni-plugins archive extracted no files"
  tar -xzf "${ARCH_DIR}/crictl-${CRI_TOOLS}-linux-${ARCH}.tar.gz" -C "${stage}/usr/bin" crictl
  tar -xzf "${ARCH_DIR}/helm-${HELM}-linux-${ARCH}.tar.gz" -C "${WORK}" "linux-${ARCH}/helm"
  mv "${WORK}/linux-${ARCH}/helm" "${stage}/usr/bin/helm"
  chmod -R 0755 "${stage}/usr/local/bin" "${stage}/usr/bin" "${stage}/opt/cni/bin"

  # The sandbox image is the pause image this release asks for, read back from
  # the lock so it can never skew from the bundled images.
  pause="$(awk '$1 == "image" && $2 ~ /\/pause:/ { print $2 }' "$LOCK_FILE" | LC_ALL=C sort | head -n1)"
  [[ -n "$pause" ]] || die "no pause image in ${LOCK_FILE}; containerd has no sandbox image to pin"
  cat >"${stage}/etc/containerd/config.toml" <<EOF
# Managed by nke-kubeadm-distro. Rendered at package build time from the
# locked image list; the sandbox image is the pause image this Kubernetes
# release asks for, so it can never skew from the bundled images. This file
# is rewritten on every install -- put local overrides in
# /etc/containerd/conf.d/ instead of editing it directly.
imports = ['/etc/containerd/conf.d/*.toml']
version = 3

[plugins]
  [plugins.'io.containerd.cri.v1.images']
    [plugins.'io.containerd.cri.v1.images'.pinned_images]
      sandbox = '${pause}'
  [plugins.'io.containerd.cri.v1.runtime']
    [plugins.'io.containerd.cri.v1.runtime'.cni]
      bin_dir = '/opt/cni/bin'
      conf_dir = '/etc/cni/net.d'
    [plugins.'io.containerd.cri.v1.runtime'.containerd]
      default_runtime_name = 'runc'
      [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes]
        [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]
          runtime_type = 'io.containerd.runc.v2'
          [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc.options]
            BinaryName = '/usr/sbin/runc'
            SystemdCgroup = true
EOF
  cat >"${stage}/etc/crictl.yaml" <<'EOF'
runtime-endpoint: unix:///run/containerd/containerd.sock
image-endpoint: unix:///run/containerd/containerd.sock
timeout: 10
EOF
}

WRITE_LOCK=0
VERSION=""
DIST=""
LOCK_DIR=""
CRANE=""
ARCH=amd64
while (($#)); do
  case "$1" in
    --write-lock) WRITE_LOCK=1; shift ;;
    --version) VERSION="${2:-}"; shift 2 ;;
    --dist) DIST="${2:-}"; shift 2 ;;
    --lock-dir) LOCK_DIR="${2:-}"; shift 2 ;;
    --crane) CRANE="${2:-}"; shift 2 ;;
    --arch) ARCH="${2:-}"; shift 2 ;;
    *) die "unknown argument $1" ;;
  esac
done
[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "--version must be vX.Y.Z, got '${VERSION}'"
[[ -n "$DIST" && -n "$LOCK_DIR" && -n "$CRANE" ]] || die "--dist, --lock-dir and --crane are required"
# The lock is one file per Kubernetes version, not per architecture, so there
# is nowhere to record a second arch's digests yet.
[[ "$ARCH" == amd64 ]] || die "--arch ${ARCH}: the lock and the .deb are amd64-only for now"
[[ "$(uname -s)" == Linux ]] ||
  die "the container images are listed by the downloaded linux kubeadm binary, so this build host ($(uname -s)) cannot produce a bundle; run on Linux"
command -v "$CRANE" >/dev/null 2>&1 || die "crane not found at ${CRANE}"

MINOR="${VERSION#v}"
[[ "${MINOR%%.*}" == 1 ]] || die "only Kubernetes 1.x is supported, got ${VERSION}"
MINOR="${MINOR#*.}"
MINOR="${MINOR%%.*}"
pins_for "$MINOR"
CRI_TOOLS="v1.${MINOR}.0"

ARCH_DIR="${DIST}/${VERSION}/${ARCH}"
LOCK_FILE="${LOCK_DIR}/${VERSION}.lock"
mkdir -p "$ARCH_DIR" "$LOCK_DIR"
WORK="$(mktemp -d)"
GPG_HOME=""
cleanup() {
  [[ -z "$GPG_HOME" ]] || { gpgconf --homedir "$GPG_HOME" --kill all >/dev/null 2>&1 || true; }
  rm -rf "$WORK" "$GPG_HOME"
}
trap cleanup EXIT

if ((!WRITE_LOCK)) && [[ ! -f "$LOCK_FILE" ]]; then
  die "no lock for ${VERSION}: run make lock and review the diff (expected ${LOCK_FILE})"
fi

echo "bundle.sh: ${VERSION} (containerd ${CONTAINERD}, runc ${RUNC}, helm ${HELM}, cri-tools ${CRI_TOOLS})" >&2
: >"${WORK}/artifacts"
for component in "${COMPONENTS[@]}"; do
  ARTIFACT_SHA=""
  download_component "$component"
  echo "artifact ${component} ${ARTIFACT_SHA} $(component_url "$component")" >>"${WORK}/artifacts"
done

# Safe to execute now, and only now: kubeadm's bytes matched what was verified.
resolve_images

if ((WRITE_LOCK)); then
  {
    echo "# nke-kubeadm-distro lock for ${VERSION} (${ARCH}). Regenerate with \`make lock\`; review the diff before committing."
    LC_ALL=C sort "${WORK}/artifacts" "${WORK}/images"
  } >"${WORK}/lock"
  if [[ -f "$LOCK_FILE" ]]; then
    diff -- "$LOCK_FILE" "${WORK}/lock" | grep -E '^[<>]' || echo "  (unchanged)"
  else
    echo "  new lock file"
  fi
  mv "${WORK}/lock" "$LOCK_FILE"
  echo "bundle.sh: wrote ${LOCK_FILE}" >&2
  exit 0
fi

if ! diff -q <(grep '^image ' "$LOCK_FILE") "${WORK}/images" >/dev/null; then
  diff -- <(grep '^image ' "$LOCK_FILE") "${WORK}/images" >&2 || true
  die "kubeadm's images do not match ${LOCK_FILE}: run make lock and review the diff"
fi
write_images_tarball
stage_tree
echo "bundle.sh: staged ${DIST}/${VERSION}" >&2
