#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Host-side unit tests for boot-smoke.sh's pure, ssh-free helper functions
# (snapshot_section, check_unowned_allowlist, sysctls_are_one, images_equal,
# image_delta, strip_digest_refs, filter_stale_paths) plus a standalone check
# that the remote stale-file-existence loop boot-smoke.sh ships as a literal
# string behaves correctly under `set -e` -- no VM, no ssh, no network. Run
# with `make -C os-image test-scripts`, or directly:
#
#   bash packer/scripts/boot-smoke_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BOOT_SMOKE="${SCRIPT_DIR}/boot-smoke.sh"

# Sourcing (rather than executing) is what makes this safe: the BASH_SOURCE
# guard at the bottom of boot-smoke.sh only calls main() when the script is
# run directly ("${BASH_SOURCE[0]}" == "${0}"), which is false here.
# shellcheck source=./boot-smoke.sh
source "${BOOT_SMOKE}"

PASS=0
FAIL=0

pass() {
  PASS=$((PASS + 1))
  echo "ok   - $1"
}

fail() {
  FAIL=$((FAIL + 1))
  echo "FAIL - $1"
}

TESTDIR="$(mktemp -d)"
trap 'rm -rf "${TESTDIR}"' EXIT

# ---------------------------------------------------------------------------
# snapshot_section
# ---------------------------------------------------------------------------

snap1="${TESTDIR}/snap1"
cat >"${snap1}" <<'EOF'
=== changed-files ===
dpkg-owned /usr/bin/kubeadm
unowned /etc/sysctl.d/k8s.conf
unowned /var/log/nke-kubeadm-distro-1.36.3-postinst.log
unowned /etc/evil.conf
=== dpkg-owned-files ===
/usr/bin/kubeadm
/usr/bin/kubectl
=== containerd-active ===
active
=== containerd-enabled ===
enabled
=== kubelet-active ===
inactive
=== sysctls ===
net.ipv4.ip_forward=1
net.ipv6.conf.all.forwarding=0
=== swap ===
=== enabled-units ===
containerd.service
kubelet.service
EOF

got="$(snapshot_section "${snap1}" containerd-active)"
[[ "${got}" == "active" ]] && pass "snapshot_section extracts a single-line section" \
  || fail "snapshot_section extracts a single-line section: got [${got}]"

got="$(snapshot_section "${snap1}" swap)"
[[ "${got}" == "" ]] && pass "snapshot_section extracts an empty section as empty" \
  || fail "snapshot_section extracts an empty section as empty: got [${got}]"

# ---------------------------------------------------------------------------
# check_unowned_allowlist -- the base allowlist plus a per-version postinst
# log entry, the same shape boot-smoke.sh's install loop builds.
# ---------------------------------------------------------------------------

base_allow=(
  /etc/sysctl.d/k8s.conf
  /etc/modules-load.d/k8s.conf
  /etc/fstab
  /etc/systemd/system/multi-user.target.wants/containerd.service
  /etc/systemd/system/multi-user.target.wants/kubelet.service
)
version_allow=("${base_allow[@]}" /var/log/nke-kubeadm-distro-1.36.3-postinst.log)

if out="$(check_unowned_allowlist "${snap1}" "${version_allow[@]}")"; then
  fail "check_unowned_allowlist should fail: /etc/evil.conf is not allowlisted"
else
  [[ "${out}" == "/etc/evil.conf" ]] && pass "check_unowned_allowlist reports exactly the one true violation" \
    || fail "check_unowned_allowlist reports exactly the one true violation: got [${out}]"
fi

if out="$(check_unowned_allowlist "${snap1}" "${version_allow[@]}" /etc/evil.conf)"; then
  pass "check_unowned_allowlist passes once every unowned entry, including the per-version log, is allowlisted"
else
  fail "check_unowned_allowlist passes once every unowned entry is allowlisted: got [${out}]"
fi

# ---------------------------------------------------------------------------
# sysctls_are_one
# ---------------------------------------------------------------------------

if out="$(sysctls_are_one "${snap1}")"; then
  fail "sysctls_are_one should fail: net.ipv6.conf.all.forwarding=0"
else
  [[ "${out}" == "net.ipv6.conf.all.forwarding=0" ]] && pass "sysctls_are_one reports the bad key" \
    || fail "sysctls_are_one reports the bad key: got [${out}]"
fi

snap_good_sysctl="${TESTDIR}/snap-good-sysctl"
cat >"${snap_good_sysctl}" <<'EOF'
=== sysctls ===
net.bridge.bridge-nf-call-iptables=1
net.bridge.bridge-nf-call-ip6tables=1
net.ipv4.ip_forward=1
net.ipv6.conf.all.forwarding=1
net.ipv6.conf.default.forwarding=1
EOF
if sysctls_are_one "${snap_good_sysctl}" >/dev/null; then
  pass "sysctls_are_one passes when every key is 1"
else
  fail "sysctls_are_one passes when every key is 1"
fi

# ---------------------------------------------------------------------------
# image_delta + strip_digest_refs + images_equal: reproduces reviewer case
# 2 -- images accumulate across versions (postrm never removes them), so the
# check has to compare a before/after delta, not the raw ctr set, and ctr
# gives every imported image both a tag reference and a "@sha256:..." digest
# alias that kubeadm's own list never contains.
# ---------------------------------------------------------------------------

before_images="${TESTDIR}/before-images"
cat >"${before_images}" <<'EOF'
registry.k8s.io/pause:3.10
registry.k8s.io/pause@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
EOF

after_images="${TESTDIR}/after-images"
cat >"${after_images}" <<'EOF'
registry.k8s.io/pause:3.10
registry.k8s.io/pause@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
registry.k8s.io/kube-apiserver:v1.36.3
registry.k8s.io/kube-apiserver@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
registry.k8s.io/etcd:3.6.4-0
registry.k8s.io/etcd@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
EOF

delta_raw="${TESTDIR}/delta-raw"
image_delta "${before_images}" "${after_images}" >"${delta_raw}"
want_delta_raw="registry.k8s.io/etcd:3.6.4-0
registry.k8s.io/etcd@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
registry.k8s.io/kube-apiserver:v1.36.3
registry.k8s.io/kube-apiserver@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
got_delta_raw="$(sort "${delta_raw}")"
[[ "${got_delta_raw}" == "$(sort <<<"${want_delta_raw}")" ]] && \
  pass "image_delta excludes the pre-existing pause image and includes only the newly imported ones" || \
  fail "image_delta excludes the pre-existing pause image and includes only the newly imported ones: got [${got_delta_raw}]"

delta_stripped="${TESTDIR}/delta-stripped"
strip_digest_refs "${delta_raw}" >"${delta_stripped}"
got_stripped="$(sort "${delta_stripped}")"
want_stripped="$(sort <<<"registry.k8s.io/etcd:3.6.4-0
registry.k8s.io/kube-apiserver:v1.36.3")"
[[ "${got_stripped}" == "${want_stripped}" ]] && \
  pass "strip_digest_refs drops the @sha256: aliases, leaving only tag references" || \
  fail "strip_digest_refs drops the @sha256: aliases, leaving only tag references: got [${got_stripped}]"

# A bare content-addressed entry (no registry/repo prefix, just
# "sha256:...") is also a digest reference ctr can list, not only the
# "repo@sha256:..." shape -- strip_digest_refs has to drop both.
bare_digest_input="${TESTDIR}/bare-digest-input"
printf 'registry.k8s.io/coredns:v1.13.1\nsha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd\n' >"${bare_digest_input}"
bare_digest_output="${TESTDIR}/bare-digest-output"
strip_digest_refs "${bare_digest_input}" >"${bare_digest_output}"
got_bare="$(cat "${bare_digest_output}")"
[[ "${got_bare}" == "registry.k8s.io/coredns:v1.13.1" ]] && \
  pass "strip_digest_refs also drops a bare sha256:... content-addressed entry" || \
  fail "strip_digest_refs also drops a bare sha256:... content-addressed entry: got [${got_bare}]"

expected_images="${TESTDIR}/expected-images"
printf 'registry.k8s.io/etcd:3.6.4-0\nregistry.k8s.io/kube-apiserver:v1.36.3\n' >"${expected_images}"
if images_equal "${expected_images}" "${delta_stripped}" >/dev/null; then
  pass "images_equal passes: stripped delta matches kubeadm's expected image list exactly"
else
  fail "images_equal passes: stripped delta matches kubeadm's expected image list exactly"
fi

# A second version's delta must not be satisfied by the same image kubeadm
# already reported for the first (i.e. this is a real equality check, not a
# subset check) -- drop one expected image and confirm it now fails.
expected_images_missing="${TESTDIR}/expected-images-missing"
printf 'registry.k8s.io/etcd:3.6.4-0\nregistry.k8s.io/kube-apiserver:v1.36.3\nregistry.k8s.io/coredns:v1.13.1\n' >"${expected_images_missing}"
if images_equal "${expected_images_missing}" "${delta_stripped}" >/dev/null 2>&1; then
  fail "images_equal fails when kubeadm expects an image the delta never imported"
else
  pass "images_equal fails when kubeadm expects an image the delta never imported"
fi

# ---------------------------------------------------------------------------
# filter_stale_paths
# ---------------------------------------------------------------------------

prev_owned="${TESTDIR}/prev-owned"
cat >"${prev_owned}" <<'EOF'
/etc/fstab
/usr/bin/kubeadm
/usr/bin/kubelet
/usr/bin/only-in-prev
EOF
curr_owned="${TESTDIR}/curr-owned"
cat >"${curr_owned}" <<'EOF'
/usr/bin/kubeadm
/usr/bin/kubelet
/usr/bin/only-in-curr
EOF
got="$(filter_stale_paths "${prev_owned}" "${curr_owned}" "${base_allow[@]}")"
[[ "${got}" == "/usr/bin/only-in-prev" ]] && \
  pass "filter_stale_paths finds the one stale, non-allowlisted path (/etc/fstab is allowlisted, so it's excluded)" || \
  fail "filter_stale_paths finds the one stale, non-allowlisted path: got [${got}]"

# ---------------------------------------------------------------------------
# Reviewer case 1: the remote "is this stale candidate still on disk?" loop
# boot-smoke.sh sends over ssh, reproduced verbatim (minus the ssh wrapper)
# and run under `set -e` exactly the way the plain assignment that captures
# it does. Before the "; :" fix, a passing run -- every candidate legitimately
# gone -- ended on a `[ -e "$p" ]` that returns 1, which under `set -e` would
# kill boot-smoke on its own success path.
# ---------------------------------------------------------------------------

remote_loop='while IFS= read -r p; do [ -e "$p" ] && echo "$p"; done; :'

still_present_dir="${TESTDIR}/still-present-root"
mkdir -p "${still_present_dir}"
absent_path="${still_present_dir}/does-not-exist"

set +e
(
  set -e
  # shellcheck disable=SC2016
  out="$(printf '%s\n' "${absent_path}" | bash -c "${remote_loop}")"
  : "${out}"
)
loop_rc=$?
set -e
if [[ "${loop_rc}" -eq 0 ]]; then
  pass "the remote stale-check loop (with the ; : fix) exits 0 under set -e when every candidate is absent"
else
  fail "the remote stale-check loop (with the ; : fix) exits 0 under set -e when every candidate is absent: exit ${loop_rc}"
fi

# Without the fix (dropping "; :"), the same loop should demonstrably trip
# set -e in the all-absent (passing) case -- pinning down exactly the defect
# reviewer blocker 1 called out, so a future regression that removes "; :"
# again is caught here rather than only on a real VM run.
remote_loop_unfixed='while IFS= read -r p; do [ -e "$p" ] && echo "$p"; done'
set +e
(
  set -e
  out="$(printf '%s\n' "${absent_path}" | bash -c "${remote_loop_unfixed}")"
  : "${out}"
)
unfixed_rc=$?
set -e
if [[ "${unfixed_rc}" -ne 0 ]]; then
  pass "confirms the bug: the same loop WITHOUT ; : does trip set -e in the all-absent case"
else
  fail "confirms the bug: the same loop WITHOUT ; : does trip set -e in the all-absent case (expected non-zero, got 0)"
fi

# And the loop still reports a candidate that genuinely is present.
present_path="${still_present_dir}/present-file"
: >"${present_path}"
out="$(printf '%s\n%s\n' "${absent_path}" "${present_path}" | bash -c "${remote_loop}")"
[[ "${out}" == "${present_path}" ]] && \
  pass "the remote stale-check loop still reports a candidate that is genuinely present" || \
  fail "the remote stale-check loop still reports a candidate that is genuinely present: got [${out}]"

echo
echo "passed=${PASS} failed=${FAIL}"
[[ "${FAIL}" -eq 0 ]]
