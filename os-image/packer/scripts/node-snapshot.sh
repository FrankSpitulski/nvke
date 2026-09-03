#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Runs INSIDE the guest, invoked by boot-smoke.sh over the same ssh helper it
# uses for every other guest command. Prints a deterministic text snapshot of
# the node-level state a kubeadm-distro install/remove is allowed to touch,
# so boot-smoke.sh can assert against it and a human can diff two runs.
#
# Usage: node-snapshot.sh <marker-file> <package-name>
#
# <marker-file> is a file boot-smoke.sh touches immediately before
# apt-get install/remove; every section below that depends on "since the
# marker" uses its ctime as the cutoff (see the note on -cnewer below).
# <package-name> is the dpkg package name (e.g. nke-kubeadm-distro-1.36.3)
# whose ownership tags and `dpkg -L` listing this snapshot reports.
#
# Needs root (ctr, and reading root-owned paths under /var/lib) -- boot-smoke.sh
# runs this via sudo.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: node-snapshot.sh <marker-file> <package-name>" >&2
  exit 1
fi

MARKER="$1"
PACKAGE="$2"

if [[ ! -e "${MARKER}" ]]; then
  echo "node-snapshot: marker file ${MARKER} not found" >&2
  exit 1
fi

# Reused by both the "changed files" ownership tagging (a) and the raw
# listing (b), so dpkg is only asked once. Loaded into an associative array
# so each of the (typically few hundred) changed files is a single hash
# lookup rather than a grep of the whole owned list.
OWNED="$(dpkg -L "${PACKAGE}" 2>/dev/null | sort)"
declare -A OWNED_SET=()
while IFS= read -r p; do
  [[ -n "${p}" ]] && OWNED_SET["${p}"]=1
done <<<"${OWNED}"

echo "=== changed-files ==="
# -cnewer, not -newer: nfpm pins every dpkg-shipped file's mtime to a fixed
# build-time value (nfpm.yaml's top-level `mtime`, so two builds of the same
# input are byte-identical), which means dpkg-installed files never show up
# under -newer's mtime comparison -- the dpkg-owned/unowned split would be
# vacuous. ctime (inode change time: dpkg unpacking a file, or postinst
# writing/chmod'ing one) is not pinned by anything and always advances, so
# it is the cutoff that actually reflects "touched since the marker".
#
# Roots: everywhere the postinst/postrm contract is allowed to touch. Not
# /lib: it is a usrmerge symlink to /usr/lib on this image, and find (with no
# -H/-L) never descends a symlinked start point, so it was a no-op root --
# dropped rather than kept as dead weight. Not /var/log as a bare root
# either: with no prunes it flags dpkg.log, apt/*, syslog, auth.log (every
# sudo call), journal/*, wtmp and lastlog as "changed" on every install --
# none of that is postinst's doing. Its one legitimate output, the postinst
# log, is picked up by the separate targeted find below instead.
#
# -not -path "${MARKER}" excludes the marker file itself from its own
# snapshot on the (currently untrue, but cheap-to-guard) chance it ever
# lives under one of these roots. /etc/ld.so.cache and /etc/mtab are runtime
# state no maintainer script owns. Everything under /var/lib is pruned
# except what postinst/postrm actually touch (dpkg's own bookkeeping, and
# the placeholder directories this package ships): containerd's content
# store (owned by containerd at runtime, not by this package's file list),
# apt's and dpkg's own state, systemd's (timers/stamp-*, and
# deb-systemd-helper's enabled-state bookkeeping), logrotate's status file,
# man-db's auto-update stamp, update-notifier's motd cache,
# ubuntu-advantage's state, and PackageKit's cache -- all periodic or
# apt/dpkg-triggered background churn unrelated to this package.
while IFS= read -r f; do
  [[ -z "${f}" ]] && continue
  if [[ -n "${OWNED_SET[${f}]+x}" ]]; then
    echo "dpkg-owned ${f}"
  else
    echo "unowned ${f}"
  fi
done < <(
  {
    find /etc /usr /opt /var/lib -cnewer "${MARKER}" \( -type f -o -type l \) \
      -not -path "${MARKER}" \
      -not -path '/var/lib/containerd/*' \
      -not -path '/var/lib/apt/*' \
      -not -path '/var/lib/dpkg/*' \
      -not -path '/var/lib/systemd/*' \
      -not -path '/var/lib/logrotate/*' \
      -not -path '/var/lib/man-db/*' \
      -not -path '/var/lib/update-notifier/*' \
      -not -path '/var/lib/ubuntu-advantage/*' \
      -not -path '/var/lib/PackageKit/*' \
      -not -path '/etc/ld.so.cache' \
      -not -path '/etc/mtab' \
      2>/dev/null
    # The one thing under /var/log postinst legitimately writes: its own
    # per-version log (LOG="/var/log/${PACKAGE}-postinst.log" in postinst).
    # -maxdepth 1 and a name glob keep this from ever seeing syslog/auth.log/
    # journal/wtmp/etc regardless of what else touches /var/log.
    find /var/log -maxdepth 1 -name 'nke-kubeadm-distro-*-postinst.log' -cnewer "${MARKER}" -type f \
      2>/dev/null
  } | sort -u
)

echo "=== dpkg-owned-files ==="
printf '%s\n' "${OWNED}"

echo "=== enabled-units ==="
systemctl list-unit-files --state=enabled --no-legend | awk '{print $1}' | sort

echo "=== containerd-active ==="
systemctl is-active containerd || true

echo "=== containerd-enabled ==="
systemctl is-enabled containerd || true

echo "=== kubelet-active ==="
systemctl is-active kubelet || true

echo "=== sysctls ==="
for k in \
  net.bridge.bridge-nf-call-iptables \
  net.bridge.bridge-nf-call-ip6tables \
  net.ipv4.ip_forward \
  net.ipv6.conf.all.forwarding \
  net.ipv6.conf.default.forwarding; do
  v="$(sysctl -n "${k}" 2>/dev/null || echo MISSING)"
  echo "${k}=${v}"
done

echo "=== loaded-modules ==="
for m in overlay br_netfilter nf_conntrack; do
  if lsmod | awk '{print $1}' | grep -Fxq -- "${m}"; then
    echo "${m}"
  fi
done | sort

echo "=== swap ==="
swapon --show --noheadings

echo "=== ctr-images ==="
ctr -n k8s.io images ls -q 2>/dev/null | sort

echo "=== hashes ==="
for f in \
  /etc/containerd/config.toml \
  /etc/crictl.yaml \
  /etc/default/kubelet \
  /lib/systemd/system/containerd.service \
  /lib/systemd/system/kubelet.service \
  /lib/systemd/system/kubelet.service.d/10-kubeadm.conf; do
  if [[ -f "${f}" ]]; then
    sha256sum "${f}"
  else
    echo "MISSING ${f}"
  fi
done
