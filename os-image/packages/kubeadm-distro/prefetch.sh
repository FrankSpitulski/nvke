#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Regenerate versions.json: the committed lock every deb is built from.
#
# For each Kubernetes version it derives the upstream URLs from the pin tables
# below, takes each artifact's sha256 from the *publisher's own* checksum
# document rather than from whatever happened to download, runs the verified
# kubeadm to ask which control-plane images that release needs, resolves each
# tag to its linux/amd64 manifest digest, and records the nix output hash of
# every image so dockerTools.pullImage can fetch it as a fixed-output
# derivation.
#
# Usage:
#   ./prefetch.sh <output-file> <k8s-version>...
#
# Linux only: it executes the downloaded linux/amd64 kubeadm binary.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Re-exec under the same nixpkgs the build uses, so `make lock` and `make debs`
# cannot disagree about which skopeo resolved a digest.
if [ "${NKE_PREFETCH_TOOLS:-}" != "1" ]; then
  if ! command -v nix >/dev/null 2>&1; then
    echo "prefetch.sh: nix is required; install it from https://nixos.org/download (the standard installer, not Determinate)." >&2
    exit 1
  fi
  export NKE_PREFETCH_TOOLS=1
  exec nix --extra-experimental-features 'nix-command flakes' \
    shell "path:${HERE}#prefetch-tools" --command bash "${BASH_SOURCE[0]}" "$@"
fi

if [ "$(uname -s)" != "Linux" ]; then
  echo "prefetch.sh: this runs the downloaded linux/amd64 kubeadm binary, so it needs a Linux host; this is $(uname -s)." >&2
  exit 1
fi

if [ "$#" -lt 2 ]; then
  echo "usage: $(basename "$0") <output-file> <k8s-version>..." >&2
  exit 2
fi

OUT="$1"
shift

for candidate in "${SSL_CERT_FILE:-}" "${NIX_SSL_CERT_FILE:-}" \
  /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-bundle.crt; do
  if [ -n "${candidate}" ] && [ -f "${candidate}" ]; then
    export SSL_CERT_FILE="${candidate}"
    break
  fi
done

# --- pin tables ------------------------------------------------------------
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
#
# containerd 2.x only. The rendered /etc/containerd/config.toml is version 3,
# which containerd 1.7 cannot read, and deb.nix has no 1.7 path, so a row
# pinning a 1.7 release would build a package that cannot start containerd.
declare -A CONTAINERD_PIN=(
  [36]=v2.3.0 [35]=v2.2.3 [34]=v2.1.7 [33]=v2.1.7 [32]=v2.1.7
)
declare -A RUNC_PIN=(
  [36]=v1.4.2 [35]=v1.3.5 [34]=v1.3.5 [33]=v1.2.9 [32]=v1.2.9
)
declare -A HELM_PIN=(
  [36]=v4.1.4 [35]=v3.20.2 [34]=v3.20.2 [33]=v3.20.2 [32]=v3.20.2
)
# The CNI plugins are not skewed against the Kubernetes minor, so one pin.
CNI_PLUGINS_PIN=v1.6.0

# The committed OpenPGP keyring holding the runc release maintainers' keys.
RUNC_KEYRING="${HERE}/keyrings/runc-release.gpg"
if [ ! -f "${RUNC_KEYRING}" ]; then
  echo "prefetch.sh: missing ${RUNC_KEYRING}; runc's signed checksum list cannot be verified." >&2
  exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

fetch() {
  curl --fail --silent --show-error --location \
    --connect-timeout 10 --max-time 900 \
    --retry 3 --retry-delay 5 --retry-connrefused "$@"
}

# sha256_from_doc <checksum-document> <filename>
#
# Publishers disagree on the format: dl.k8s.io writes a bare hex digest,
# containerd and the CNI plugins write `sha256sum` lines, and runc writes an
# OpenPGP clearsigned `sha256sum` list. Prefer the line naming the file we
# want; fall back to a lone digest.
sha256_from_doc() {
  local doc="$1" want="$2" line
  line="$(awk -v want="${want}" '$2 ~ ("(^|/)" want "$") { print $1; exit }' "${doc}" || true)"
  if [ -z "${line}" ]; then
    line="$(grep -Eo '\b[0-9a-f]{64}\b' "${doc}" | head -n1 || true)"
  fi
  if [ -z "${line}" ]; then
    echo "prefetch.sh: no sha256 for ${want} in ${doc}" >&2
    return 1
  fi
  printf '%s' "${line}"
}

# verify_clearsigned <checksum-document> <keyring> <name>
#
# runc is the one publisher here that signs its checksum list, so it is the one
# place a signature can be checked at all: the others publish a bare `.sha256`
# next to the artifact, where an attacker who can rewrite one can rewrite both.
# Verifying the clearsigned document against the committed release keyring makes
# runc's sha256 in the lock an upstream *signed* statement.
verify_clearsigned() {
  local doc="$1" keyring="$2" name="$3"
  local gnupghome="${WORK}/gnupg"
  if ! grep -q '^-----BEGIN PGP SIGNED MESSAGE-----' "${doc}"; then
    echo "prefetch.sh: the ${name} checksum document is not clearsigned; refusing to parse it" >&2
    return 1
  fi
  mkdir -p "${gnupghome}"
  chmod 0700 "${gnupghome}"
  if ! gpg --batch --homedir "${gnupghome}" \
    --no-default-keyring --keyring "${keyring}" --verify "${doc}" >&2; then
    echo "prefetch.sh: the ${name} checksum document does not verify against ${keyring}" >&2
    return 1
  fi
}

# artifact <name> <url> <checksum-url> [<gpg-keyring>]
# -> writes "<local-path> <sha256>"
artifact() {
  local name="$1" url="$2" sum_url="$3" keyring="${4:-}"
  local file="${WORK}/${name}.artifact" doc="${WORK}/${name}.sums" want got
  echo "  ${name}: ${url}" >&2
  fetch -o "${file}" "${url}"
  fetch -o "${doc}" "${sum_url}"
  if [ -n "${keyring}" ]; then
    verify_clearsigned "${doc}" "${keyring}" "${name}"
  fi
  want="$(sha256_from_doc "${doc}" "$(basename "${url}")")"
  got="$(sha256sum "${file}" | cut -d' ' -f1)"
  if [ "${want}" != "${got}" ]; then
    echo "prefetch.sh: ${name} does not match the publisher's checksum (${want} vs ${got})" >&2
    return 1
  fi
  printf '%s %s' "${file}" "${got}"
}

# amd64_digest <ref> -> the linux/amd64 manifest digest, not the index digest.
amd64_digest() {
  local ref="$1" raw digest
  raw="$(skopeo inspect --raw "docker://${ref}")"
  digest="$(printf '%s' "${raw}" |
    jq -r '(.manifests // [])[]
           | select(.platform.os == "linux" and .platform.architecture == "amd64")
           | select((.platform.variant // "") == "")
           | .digest' | head -n1)"
  if [ -z "${digest}" ]; then
    # Not a manifest list: the image itself is the amd64 manifest.
    digest="$(skopeo inspect --format '{{.Digest}}' "docker://${ref}")"
  fi
  printf '%s' "${digest}"
}

versions_json="${WORK}/versions.json"
echo '{}' >"${versions_json}"

for raw_version in "$@"; do
  version="v${raw_version#v}"
  IFS=. read -r major minor _ <<<"${version#v}"
  if [ "${major}" != "1" ] || [ -z "${CONTAINERD_PIN[${minor}]:-}" ]; then
    echo "prefetch.sh: no containerd, runc and helm pins for Kubernetes ${major}.${minor}:" >&2
    echo "  add a row to the pin tables in prefetch.sh, choosing containerd from its RELEASES.md" >&2
    echo "  Kubernetes support matrix and helm from its version skew policy." >&2
    exit 1
  fi
  containerd="${CONTAINERD_PIN[${minor}]}"
  runc="${RUNC_PIN[${minor}]}"
  helm="${HELM_PIN[${minor}]}"
  # cri-tools only tags v1.MM.0.
  crictl="v${major}.${minor}.0"

  echo "==> ${version} (containerd ${containerd}, runc ${runc}, helm ${helm}, crictl ${crictl})" >&2

  declare -A ART_URL=() ART_SHA=() ART_PATH=()
  for binary in kubeadm kubectl kubelet; do
    url="https://dl.k8s.io/release/${version}/bin/linux/amd64/${binary}"
    result="$(artifact "${binary}" "${url}" "${url}.sha256")"
    read -r path sha <<<"${result}"
    ART_URL[${binary}]="${url}"
    ART_SHA[${binary}]="${sha}"
    ART_PATH[${binary}]="${path}"
  done

  url="https://github.com/containerd/containerd/releases/download/${containerd}/containerd-${containerd#v}-linux-amd64.tar.gz"
  result="$(artifact containerd "${url}" "${url}.sha256sum")"
  read -r path sha <<<"${result}"
  ART_URL[containerd]="${url}"
  ART_SHA[containerd]="${sha}"

  url="https://github.com/opencontainers/runc/releases/download/${runc}/runc.amd64"
  result="$(artifact runc "${url}" \
    "https://github.com/opencontainers/runc/releases/download/${runc}/runc.sha256sum" \
    "${RUNC_KEYRING}")"
  read -r path sha <<<"${result}"
  ART_URL[runc]="${url}"
  ART_SHA[runc]="${sha}"

  url="https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS_PIN}/cni-plugins-linux-amd64-${CNI_PLUGINS_PIN}.tgz"
  result="$(artifact cni-plugins "${url}" "${url}.sha256")"
  read -r path sha <<<"${result}"
  ART_URL[cni-plugins]="${url}"
  ART_SHA[cni-plugins]="${sha}"

  url="https://github.com/kubernetes-sigs/cri-tools/releases/download/${crictl}/crictl-${crictl}-linux-amd64.tar.gz"
  result="$(artifact crictl "${url}" "${url}.sha256")"
  read -r path sha <<<"${result}"
  ART_URL[crictl]="${url}"
  ART_SHA[crictl]="${sha}"

  url="https://get.helm.sh/helm-${helm}-linux-amd64.tar.gz"
  result="$(artifact helm "${url}" "${url}.sha256")"
  read -r path sha <<<"${result}"
  ART_URL[helm]="${url}"
  ART_SHA[helm]="${sha}"

  # kubeadm is verified against the publisher's checksum above, so this runs a
  # binary whose digest is about to be written into the lock and reviewed.
  kubeadm_bin="${ART_PATH[kubeadm]}"
  chmod 0755 "${kubeadm_bin}"
  echo "  running kubeadm config images list --kubernetes-version ${version}" >&2
  mapfile -t refs < <("${kubeadm_bin}" config images list --kubernetes-version "${version}" | sort)
  if [ "${#refs[@]}" -eq 0 ]; then
    echo "prefetch.sh: kubeadm listed no images for ${version}" >&2
    exit 1
  fi

  images_json="${WORK}/images-${version}.json"
  echo '[]' >"${images_json}"
  for ref in "${refs[@]}"; do
    [ -n "${ref}" ] || continue
    digest="$(amd64_digest "${ref}")"
    echo "  ${ref} -> ${digest}" >&2
    name="${ref%:*}"
    tag="${ref##*:}"
    # .hash, not .sha256: nix-prefetch-docker emits the SRI form, which is what
    # dockerTools.pullImage wants as its fixed-output hash.
    nix_hash="$(nix-prefetch-docker --json --quiet \
      --os linux --arch amd64 \
      --image-name "${name}" --image-digest "${digest}" \
      --final-image-name "${name}" --final-image-tag "${tag}" | jq -r '.hash')"
    if [ -z "${nix_hash}" ] || [ "${nix_hash}" = null ]; then
      echo "prefetch.sh: nix-prefetch-docker returned no hash for ${ref}" >&2
      exit 1
    fi
    jq --arg ref "${ref}" --arg digest "${digest}" --arg hash "${nix_hash}" \
      '. + [{ref: $ref, digest: $digest, hash: $hash}]' \
      "${images_json}" >"${images_json}.new"
    mv "${images_json}.new" "${images_json}"
  done

  artifacts_json="${WORK}/artifacts-${version}.json"
  echo '{}' >"${artifacts_json}"
  for name in "${!ART_URL[@]}"; do
    jq --arg n "${name}" --arg u "${ART_URL[${name}]}" --arg s "${ART_SHA[${name}]}" \
      '. + {($n): {url: $u, sha256: $s}}' \
      "${artifacts_json}" >"${artifacts_json}.new"
    mv "${artifacts_json}.new" "${artifacts_json}"
  done

  jq --arg v "${version}" \
    --slurpfile a "${artifacts_json}" \
    --slurpfile i "${images_json}" \
    --arg containerd "${containerd}" --arg runc "${runc}" --arg helm "${helm}" \
    --arg crictl "${crictl}" --arg cni "${CNI_PLUGINS_PIN}" \
    '. + {($v): {
       components: {
         kubernetes: $v,
         containerd: $containerd,
         runc: $runc,
         helm: $helm,
         crictl: $crictl,
         "cni-plugins": $cni
       },
       artifacts: $a[0],
       images: $i[0]
     }}' "${versions_json}" >"${versions_json}.new"
  mv "${versions_json}.new" "${versions_json}"
done

jq -S --indent 2 . "${versions_json}" >"${WORK}/canonical.json"

if [ -f "${OUT}" ]; then
  echo >&2
  echo "diff against the existing ${OUT}:" >&2
  if diff -u "${OUT}" "${WORK}/canonical.json" >&2; then
    echo "  (no change)" >&2
  fi
fi

cp "${WORK}/canonical.json" "${OUT}"
echo >&2
echo "wrote ${OUT}" >&2
