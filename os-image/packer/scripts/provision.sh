#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Guest provisioner. Artifacts are already in /tmp/nke-image.

set -euo pipefail

SRC=/tmp/nke-image

sudo install -d -m 0755 /usr/lib/nke /etc/nke /var/lib/nke /usr/local/bin
sudo install -m 0755 "${SRC}/spire-agent" /usr/local/bin/spire-agent
sudo install -m 0755 "${SRC}/spire-configure.sh" /usr/lib/nke/spire-configure.sh
sudo install -m 0755 "${SRC}/wait-spiffe-socket.sh" /usr/lib/nke/wait-spiffe-socket.sh
sudo install -m 0644 "${SRC}/spire-agent.conf.tmpl" /usr/lib/nke/spire-agent.conf.tmpl
sudo install -m 0644 "${SRC}/spire-agent.service" /etc/systemd/system/spire-agent.service
sudo install -m 0644 "${SRC}/nke-spire-configure.service" /etc/systemd/system/nke-spire-configure.service

sudo install -m 0755 "${SRC}/nke-agent" /usr/local/bin/nke-agent
sudo install -m 0644 "${SRC}/nke-agent.service" /etc/systemd/system/nke-agent.service

if [[ ! -f /etc/default/nke-agent ]]; then
  echo 'AGENT_ARGS=""' | sudo tee /etc/default/nke-agent >/dev/null
fi

if ! id -u spire >/dev/null 2>&1; then
  sudo useradd -r -s /bin/false -d /var/lib/spire-agent spire
fi
sudo install -d -m 0750 -o spire -g spire /var/lib/spire-agent

sudo systemctl daemon-reload
sudo systemctl enable nke-spire-configure.service
sudo systemctl enable spire-agent.service
sudo systemctl enable nke-agent.service

# Stage the nke-kubeadm-distro debs as a local flat apt repo: the install
# command at cluster create stays `apt-get install -y
# nke-kubeadm-distro-<x.y.z>` unchanged, and it resolves fully offline. A
# file: repo is used (not dpkg -i) so apt still resolves deps normally.
# trusted=yes is fine because the content is already on this root fs.
sudo install -d -m 0755 /usr/lib/nke/debs
sudo install -m 0644 "${SRC}"/debs/*.deb /usr/lib/nke/debs/
sudo install -m 0644 "${SRC}/debs/Packages" /usr/lib/nke/debs/Packages
echo 'deb [trusted=yes] file:/usr/lib/nke/debs ./' | sudo tee /etc/apt/sources.list.d/nke-local.list >/dev/null
# Cache the local index so cluster create needs no apt-get update of its
# own. Scoped to only the local source: a mirror hiccup on the build host
# cannot fail the bake, and no Ubuntu archive lists get baked into the image.
sudo apt-get update \
  -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/nke-local.list \
  -o Dir::Etc::sourceparts=- \
  -o APT::Get::List-Cleanup=0

# Golden image: next boot must look like a new instance, without the Packer
# SSH key or a machine-id from this build VM.
sudo cloud-init clean --logs --seed || true
if [[ -f /etc/machine-id ]]; then
  sudo truncate -s 0 /etc/machine-id
fi
sudo rm -f /var/lib/dbus/machine-id
sudo rm -f /home/ubuntu/.ssh/authorized_keys

# Smaller qcow2: apt's package cache and now-unreferenced blocks (deleted
# files, old package versions) don't shrink the disk image on their own --
# fstrim tells the guest kernel to discard them, which the disk_discard =
# "unmap" builder setting (nke-node-fde.pkr.hcl / nke-node-non-fde.pkr.hcl)
# passes through to the backing qcow2 so Packer's own post-build compaction
# ("Converting hard drive...") actually has free space to reclaim. Must run
# last: any write after this leaves blocks un-trimmed again.
echo "Cleaning up package cache and trimming filesystem..."
sudo apt-get -y autoremove
# Empties /var/cache/apt/archives only; /var/lib/apt/lists (the local index
# apt-get update above populated) is untouched, so it stays usable.
sudo apt-get clean
# The uploaded artifacts (~1 GB, including the debs) must not stay allocated
# in the qcow2 now that everything is installed/staged under /usr and /etc.
sudo rm -rf /tmp/nke-image
sudo fstrim -av || true
