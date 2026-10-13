#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Build the guestfs-tier test fixtures: two ~64 MiB qcow2 disks shaped like
# the node images' storage stacks, holding just enough filesystem content for
# every guestfs-tier check to pass.
#
#   disk-fde.qcow2      LUKS2 (passphrase "changeit") -> LVM ubuntu-vg/ubuntu-lv -> ext4
#   disk-nonfde.qcow2   plain ext4
#
# Needs root on Linux (cryptsetup + device-mapper + loop); runs happily in a
# privileged container. Container lessons baked in: dm devices are
# kernel-global so cleanup is targeted and idempotent; LVM runs udev-less.
#
# Usage: gen-guestfs-fixture.sh <output-dir>
set -euo pipefail
OUT="${1:?usage: gen-guestfs-fixture.sh <output-dir>}"
mkdir -p "${OUT}"
export DM_DISABLE_UDEV=1

VG=nvkefixt-vg
LV=root
LUKSNAME=nvkefixt-luks

cleanup_residue() {
  vgchange -an --noudevsync "${VG}" 2>/dev/null || true
  dmsetup remove "${VG//-/--}-${LV}" 2>/dev/null || true
  cryptsetup close "${LUKSNAME}" 2>/dev/null || true
}
cleanup_residue

populate() { # populate <mountpoint>
  local mnt="$1"
  mkdir -p "${mnt}/var/lib/dpkg" "${mnt}/etc/systemd/system" "${mnt}/var/lib/etcd"
  cat > "${mnt}/var/lib/dpkg/status" <<'STATUS'
Package: linux-nvidia
Status: install ok installed
Version: 6.8.0-1064.67
Architecture: amd64

Package: nke-cni-calico-manifests-3.30.1
Status: install ok installed
Version: 1
Architecture: all

Package: dash
Status: install ok installed
Version: 0.5.12-6ubuntu5
Architecture: amd64
STATUS
  printf 'root:x:0:0:root:/root:/bin/bash\netcd:x:998:997::/var/lib/etcd:/usr/sbin/nologin\n' > "${mnt}/etc/passwd"
  printf 'root:x:0:\netcd:x:997:\n' > "${mnt}/etc/group"
  for unit in apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service multipathd.service multipathd.socket; do
    ln -sf /dev/null "${mnt}/etc/systemd/system/${unit}"
  done
  chown 998:997 "${mnt}/var/lib/etcd"
  chmod 0700 "${mnt}/var/lib/etcd"
}

# --- non-FDE: plain ext4 ----------------------------------------------------
RAW="${OUT}/disk-nonfde.raw"
truncate -s 64M "${RAW}"
mkfs.ext4 -q "${RAW}"
MNT="$(mktemp -d)"
mount -o loop "${RAW}" "${MNT}"
populate "${MNT}"
umount "${MNT}"
qemu-img convert -f raw -O qcow2 "${RAW}" "${OUT}/disk-nonfde.qcow2"
rm -f "${RAW}"

# --- FDE: LUKS2 -> LVM -> ext4 ----------------------------------------------
RAW="${OUT}/disk-fde.raw"
truncate -s 96M "${RAW}"
LOOP="$(losetup --find --show "${RAW}")"
echo -n changeit | cryptsetup -q luksFormat --type luks2 --pbkdf pbkdf2 --pbkdf-force-iterations 1000 "${LOOP}"
echo -n changeit | cryptsetup open "${LOOP}" "${LUKSNAME}"
pvcreate "/dev/mapper/${LUKSNAME}" >/dev/null
vgcreate "${VG}" "/dev/mapper/${LUKSNAME}" >/dev/null
lvcreate -y -Zn --noudevsync -n "${LV}" -l 100%FREE "${VG}" >/dev/null
vgchange -ay --noudevsync "${VG}" >/dev/null
dmsetup mknodes
LVDEV="/dev/mapper/${VG//-/--}-${LV}"
mkfs.ext4 -q "${LVDEV}"
mount "${LVDEV}" "${MNT}"
populate "${MNT}"
umount "${MNT}"
vgchange -an --noudevsync "${VG}" >/dev/null
cryptsetup close "${LUKSNAME}"
losetup -d "${LOOP}"
qemu-img convert -f raw -O qcow2 "${RAW}" "${OUT}/disk-fde.qcow2"
rm -f "${RAW}"
rmdir "${MNT}"

echo "fixtures written to ${OUT}:"
ls -la "${OUT}"
