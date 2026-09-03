#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Smoke test: boot a qcow2 the non-FDE build produced and confirm it comes up
# clean (cloud-init completes, SSH answers, nke-agent and spire-agent units
# are installed and enabled). Does not exercise SPIRE/nke-agent against a real
# server -- just proves the image boots on its own.
#
# QCOW defaults to the single *.qcow2 under output-non-fde/ (its filename
# encodes the Ubuntu version/arch, e.g. nke-node-non-fde-ubuntu-26.04-amd64.qcow2
# -- see vm_name in nke-node-non-fde.pkr.hcl). This variant is UEFI-installed,
# so OVMF_CODE (and optionally OVMF_VARS_TEMPLATE) is required:
#   OVMF_CODE=/usr/share/OVMF/OVMF_CODE_4M.fd ./boot-smoke.sh
# The FDE variant needs its own passphrase/TPM handling instead of this
# script -- see ../../demo-fde.sh.
#
# Also proves every staged kubeadm-distro deb installs and runs fully
# offline (os-image/SPEC.md's "Staged, not installed" section): the guest's
# netdev is restrict=on, so only our own hostfwd SSH can reach it, and each
# staged version is installed and checked in turn. SKIP_OFFLINE_INSTALL=1
# skips this, for anyone booting an image built without staged debs.
#
# Each install/remove is checked against node-snapshot.sh's node-state
# contract (see its header): everything dpkg didn't already put on disk that
# a maintainer script wrote must be one of the explicit ALLOWLIST entries
# below (plus that version's own postinst log, see the loop). That allowlist
# is derived from postinst/postrm in packages/kubeadm-distro/packaging/ --
# see the comment on ALLOWLIST.
#
# This file can also be sourced (the BASH_SOURCE guard at the bottom is what
# makes that safe) to load its pure, ssh-free helper functions --
# snapshot_section/check_unowned_allowlist/images_equal/image_delta/
# strip_digest_refs/filter_stale_paths/sysctls_are_one -- for unit testing
# against fixture snapshot files, without booting a VM. See
# boot-smoke_test.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
QCOW="${QCOW:-}"
SSH_KEY="${SSH_KEY:-${ROOT}/.ssh/id_ed25519}"
SSH_PORT="${SSH_PORT:-2222}"
OVMF_CODE="${OVMF_CODE:-}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
BUILD_LOGS_DIR="${ROOT}/build-logs"

# Every path a kubeadm-distro install/remove may create or edit that dpkg
# does not already own (nfpm.yaml lists what dpkg owns; everything else here
# comes straight from packaging/postinst and packaging/postrm):
#   - /etc/sysctl.d/k8s.conf, /etc/modules-load.d/k8s.conf: postinst writes
#     these directly (`cat >...`, `printf ... >...`).
#   - /etc/fstab: postinst comments out swap lines in place with `sed -i`;
#     it is a pre-existing file, not a new one, but still shows up as
#     "changed since the marker".
#   - /etc/systemd/system/multi-user.target.wants/{containerd,kubelet}.service:
#     postinst's `systemctl enable containerd` / `systemctl enable kubelet`
#     create these symlinks (both units carry `WantedBy=multi-user.target`
#     in packaging/systemd/*.service); systemd, not postinst, writes the
#     symlink, but it is still unowned by dpkg.
# A per-version entry (that version's own postinst log under /var/log/) is
# appended in the install loop below, since its name isn't fixed.
ALLOWLIST=(
  /etc/sysctl.d/k8s.conf
  /etc/modules-load.d/k8s.conf
  /etc/fstab
  /etc/systemd/system/multi-user.target.wants/containerd.service
  /etc/systemd/system/multi-user.target.wants/kubelet.service
)

# ---------------------------------------------------------------------------
# Pure helpers: no ssh, no state beyond their arguments. Kept free of the
# rest of the script's side effects so they can be exercised directly by a
# host-side unit test (source this file, then call them against fixture
# snapshot text) -- see boot-smoke_test.sh.
# ---------------------------------------------------------------------------

# snapshot_section <snapshot-file> <section-name>
# Prints the lines between "=== <section-name> ===" and the next "=== ... ==="
# marker (or EOF), matching the section headers node-snapshot.sh prints.
snapshot_section() {
  local file="$1" name="$2"
  awk -v want="=== ${name} ===" '
    $0 == want { on=1; next }
    /^=== .* ===$/ { on=0 }
    on { print }
  ' "${file}"
}

# path_in_list <path> <list entries...>
path_in_list() {
  local needle="$1"
  shift
  local p
  for p in "$@"; do
    [[ "${needle}" == "${p}" ]] && return 0
  done
  return 1
}

# check_unowned_allowlist <snapshot-file> <allowlist entries...>
# Prints each unowned changed-files entry that is not in the allowlist, one
# per line, and returns non-zero if there were any.
check_unowned_allowlist() {
  local snapshot="$1"
  shift
  local violations=()
  local line path
  while IFS= read -r line; do
    [[ -z "${line}" ]] && continue
    [[ "${line}" == unowned\ * ]] || continue
    path="${line#unowned }"
    if ! path_in_list "${path}" "$@"; then
      violations+=("${path}")
    fi
  done < <(snapshot_section "${snapshot}" "changed-files")
  if [[ "${#violations[@]}" -gt 0 ]]; then
    printf '%s\n' "${violations[@]}"
    return 1
  fi
  return 0
}

# images_equal <expected-file> <got-file>
# Both files: one image reference per line. Returns non-zero, and prints a
# diff, unless the two sets are exactly equal.
images_equal() {
  local expected="$1" got="$2"
  if ! diff -u <(sort "${expected}") <(sort "${got}"); then
    return 1
  fi
  return 0
}

# image_delta <before-file> <after-file>
# Both files: one image reference per line (as `ctr images ls -q` prints).
# Prints every reference present in after but not in before -- what this
# install actually added to containerd's image set, ignoring whatever was
# already there from an earlier version's install (postrm never removes
# images, so later versions only add to a growing set).
image_delta() {
  local before="$1" after="$2"
  comm -13 <(sort -u "${before}") <(sort -u "${after}")
}

# strip_digest_refs <file>
# `ctr images import` gives an imported image both its tag reference (e.g.
# registry.k8s.io/kube-apiserver:v1.36.3) and a digest-pinned alias for the
# same content -- usually registry.k8s.io/kube-apiserver@sha256:..., but ctr
# can also list a bare content-addressed entry with no registry/repo prefix
# at all (sha256:...). kubeadm config images list only ever prints tag
# references, so both digest shapes have to be stripped out of a ctr-derived
# list before the two are compared, or the delta would spuriously contain
# extra entries no expected-image list would ever have.
strip_digest_refs() {
  grep -Ev '(@|^)sha256:' "$1" || true
}

# sysctls_are_one <snapshot-file>
# Every "key=value" line under === sysctls === must read "=1".
sysctls_are_one() {
  local snapshot="$1"
  local line bad=0
  while IFS= read -r line; do
    [[ -z "${line}" ]] && continue
    if [[ "${line}" != *"=1" ]]; then
      echo "${line}"
      bad=1
    fi
  done < <(snapshot_section "${snapshot}" "sysctls")
  return "${bad}"
}

# filter_stale_paths <prev-owned-file> <curr-owned-file> <allowlist entries...>
# prev/curr-owned-file: sorted dpkg -L output (one path per line), as
# node-snapshot.sh's "=== dpkg-owned-files ===" section prints. Prints every
# path that was owned by prev, is not owned by curr, and is not in the
# allowlist -- candidates that must not still exist on disk after the switch.
filter_stale_paths() {
  local prev="$1" curr="$2"
  shift 2
  local p
  while IFS= read -r p; do
    [[ -z "${p}" ]] && continue
    path_in_list "${p}" "$@" || echo "${p}"
  done < <(comm -23 <(sort -u "${prev}") <(sort -u "${curr}"))
}

# ---------------------------------------------------------------------------
# Everything below touches the network, QEMU, or the filesystem outside of
# fixture files -- wrapped in main() so sourcing this script for the helpers
# above does not also boot a VM.
# ---------------------------------------------------------------------------

main() {
  if [[ -z "${QCOW}" ]]; then
    shopt -s nullglob
    local non_fde_qcows=("${ROOT}/output-non-fde"/*.qcow2)
    shopt -u nullglob
    if [[ ${#non_fde_qcows[@]} -eq 1 ]]; then
      QCOW="${non_fde_qcows[0]}"
    else
      echo "boot-smoke: QCOW not set and found ${#non_fde_qcows[@]} qcow2 files under output-non-fde/ (need exactly 1); run 'make os-image-non-fde' or set QCOW explicitly" >&2
      exit 1
    fi
  fi

  # Same probe as packer/Makefile, and for the same reason: the CI runners have
  # no /dev/kvm, so -enable-kvm makes QEMU exit instead of booting. Emulated
  # boot is minutes rather than seconds, so the timeout moves with the
  # accelerator.
  local accel_args boot_timeout
  if [[ -c /dev/kvm && -w /dev/kvm ]]; then
    accel_args=(-enable-kvm -cpu host)
    boot_timeout="${BOOT_TIMEOUT:-180}"
  else
    echo "boot-smoke: no usable /dev/kvm, booting under software emulation (slower)" >&2
    accel_args=(-accel tcg)
    boot_timeout="${BOOT_TIMEOUT:-1200}"
  fi

  if [[ ! -f "${QCOW}" ]]; then
    echo "boot-smoke: no image at ${QCOW}, run 'make os-image-non-fde' first" >&2
    exit 1
  fi
  if [[ ! -f "${SSH_KEY}" ]]; then
    echo "boot-smoke: no ssh key at ${SSH_KEY}, run 'make ssh-key' first" >&2
    exit 1
  fi

  mkdir -p "${BUILD_LOGS_DIR}"

  local workdir disk seed pidfile
  workdir="$(mktemp -d)"
  disk="${workdir}/nke-node-smoke.qcow2"
  seed="${workdir}/seed.iso"
  pidfile="${workdir}/qemu.pid"

  cleanup() {
    if [[ -f "${pidfile}" ]] && kill -0 "$(cat "${pidfile}")" 2>/dev/null; then
      kill "$(cat "${pidfile}")" 2>/dev/null || true
      wait "$(cat "${pidfile}")" 2>/dev/null || true
    fi
    rm -rf "${workdir}"
  }
  trap cleanup EXIT

  # Same options on every ssh/scp call: batch, non-interactive, and don't
  # touch the real known_hosts for a throwaway VM whose host key changes
  # every run.
  local ssh_opts=(
    -i "${SSH_KEY}" -p "${SSH_PORT}"
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
    -o ConnectTimeout=5 -o BatchMode=yes
  )

  # ssh_run <timeout-seconds> <remote-command...>
  ssh_run() {
    local secs="$1"
    shift
    timeout "${secs}" ssh "${ssh_opts[@]}" ubuntu@127.0.0.1 "$@"
  }

  # ssh_script <timeout-seconds>  (remote bash script piped in on stdin)
  ssh_script() {
    local secs="$1"
    timeout "${secs}" ssh "${ssh_opts[@]}" ubuntu@127.0.0.1 bash -s
  }

  # Copy-on-write overlay so the smoke test never mutates the built artifact.
  qemu-img create -f qcow2 -F qcow2 -b "${QCOW}" "${disk}" >/dev/null

  cat >"${workdir}/user-data" <<EOF
#cloud-config
ssh_authorized_keys:
  - $(cat "${SSH_KEY}.pub")
EOF
  echo "instance-id: nke-boot-smoke
local-hostname: nke-boot-smoke" >"${workdir}/meta-data"

  cloud-localds "${seed}" "${workdir}/user-data" "${workdir}/meta-data"

  local efi_args=()
  if [[ -n "${OVMF_CODE}" ]]; then
    local efivars="${workdir}/OVMF_VARS.fd"
    cp "${OVMF_VARS_TEMPLATE}" "${efivars}"
    efi_args=(
      -drive if=pflash,format=raw,readonly=on,file="${OVMF_CODE}"
      -drive if=pflash,format=raw,file="${efivars}"
    )
  fi

  echo "boot-smoke: starting VM (ssh on 127.0.0.1:${SSH_PORT})"
  qemu-system-x86_64 \
    -m 2048 -smp 2 \
    "${accel_args[@]}" \
    -nographic \
    "${efi_args[@]}" \
    -drive if=virtio,file="${disk}",format=qcow2 \
    -drive if=virtio,file="${seed}",format=raw \
    -netdev user,id=net0,restrict=on,hostfwd=tcp::"${SSH_PORT}"-:22 \
    -device virtio-net,netdev=net0 \
    -pidfile "${pidfile}" \
    &

  local deadline=$((SECONDS + boot_timeout))
  # timeout wraps the ssh call itself: ConnectTimeout only bounds the SSH
  # handshake, not 'cloud-init status --wait' actually returning on the guest.
  # Without this, a guest that connects but never finishes cloud-init blocks
  # forever inside the `until` condition -- the deadline check below never
  # runs, since it's in the loop body, not the condition.
  until ssh_run 30 'cloud-init status --wait' 2>/dev/null; do
    if ((SECONDS > deadline)); then
      echo "boot-smoke: VM did not become SSH-reachable within ${boot_timeout}s" >&2
      exit 1
    fi
    sleep 5
  done

  echo "boot-smoke: VM booted and completed cloud-init OK"

  # The netdev above is restrict=on, so hostfwd (our own inbound SSH) works
  # but the guest can't reach anything outbound. Assert that directly rather
  # than just trusting the flag, so every check below it -- especially the
  # offline kubeadm-distro installs -- is provably using only what's already
  # on disk. curl's own --connect-timeout/--max-time bound it without an
  # outer `timeout` wrapper (ssh_run's own timeout is enough of a backstop).
  # Assert a specific curl exit code rather than "curl failed somehow": 6
  # (couldn't resolve host), 7 (couldn't connect) and 28 (timed out) are the
  # shapes restrict=on egress denial actually takes; anything else (e.g. 3, a
  # malformed URL) means the check itself is wrong, not that egress is
  # blocked.
  local curl_rc
  curl_rc="$(ssh_run 30 'curl -fsS --connect-timeout 5 --max-time 10 https://dl.k8s.io/ >/dev/null 2>&1; printf "%d" "$?"')"
  case "${curl_rc}" in
  6 | 7 | 28) ;;
  *)
    echo "boot-smoke: expected curl to fail with exit 6, 7 or 28 (no egress under restrict=on), got ${curl_rc}" >&2
    exit 1
    ;;
  esac

  echo "boot-smoke: confirmed the VM has no egress (curl exit ${curl_rc})"

  ssh_script 30 <<'EOF'
set -euo pipefail
systemctl is-enabled nke-agent.service
systemctl is-enabled spire-agent.service
systemctl is-enabled nke-spire-configure.service
test -x /usr/local/bin/nke-agent || test -x /usr/bin/nke-agent
test -x /usr/local/bin/spire-agent
test -x /usr/lib/nke/spire-configure.sh
test -x /usr/lib/nke/wait-spiffe-socket.sh
EOF

  echo "boot-smoke: nke-agent and spire-agent are installed and enabled"

  if [[ "${SKIP_OFFLINE_INSTALL:-0}" == "1" ]]; then
    echo "boot-smoke: SKIP_OFFLINE_INSTALL=1, skipping the offline kubeadm-distro install check"
    return 0
  fi

  echo "boot-smoke: checking offline install of every staged kubeadm-distro deb"

  # List every staged version, oldest first (sort -V), reading each one from
  # the package's own dpkg-deb metadata rather than parsing its filename. One
  # ssh call finds and echoes the list back in a single round trip.
  local versions
  versions="$(ssh_script 30 <<'EOF'
set -euo pipefail
shopt -s nullglob
debs=(/usr/lib/nke/debs/nke-kubeadm-distro-*_amd64.deb)
if [[ ${#debs[@]} -eq 0 ]]; then
  echo "boot-smoke: no staged nke-kubeadm-distro-*_amd64.deb under /usr/lib/nke/debs/" >&2
  exit 1
fi
for deb in "${debs[@]}"; do
  dpkg-deb -f "${deb}" Package
done | sed 's/^nke-kubeadm-distro-//' | sort -V
EOF
  )"

  if [[ -z "${versions}" ]]; then
    echo "boot-smoke: no staged kubeadm-distro versions found" >&2
    exit 1
  fi
  echo "boot-smoke: staged versions, oldest first: ${versions//$'\n'/ }"

  scp -i "${SSH_KEY}" -P "${SSH_PORT}" \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=5 -o BatchMode=yes \
    "${ROOT}/scripts/node-snapshot.sh" ubuntu@127.0.0.1:/tmp/node-snapshot.sh
  ssh_run 15 'chmod +x /tmp/node-snapshot.sh'

  local prev_snapshot="" prev_ver="" last_ver=""
  local marker="/tmp/nke-snapshot-marker"

  while IFS= read -r ver; do
    [[ -z "${ver}" ]] && continue
    last_ver="${ver}"
    local pkg="nke-kubeadm-distro-${ver}"
    echo "boot-smoke: installing ${pkg} offline (apt's Conflicts removes any version installed by an earlier loop iteration, exercising the version switch)"

    ssh_run 15 "sudo touch ${marker}"

    # The image-equality check below compares a before/after ctr images
    # delta, not an absolute set, because postrm never removes a prior
    # version's images (see the comment above disk-budget checking further
    # down) -- containerd's image set only grows across this loop. Before
    # the very first install, ctr/containerd don't exist on the base image
    # yet, so `command -v ctr` guards that case to an empty before-set
    # rather than an ssh failure.
    local before_images_file="${workdir}/before-images-${ver}.txt"
    ssh_run 30 'command -v ctr >/dev/null 2>&1 && sudo ctr -n k8s.io images ls -q 2>/dev/null | sort || true' >"${before_images_file}"

    # 15 minutes per version: the postinst starts containerd and imports the
    # base images from the tarball baked into the deb, which is slow under tcg
    # emulation. Removing a package does not remove its images from containerd,
    # so later iterations only add their own.
    ssh_script 900 <<EOF
set -euo pipefail
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y ${pkg}
EOF

    ssh_script 60 <<EOF
set -euo pipefail
echo "kubeadm version:"
got_version="\$(kubeadm version -o short)"
echo "\${got_version}"
[[ "\${got_version}" == "v${ver}" ]]

echo "containerd status:"
systemctl is-active containerd

echo "kubeadm preflight (2 vCPU / 2GB smoke VM, so NumCPU and Mem are ignored):"
sudo kubeadm init phase preflight --ignore-preflight-errors=NumCPU,Mem
EOF

    echo "boot-smoke: ${pkg} passed version/containerd/preflight checks"

    # The node-state contract: take the snapshot, save it as a build
    # artifact, then assert against it. 300s, not 60s: node-snapshot.sh's
    # find over /etc /usr /opt /var/lib /lib /var/log plus a full dpkg -L
    # is slower than the checks above, especially under tcg emulation.
    local snapshot_file="${BUILD_LOGS_DIR}/node-${ver}.snapshot"
    ssh_run 300 "sudo /tmp/node-snapshot.sh ${marker} ${pkg}" >"${snapshot_file}"
    echo "boot-smoke: wrote node snapshot to ${snapshot_file}"

    # This version's own postinst log is expected and unowned by dpkg; every
    # other install writes a differently-named log, so it has to be added to
    # the allowlist per-iteration rather than listed once in ALLOWLIST.
    local version_allow=("${ALLOWLIST[@]}" "/var/log/${pkg}-postinst.log")

    local unowned_violations
    if ! unowned_violations="$(check_unowned_allowlist "${snapshot_file}" "${version_allow[@]}")"; then
      echo "boot-smoke: ${pkg} left unowned files outside the allowlist:" >&2
      echo "${unowned_violations}" >&2
      exit 1
    fi

    local sysctl_bad
    if ! sysctl_bad="$(sysctls_are_one "${snapshot_file}")"; then
      echo "boot-smoke: ${pkg} left a k8s sysctl not set to 1:" >&2
      echo "${sysctl_bad}" >&2
      exit 1
    fi

    if [[ -n "$(snapshot_section "${snapshot_file}" swap)" ]]; then
      echo "boot-smoke: ${pkg} left swap on:" >&2
      snapshot_section "${snapshot_file}" swap >&2
      exit 1
    fi

    local containerd_active containerd_enabled kubelet_active
    containerd_active="$(snapshot_section "${snapshot_file}" containerd-active)"
    containerd_enabled="$(snapshot_section "${snapshot_file}" containerd-enabled)"
    kubelet_active="$(snapshot_section "${snapshot_file}" kubelet-active)"
    if [[ "${containerd_active}" != "active" ]]; then
      echo "boot-smoke: ${pkg} left containerd '${containerd_active}', want active" >&2
      exit 1
    fi
    if [[ "${containerd_enabled}" != "enabled" ]]; then
      echo "boot-smoke: ${pkg} left containerd '${containerd_enabled}', want enabled" >&2
      exit 1
    fi
    if [[ "${kubelet_active}" != "inactive" ]]; then
      echo "boot-smoke: ${pkg} left kubelet '${kubelet_active}', want inactive (enabled but not started until kubeadm init/join)" >&2
      exit 1
    fi
    if ! snapshot_section "${snapshot_file}" enabled-units | grep -Fxq 'kubelet.service'; then
      echo "boot-smoke: ${pkg} did not leave kubelet.service enabled" >&2
      exit 1
    fi

    echo "boot-smoke: ${pkg} passed the node-state contract (unowned files, sysctls, swap, containerd/kubelet state)"

    # Image equality: compare the delta this install actually imported
    # (after minus before, digest-alias references stripped) against
    # `kubeadm config images list`, both pulled back to the host so the
    # sourceable images_equal function is what actually runs the comparison
    # -- not a remote re-implementation of the same check.
    local after_images_file="${workdir}/after-images-${ver}.txt"
    snapshot_section "${snapshot_file}" "ctr-images" >"${after_images_file}"

    local delta_raw_file="${workdir}/delta-images-${ver}.txt"
    image_delta "${before_images_file}" "${after_images_file}" >"${delta_raw_file}"

    local delta_file="${workdir}/delta-images-stripped-${ver}.txt"
    strip_digest_refs "${delta_raw_file}" >"${delta_file}"

    local expected_file="${workdir}/expected-images-${ver}.txt"
    ssh_run 30 "kubeadm config images list --kubernetes-version v${ver} | sort" >"${expected_file}"

    if ! images_equal "${expected_file}" "${delta_file}"; then
      echo "boot-smoke: ${pkg} imported image delta does not equal kubeadm config images list" >&2
      exit 1
    fi
    echo "boot-smoke: ${pkg} imported exactly the expected image delta ($(wc -l <"${delta_file}" | tr -d ' ') images)"

    # Bound /var/cache/apt/archives: apt copies the local .deb it installs
    # from into its archive cache even though the source is already on disk
    # under /usr/lib/nke/debs/, so without this it grows by one full deb
    # every iteration. See the disk_size arithmetic in nke-node-fde.pkr.hcl.
    ssh_run 30 'sudo apt-get clean'

    # Version switch: nothing prev's own package owned may still be on disk
    # now unless the new package also owns it, or it's an allowlisted path.
    if [[ -n "${prev_snapshot}" ]]; then
      echo "boot-smoke: checking the switch from ${prev_ver} to ${ver} left no stale files"
      local prev_owned_file curr_owned_file
      prev_owned_file="$(mktemp)"
      curr_owned_file="$(mktemp)"
      snapshot_section "${prev_snapshot}" "dpkg-owned-files" >"${prev_owned_file}"
      snapshot_section "${snapshot_file}" "dpkg-owned-files" >"${curr_owned_file}"

      local stale_candidates=()
      while IFS= read -r p; do
        [[ -z "${p}" ]] && continue
        stale_candidates+=("${p}")
      done < <(filter_stale_paths "${prev_owned_file}" "${curr_owned_file}" "${ALLOWLIST[@]}")
      rm -f "${prev_owned_file}" "${curr_owned_file}"

      if [[ "${#stale_candidates[@]}" -gt 0 ]]; then
        local still_present
        # The trailing "; :" matters under `set -e`: without it, the remote
        # script's exit status is whatever the *last* `[ -e "$p" ]` test
        # returned, and in the passing case (the last candidate is indeed
        # gone) that's 1 -- which would make this whole plain assignment
        # trip `set -e` and kill boot-smoke on the success path. "; :" pins
        # the remote exit status to 0 regardless of the last test's result;
        # the actual signal is still_present's *content*, checked below.
        still_present="$(printf '%s\n' "${stale_candidates[@]}" | ssh_run 30 \
          'while IFS= read -r p; do [ -e "$p" ] && echo "$p"; done; :')"
        if [[ -n "${still_present}" ]]; then
          echo "boot-smoke: switching to ${ver} left files ${prev_ver} owned that ${ver} does not own and are not in the allowlist:" >&2
          echo "${still_present}" >&2
          exit 1
        fi
      fi
      echo "boot-smoke: no stale files from ${prev_ver} after switching to ${ver}"
    fi

    prev_snapshot="${snapshot_file}"
    prev_ver="${ver}"
  done <<<"${versions}"

  echo "boot-smoke: offline install checks passed for every staged kubeadm-distro version"

  # Disk budget: three installs each unpack binaries and import a base-image
  # tarball into containerd's content store, which accumulates across the
  # loop above (removing a package does not remove its images, see the
  # comment above the install loop). A regression here should show up as
  # this explicit message, not an ENOSPC mid-import three versions later.
  echo "boot-smoke: disk usage on / after all installs:"
  ssh_run 15 'df -h /'
  local free_bytes
  free_bytes="$(ssh_run 15 "df --output=avail -B1 / | tail -n1 | tr -d ' '")"
  if ((free_bytes < 1073741824)); then
    echo "boot-smoke: only ${free_bytes} bytes free on / after installs, want > 1 GiB" >&2
    exit 1
  fi

  # Remove: postrm's contract (see packaging/postrm) is to disable both
  # units, delete the two node-state drop-ins, and leave /etc/fstab alone --
  # reverting the swap-off edit would let swap come back under a kubelet
  # that may still be running.
  echo "boot-smoke: removing nke-kubeadm-distro-${last_ver} and checking postrm's contract"
  local fstab_before fstab_after
  fstab_before="$(ssh_run 15 'sha256sum /etc/fstab')"

  ssh_script 120 <<EOF
set -euo pipefail
sudo DEBIAN_FRONTEND=noninteractive apt-get remove -y nke-kubeadm-distro-${last_ver}
EOF

  fstab_after="$(ssh_run 15 'sha256sum /etc/fstab')"
  if [[ "${fstab_before}" != "${fstab_after}" ]]; then
    echo "boot-smoke: /etc/fstab changed during package removal (before: ${fstab_before}, after: ${fstab_after})" >&2
    exit 1
  fi
  echo "boot-smoke: /etc/fstab untouched by removal"

  local removal_check
  removal_check="$(ssh_script 30 <<'EOF'
set -euo pipefail
echo "containerd-enabled:$(systemctl is-enabled containerd 2>/dev/null || echo not-found)"
echo "kubelet-enabled:$(systemctl is-enabled kubelet 2>/dev/null || echo not-found)"
if [ -e /etc/sysctl.d/k8s.conf ]; then
  echo "sysctl-dropin:present"
else
  echo "sysctl-dropin:absent"
fi
if [ -e /etc/modules-load.d/k8s.conf ]; then
  echo "modules-dropin:present"
else
  echo "modules-dropin:absent"
fi
shopt -s nullglob
for l in /etc/systemd/system/multi-user.target.wants/*; do
  if [ -L "${l}" ] && [ ! -e "${l}" ]; then
    echo "dangling:${l}"
  fi
done
EOF
  )"

  echo "${removal_check}"
  # Removing the package also removes the unit files themselves (dpkg owns
  # them), so `systemctl is-enabled` after removal legitimately reports
  # "not-found" rather than "disabled" -- both mean "won't start on its
  # own", which is what actually matters here. The dangling-symlink check
  # below is the real signal for "did postrm clean up after itself"; a
  # follow-up packaging change is moving the disable itself into prerm
  # (while the units still exist to be disabled), which would make
  # "disabled" the expected state again.
  if ! grep -Eq '^containerd-enabled:(disabled|not-found)$' <<<"${removal_check}"; then
    echo "boot-smoke: containerd.service was not left disabled (or removed) after removing nke-kubeadm-distro-${last_ver}" >&2
    exit 1
  fi
  if ! grep -Eq '^kubelet-enabled:(disabled|not-found)$' <<<"${removal_check}"; then
    echo "boot-smoke: kubelet.service was not left disabled (or removed) after removing nke-kubeadm-distro-${last_ver}" >&2
    exit 1
  fi
  if ! grep -q '^sysctl-dropin:absent$' <<<"${removal_check}"; then
    echo "boot-smoke: /etc/sysctl.d/k8s.conf was not removed by postrm" >&2
    exit 1
  fi
  if ! grep -q '^modules-dropin:absent$' <<<"${removal_check}"; then
    echo "boot-smoke: /etc/modules-load.d/k8s.conf was not removed by postrm" >&2
    exit 1
  fi
  if grep -q '^dangling:' <<<"${removal_check}"; then
    echo "boot-smoke: dangling multi-user.target.wants symlinks remain after removal:" >&2
    grep '^dangling:' <<<"${removal_check}" >&2
    exit 1
  fi

  echo "boot-smoke: nke-kubeadm-distro-${last_ver} removal passed postrm's contract"
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
