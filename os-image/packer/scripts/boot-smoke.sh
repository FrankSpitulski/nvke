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
# staged version is installed and checked in turn -- kubeadm version,
# containerd/kubelet unit state, the k8s sysctls and swap, the imported image
# set, kubeadm preflight -- then the last one is removed and postrm's cleanup
# is checked. SKIP_OFFLINE_INSTALL=1 skips this, for anyone booting an image
# built without staged debs.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
QCOW="${QCOW:-}"
SSH_KEY="${SSH_KEY:-${ROOT}/.ssh/id_ed25519}"
SSH_PORT="${SSH_PORT:-2222}"
OVMF_CODE="${OVMF_CODE:-}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"

if [[ -z "${QCOW}" ]]; then
  shopt -s nullglob
  non_fde_qcows=("${ROOT}/output-non-fde"/*.qcow2)
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
#
# The KVM budget is generous on purpose: the one measured KVM boot on the CI
# runner took 159 s to finish cloud-init, and a run on the same commit blew
# through 180 s. Under restrict=on (below) anything in the guest that phones
# home times out instead of being refused, which makes boot time vary.
if [[ -c /dev/kvm && -w /dev/kvm ]]; then
  accel_args=(-enable-kvm -cpu host)
  boot_timeout="${BOOT_TIMEOUT:-600}"
else
  echo "boot-smoke: no usable /dev/kvm, booting under software emulation (slower)" >&2
  accel_args=(-accel tcg)
  boot_timeout="${BOOT_TIMEOUT:-1800}"
fi

if [[ ! -f "${QCOW}" ]]; then
  echo "boot-smoke: no image at ${QCOW}, run 'make os-image-non-fde' first" >&2
  exit 1
fi
if [[ ! -f "${SSH_KEY}" ]]; then
  echo "boot-smoke: no ssh key at ${SSH_KEY}, run 'make ssh-key' first" >&2
  exit 1
fi

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

# Same options on every ssh call: batch, non-interactive, and don't touch the
# real known_hosts for a throwaway VM whose host key changes every run.
ssh_opts=(
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

efi_args=()
if [[ -n "${OVMF_CODE}" ]]; then
  efivars="${workdir}/OVMF_VARS.fd"
  cp "${OVMF_VARS_TEMPLATE}" "${efivars}"
  efi_args=(
    -drive if=pflash,format=raw,readonly=on,file="${OVMF_CODE}"
    -drive if=pflash,format=raw,file="${efivars}"
  )
fi

# The guest's serial console goes to build-logs/, the directory the Packer
# templates already write their install consoles to and CI uploads on
# failure, so a boot-smoke failure leaves the same evidence a bake failure
# does. dump_console tails it into the job log on the failure paths below.
console_log="${ROOT}/build-logs/boot-smoke-console.log"
mkdir -p "$(dirname "${console_log}")"
dump_console() {
  echo "boot-smoke: last 60 lines of the guest console (${console_log}):" >&2
  tail -n 60 "${console_log}" >&2 2>/dev/null || true
}

echo "boot-smoke: starting VM (ssh on 127.0.0.1:${SSH_PORT}, console in ${console_log})"
qemu-system-x86_64 \
  -m 2048 -smp 2 \
  "${accel_args[@]}" \
  -display none \
  -serial "file:${console_log}" \
  "${efi_args[@]}" \
  -drive if=virtio,file="${disk}",format=qcow2 \
  -drive if=virtio,file="${seed}",format=raw \
  -netdev user,id=net0,restrict=on,hostfwd=tcp::"${SSH_PORT}"-:22 \
  -device virtio-net,netdev=net0 \
  -pidfile "${pidfile}" \
  &

# Two waits, so a failure names what actually failed. First, SSH reachable at
# all; only the handshake is bounded here. Second, one blocking
# `cloud-init status --wait` for whatever budget is left, with its output
# kept: it exits 1 on error and 2 on degraded, and folding it into the SSH
# loop reported both as "not SSH-reachable" with the reason thrown away.
boot_start=${SECONDS}
deadline=$((boot_start + boot_timeout))
until ssh_run 15 true 2>/dev/null; do
  if ((SECONDS > deadline)); then
    echo "boot-smoke: no SSH on 127.0.0.1:${SSH_PORT} within ${boot_timeout}s" >&2
    dump_console
    exit 1
  fi
  sleep 5
done
echo "boot-smoke: SSH reachable after $((SECONDS - boot_start))s"

remaining=$((deadline - SECONDS))
((remaining > 30)) || remaining=30
if ! ssh_run "${remaining}" 'cloud-init status --wait --long'; then
  echo "boot-smoke: cloud-init did not finish cleanly within ${boot_timeout}s of boot" >&2
  ssh_run 30 'cloud-init status --long; sudo tail -n 100 /var/log/cloud-init.log' >&2 || true
  dump_console
  exit 1
fi

echo "boot-smoke: VM booted and completed cloud-init OK"

# The netdev is restrict=on, so hostfwd (our own inbound SSH) works but the
# guest can't reach anything outbound. Assert that directly so the offline
# installs below are provably using only what's already on disk. curl exits
# 6 (resolve), 7 (connect) or 28 (timeout) when egress is denied; anything
# else means the check itself is wrong.
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
  exit 0
fi

echo "boot-smoke: checking offline install of every staged kubeadm-distro deb"

# List every staged version, oldest first (sort -V), reading each one from
# the package's own dpkg-deb metadata rather than parsing its filename.
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

last_ver=""
while IFS= read -r ver; do
  [[ -z "${ver}" ]] && continue
  last_ver="${ver}"
  pkg="nke-kubeadm-distro-${ver}"
  echo "boot-smoke: installing ${pkg} offline (apt's Conflicts removes any version installed by an earlier loop iteration, exercising the version switch)"

  # Removing a package never removes its images from containerd, so the image
  # set only grows across this loop. Compare what this install added (after
  # minus before) to kubeadm's list, not the absolute set. Before the first
  # install ctr doesn't exist yet, hence the `command -v` guard.
  before_images="${workdir}/before-images-${ver}.txt"
  ssh_run 30 'command -v ctr >/dev/null 2>&1 && sudo ctr -n k8s.io images ls -q 2>/dev/null | sort -u || true' >"${before_images}"

  # 15 minutes per version: the postinst starts containerd and imports the
  # base images from the tarball baked into the deb, which is slow under tcg
  # emulation.
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

echo "unit state (containerd active+enabled, kubelet enabled but not started until kubeadm init/join):"
systemctl is-active containerd
systemctl is-enabled containerd
systemctl is-enabled kubelet
[[ "\$(systemctl is-active kubelet || true)" == "inactive" ]]

echo "k8s sysctls and swap:"
for k in net.bridge.bridge-nf-call-iptables net.bridge.bridge-nf-call-ip6tables net.ipv4.ip_forward net.ipv6.conf.all.forwarding net.ipv6.conf.default.forwarding; do
  v="\$(sysctl -n "\${k}")"
  echo "\${k}=\${v}"
  [[ "\${v}" == "1" ]]
done
[[ -z "\$(swapon --noheadings)" ]]

echo "kubeadm preflight (2 vCPU / 2GB smoke VM, so NumCPU and Mem are ignored):"
sudo kubeadm init phase preflight --ignore-preflight-errors=NumCPU,Mem
EOF

  # Two directions, checked separately because images accumulate across the
  # loop and consecutive releases share some (1.34.2 and 1.35.3 both ship
  # pause:3.10.1), so "what this install added" is not "what kubeadm needs":
  #   1. every image kubeadm lists is in containerd after the install;
  #   2. nothing this install added is outside kubeadm's list.
  # The digest aliases `ctr images import` adds (registry/repo@sha256:... and
  # bare sha256:...) are dropped first; kubeadm's list never has them.
  after_images="${workdir}/after-images-${ver}.txt"
  ssh_run 30 'sudo ctr -n k8s.io images ls -q | sort -u' >"${after_images}"
  expected_images="${workdir}/expected-images-${ver}.txt"
  ssh_run 30 "kubeadm config images list --kubernetes-version v${ver} | sort -u" >"${expected_images}"
  delta_images="${workdir}/delta-images-${ver}.txt"
  comm -13 "${before_images}" "${after_images}" | { grep -Ev '(@|^)sha256:' || [[ $? -eq 1 ]]; } >"${delta_images}"
  missing_images="$(comm -23 "${expected_images}" "${after_images}")"
  if [[ -n "${missing_images}" ]]; then
    echo "boot-smoke: ${pkg} did not import every image kubeadm config images list needs; missing:" >&2
    echo "${missing_images}" >&2
    exit 1
  fi
  extra_images="$(comm -13 "${expected_images}" "${delta_images}")"
  if [[ -n "${extra_images}" ]]; then
    echo "boot-smoke: ${pkg} imported image(s) kubeadm config images list does not ask for:" >&2
    echo "${extra_images}" >&2
    exit 1
  fi
  echo "boot-smoke: ${pkg} passed version/unit-state/sysctl/preflight checks; all $(wc -l <"${expected_images}" | tr -d ' ') images kubeadm lists are present and this install added nothing else ($(wc -l <"${delta_images}" | tr -d ' ') new)"

  # apt copies the local .deb it installs from into /var/cache/apt/archives
  # even though the source is already on disk under /usr/lib/nke/debs/, so
  # without this the cache grows by one full deb every iteration. See the
  # disk_size arithmetic in nke-node-fde.pkr.hcl.
  ssh_run 30 'sudo apt-get clean'
done <<<"${versions}"

echo "boot-smoke: offline install checks passed for every staged kubeadm-distro version"

# Disk budget: each install imports a base-image tarball into containerd's
# content store, which accumulates across the loop above. A regression here
# should show up as this explicit message, not an ENOSPC mid-import.
echo "boot-smoke: disk usage on / after all installs:"
ssh_run 15 'df -h /'
free_bytes="$(ssh_run 15 "df --output=avail -B1 / | tail -n1 | tr -d ' '")"
if ((free_bytes < 1073741824)); then
  echo "boot-smoke: only ${free_bytes} bytes free on / after installs, want > 1 GiB" >&2
  exit 1
fi

# Remove: prerm/postrm's contract (see packaging/prerm and packaging/postrm)
# is to disable both units, delete the two node-state drop-ins, and leave
# /etc/fstab alone -- reverting the swap-off edit would let swap come back
# under a kubelet that may still be running.
echo "boot-smoke: removing nke-kubeadm-distro-${last_ver} and checking postrm's contract"
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

# dpkg removes the unit files themselves, so `is-enabled` legitimately reports
# not-found rather than disabled; both mean "won't start on its own". The
# dangling-symlink check is the real signal that prerm disabled the units
# while they still existed.
ssh_script 30 <<'EOF'
set -euo pipefail
for u in containerd kubelet; do
  state="$(systemctl is-enabled "${u}" 2>/dev/null || echo not-found)"
  echo "${u}: ${state}"
  [[ "${state}" == disabled || "${state}" == not-found ]]
done
[[ ! -e /etc/sysctl.d/k8s.conf ]]
[[ ! -e /etc/modules-load.d/k8s.conf ]]
shopt -s nullglob
for l in /etc/systemd/system/multi-user.target.wants/*; do
  if [[ -L "${l}" && ! -e "${l}" ]]; then
    echo "dangling unit symlink left after removal: ${l}" >&2
    exit 1
  fi
done
EOF

echo "boot-smoke: nke-kubeadm-distro-${last_ver} removal passed postrm's contract (units disabled, drop-ins removed, /etc/fstab untouched)"
