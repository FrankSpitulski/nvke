#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Shell-level unit test for scripts/upgrade-kubelet-kubeadm.sh. Sources the
# script (which only defines functions when sourced, see the BASH_SOURCE
# guard at its end) with dpkg-query/apt-get/apt-mark/curl/gpg/apt-key/chmod/
# tee/rm/mkdir/systemctl/kubectl/kubeadm/kubelet/crictl stubbed out via PATH,
# then calls the individual phase functions (and, once, main) directly and
# asserts on what they invoked.
#
# Every stub is a script under $STUB_DIR that appends its invocation to
# $CALL_LOG before doing anything else. None of them touch the real
# filesystem: `gpg -o <path>` writes to $STUB_DIR/<basename> instead of
# <path>, and rm/mkdir/chmod/tee are no-ops. This matters because the real
# script writes under /etc/apt and /etc/kubernetes as root; on a developer
# machine (no root) or in CI (root, but a real system) either an unstubbed
# command silently no-ops on a permission error or, worse, actually mutates
# the host's apt state.
#
# Usage: upgrade-kubelet-kubeadm_test.sh <path-to-upgrade-kubelet-kubeadm.sh>
# Exit 0 and prints PASS lines on success; exits 1 with a message on the
# first failing assertion.

set -uo pipefail

if [ $# -ne 1 ]; then
    echo "usage: $0 <path-to-upgrade-kubelet-kubeadm.sh>" >&2
    exit 2
fi
SCRIPT_UNDER_TEST="$1"

# Captured before PATH is shadowed below (and before this harness creates any
# fixtures), as absolute paths, so:
#  - the EXIT trap that cleans up $STUB_DIR always resolves to the real rm,
#    never the "rm" stub the script under test sees on the shadowed PATH -
#    otherwise the stub silently no-ops and every run leaks a temp dir.
#  - the harness's own fixture setup can't be broken by some other "mkdir"
#    shim already inherited on PATH before this script even started.
REAL_RM="$(command -v rm)"
REAL_MKDIR="$(command -v mkdir)"

STUB_DIR="$(mktemp -d)"
trap '"$REAL_RM" -rf "$STUB_DIR"' EXIT

CALL_LOG="$STUB_DIR/calls.log"
ENV_LOG="$STUB_DIR/env.log"
# One fixture directory per apt-sources scenario, so the harness never has to
# delete-and-recreate a shared directory (which would need the stubbed rm).
"$REAL_MKDIR" -p "$STUB_DIR/apt-sources-local" "$STUB_DIR/apt-sources-remote" "$STUB_DIR/apt-sources-empty"
: >"$STUB_DIR/apt-sources-local/nke-local.list"
: >"$STUB_DIR/apt-sources-remote/nke.list"
: >"$CALL_LOG"
: >"$ENV_LOG"

make_stub() {
    local name="$1" body="$2"
    cat >"$STUB_DIR/$name" <<STUB
#!/usr/bin/env bash
echo "$name \$*" >> "$CALL_LOG"
$body
STUB
    chmod +x "$STUB_DIR/$name"
}

# dpkg-query stub: prints DPKG_QUERY_OUTPUT verbatim (a caller-controlled
# "<db:Status-Status> <Package>" line, or nothing) so tests can drive
# is_distro_node() through both its "installed" and "rc"/half-configured
# code paths without a real dpkg database.
make_stub dpkg-query 'if [ -n "${DPKG_QUERY_OUTPUT:-}" ]; then echo "$DPKG_QUERY_OUTPUT"; fi
exit 0'
make_stub apt-get 'echo "DEBIAN_FRONTEND=${DEBIAN_FRONTEND:-}" >> "'"$ENV_LOG"'"
exit 0'
make_stub apt-mark 'exit 0'
make_stub apt-key 'exit 0'
make_stub curl 'exit 0'
# gpg stub: honors "-o <file>" like the real `gpg --dearmor -o <file>`, but
# writes under $STUB_DIR instead of the real (root-owned, possibly
# nonexistent on a dev box) path, so the script's later `chmod` has
# something to operate on without touching the real filesystem.
#
# Deliberately NOT built with make_stub: that helper logs the invocation as
# its first statement, before the stub does anything else. gpg is the read
# end of `curl ... | gpg ...` - if it logged first, its log line could win a
# race against curl's own log line (both processes start at roughly the same
# time; nothing before this line depends on curl at all). Reading stdin to
# EOF first can only complete once curl has exited (and thus already
# logged), so logging after `cat >/dev/null` makes the call log's order
# match the real pipeline's data-flow order, not an unrelated process-start
# race.
cat >"$STUB_DIR/gpg" <<STUB
#!/usr/bin/env bash
args="\$*"
out=""
while [ \$# -gt 0 ]; do
    if [ "\$1" = "-o" ]; then out="\$2"; fi
    shift
done
cat >/dev/null
echo "gpg \$args" >> "$CALL_LOG"
if [ -n "\$out" ]; then
    : >"$STUB_DIR/\$(basename "\$out")"
fi
exit 0
STUB
chmod +x "$STUB_DIR/gpg"
make_stub chmod 'exit 0'
make_stub tee 'cat >/dev/null
exit 0'
make_stub rm 'exit 0'
make_stub mkdir 'exit 0'
make_stub systemctl 'exit 0'
# kubectl stub: every call succeeds except "get --raw /readyz", which is
# driven by KUBECTL_READYZ_FAIL_COUNT - it counts its own invocations (via a
# per-fixture-dir counter file, since wait_for_control_plane_ready() polls in
# a loop) and fails for the first KUBECTL_READYZ_FAIL_COUNT of them.
make_stub kubectl 'if printf "%s" "$*" | grep -q "readyz"; then
    n=0
    if [ -f "'"$STUB_DIR"'/readyz-calls" ]; then n=$(cat "'"$STUB_DIR"'/readyz-calls"); fi
    n=$((n + 1))
    echo "$n" > "'"$STUB_DIR"'/readyz-calls"
    if [ "$n" -le "${KUBECTL_READYZ_FAIL_COUNT:-0}" ]; then
        exit 1
    fi
fi
exit 0'
make_stub kubeadm 'exit 0'
make_stub kubelet 'echo "Kubernetes v1.30.5"
exit 0'
make_stub crictl 'exit 0'
make_stub ctr 'exit 0'
# restart_kubelet's stabilization pause and the wait_for_containerd /
# wait_for_control_plane_ready polling loops all call real sleep otherwise -
# harmless but slow (the package test went from ~3.5s to ~11s before this).
make_stub sleep 'exit 0'

export PATH="$STUB_DIR:$PATH"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

# Guard: if any of these names resolve to something other than our stub (a
# real binary earlier on PATH, a shell builtin, PATH not actually exported
# before the lookup, etc.) the assertions below would silently exercise the
# real command instead of the fake one. Fail fast and loud instead.
STUBBED_COMMANDS=(dpkg-query apt-get apt-mark apt-key curl gpg chmod tee rm mkdir systemctl kubectl kubeadm kubelet crictl ctr sleep)
for cmd in "${STUBBED_COMMANDS[@]}"; do
    resolved="$(command -v "$cmd" 2>/dev/null || true)"
    if [ "$resolved" != "$STUB_DIR/$cmd" ]; then
        echo "FATAL: PATH shadowing broken for '$cmd': resolved to '${resolved:-<not found>}', expected '$STUB_DIR/$cmd'" >&2
        exit 1
    fi
done

assert_contains() {
    if ! grep -qF "$1" "$CALL_LOG"; then
        fail "expected call log to contain: $1"
    fi
}

assert_not_contains() {
    if grep -qF "$1" "$CALL_LOG"; then
        fail "expected call log NOT to contain: $1"
    fi
}

assert_golden() {
    local expected_file="$1"
    if ! diff -u "$expected_file" "$CALL_LOG" >"$STUB_DIR/diff.out" 2>&1; then
        cat "$STUB_DIR/diff.out" >&2
        fail "call log did not match the expected sequence (diff above)"
    fi
}

reset_log() {
    : >"$CALL_LOG"
    : >"$ENV_LOG"
}

# shellcheck source=/dev/null
source "$SCRIPT_UNDER_TEST"
# Sourcing runs the script's own top-level `set -e`. Turn it back off: this
# harness uses explicit `if`/return-code checks (assert_contains et al.), not
# `&&`/`||` short-circuiting, so errexit here would only make a *passing*
# assertion (grep -qF finding nothing, as expected) abort the whole test.
set +e

DPKG_QUERY_INSTALLED="installed nke-kubeadm-distro-1.29.9"

# --- Case 1: distro node, Phase 1 (update_packages) ---
reset_log
VERSION=1.30.5 parse_inputs
DPKG_QUERY_OUTPUT="$DPKG_QUERY_INSTALLED" APT_SOURCES_DIR="$STUB_DIR/apt-sources-local" update_packages >"$STUB_DIR/out1.log" 2>&1

assert_contains "apt-get update -o Dir::Etc::sourcelist=$STUB_DIR/apt-sources-local/nke-local.list -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 -qq"
assert_contains "apt-mark unhold kubeadm kubelet kubectl"
assert_contains "apt-get install -qqy -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold nke-kubeadm-distro-1.30.5"
assert_contains "systemctl is-active --quiet containerd"
assert_contains "crictl --runtime-endpoint unix:///run/containerd/containerd.sock info"
assert_not_contains "curl "
assert_not_contains "apt-key "
assert_not_contains "apt-mark hold"
assert_not_contains "pkgs.k8s.io"
assert_not_contains "sources.list.d/kubernetes.list"
if ! grep -qF "DEBIAN_FRONTEND=noninteractive" "$ENV_LOG"; then
    fail "expected the distro install to set DEBIAN_FRONTEND=noninteractive"
fi
echo "PASS: distro node - Phase 1 installs from the local NKE repo, skips pkgs.k8s.io"

# --- Case 2: distro node, Phase 1, remote nke.list instead of nke-local.list ---
reset_log
DPKG_QUERY_OUTPUT="$DPKG_QUERY_INSTALLED" APT_SOURCES_DIR="$STUB_DIR/apt-sources-remote" update_packages >"$STUB_DIR/out2.log" 2>&1

assert_contains "apt-get update -o Dir::Etc::sourcelist=$STUB_DIR/apt-sources-remote/nke.list -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 -qq"
assert_not_contains "nke-local.list"
echo "PASS: distro node - Phase 1 falls back to nke.list when nke-local.list is absent"

# --- Case 3: distro node, Phase 1, neither local repo exists - hard error ---
reset_log
# update_packages calls error(), which calls exit - run it in a subshell so
# that exit only ends the subshell, not this test script.
(
    DPKG_QUERY_OUTPUT="$DPKG_QUERY_INSTALLED" APT_SOURCES_DIR="$STUB_DIR/apt-sources-empty" update_packages
) >"$STUB_DIR/out3.log" 2>&1
rc=$?

if [ "$rc" -eq 0 ]; then
    fail "expected update_packages to fail when no local NKE repo exists"
fi
if ! grep -q "ERROR: No local NKE apt repository found" "$STUB_DIR/out3.log"; then
    fail "expected a clear error message when no local NKE repo exists"
fi
echo "PASS: distro node - Phase 1 errors out when no local NKE repo is configured"

# --- Case 4: distro node, Phase 3 (update_kubelet_packages) ---
reset_log
DPKG_QUERY_OUTPUT="$DPKG_QUERY_INSTALLED" update_kubelet_packages >"$STUB_DIR/out4.log" 2>&1

assert_not_contains "apt-mark unhold kubelet kubectl"
assert_not_contains "apt-get "
assert_not_contains "apt-mark hold"
echo "PASS: distro node - Phase 3 skips the apt install and hold steps"

# --- Case 5: dpkg-query reports an "rc" (removed, config files remain) row - NOT a distro node ---
# db:Status-Status for "rc" is "config-files" ("deinstall" is Status-Want,
# a different dpkg-query field entirely).
reset_log
VERSION=1.30.5 parse_inputs
DPKG_QUERY_OUTPUT="config-files nke-kubeadm-distro-1.35.3" APT_SOURCES_DIR="$STUB_DIR/apt-sources-local" update_packages >"$STUB_DIR/out5.log" 2>&1

assert_contains "curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.30/deb/Release.key"
assert_not_contains "apt-get install -qqy -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold nke-kubeadm-distro"
echo "PASS: a removed-but-not-purged (rc) nke-kubeadm-distro row is NOT treated as a distro node"

# --- Case 5b: dpkg-query reports a "half-configured" row - NOT a distro node ---
reset_log
VERSION=1.30.5 parse_inputs
DPKG_QUERY_OUTPUT="half-configured nke-kubeadm-distro-1.35.3" APT_SOURCES_DIR="$STUB_DIR/apt-sources-local" update_packages >"$STUB_DIR/out5b.log" 2>&1

assert_contains "curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.30/deb/Release.key"
assert_not_contains "apt-get install -qqy -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold nke-kubeadm-distro"
echo "PASS: a half-configured nke-kubeadm-distro row is NOT treated as a distro node"

# --- Case 6: non-distro node, Phase 1 (update_packages) - exact previous sequence ---
reset_log
VERSION=1.30.5 parse_inputs
update_packages >"$STUB_DIR/out6.log" 2>&1

cat >"$STUB_DIR/phase1.golden" <<'EOF'
dpkg-query -W -f=${db:Status-Status} ${Package}\n nke-kubeadm-distro-*
apt-mark unhold kubeadm
rm -f /etc/apt/keyrings/kubernetes-apt-keyring.gpg
rm -f /etc/apt/sources.list.d/kubernetes.list
curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.30/deb/Release.key
gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
chmod 644 /etc/apt/keyrings/kubernetes-apt-keyring.gpg
apt-key add /etc/apt/keyrings/kubernetes-apt-keyring.gpg
tee /etc/apt/sources.list.d/kubernetes.list
apt-get update -o Dir::Etc::sourcelist=sources.list.d/kubernetes.list -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 -qq
apt-get install -qqy kubeadm=1.30.5-*
apt-mark hold kubeadm
EOF
assert_golden "$STUB_DIR/phase1.golden"
echo "PASS: non-distro node - Phase 1 behaves exactly as before"

# --- Case 7: non-distro node, Phase 3 (update_kubelet_packages) - exact previous sequence ---
reset_log
update_kubelet_packages >"$STUB_DIR/out7.log" 2>&1

cat >"$STUB_DIR/phase3.golden" <<'EOF'
dpkg-query -W -f=${db:Status-Status} ${Package}\n nke-kubeadm-distro-*
apt-mark unhold kubelet kubectl
apt-get update -o Dir::Etc::sourcelist=sources.list.d/kubernetes.list -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 -qq
apt-get install -qqy kubelet=1.30.5-* kubectl=1.30.5-*
apt-mark hold kubelet kubectl
EOF
assert_golden "$STUB_DIR/phase3.golden"
echo "PASS: non-distro node - Phase 3 behaves exactly as before"

# --- Case 8: full main() run, non-distro, cordon+drain - drain precedes the first apt-get call ---
reset_log
(
    CORDON_NODE=true DRAIN_NODE=true NODE_NAME=test-node VERSION=1.31.2 \
        KUBEADM_PATCHES_DIR="$STUB_DIR/patches" main
) >"$STUB_DIR/out8.log" 2>&1
# main() may exit non-zero past this point (verify_upgrade's kubelet stub
# always reports 1.30.5, which will not match VERSION=1.31.2 here) - this
# case only cares about call ordering up through Phase 1, not the final rc.

drain_line=$(grep -n '^kubectl drain ' "$CALL_LOG" | head -1 | cut -d: -f1)
first_apt_line=$(grep -n '^apt-get ' "$CALL_LOG" | head -1 | cut -d: -f1)
if [ -z "$drain_line" ]; then
    fail "expected a kubectl drain call in the call log"
fi
if [ -z "$first_apt_line" ]; then
    fail "expected at least one apt-get call in the call log"
fi
if [ "$drain_line" -ge "$first_apt_line" ]; then
    fail "expected kubectl drain (line $drain_line) to precede the first apt-get call (line $first_apt_line)"
fi
echo "PASS: main() drains the node before touching packages"

# --- Case 9: distro node, IS_MASTER=true - waits for /readyz, retrying past failures ---
reset_log
: >"$STUB_DIR/readyz-calls"
# IS_MASTER is a plain (persistent) assignment, not a prefix on parse_inputs:
# bash reverts prefix assignments to their pre-call value once the command
# (here, the parse_inputs function call) returns, which would silently put
# IS_MASTER back to whatever an earlier case left it at.
export IS_MASTER=true
VERSION=1.30.5 parse_inputs
KUBECTL_READYZ_FAIL_COUNT=2 DPKG_QUERY_OUTPUT="$DPKG_QUERY_INSTALLED" APT_SOURCES_DIR="$STUB_DIR/apt-sources-local" \
    update_packages >"$STUB_DIR/out9.log" 2>&1
rc=$?

if [ "$rc" -ne 0 ]; then
    cat "$STUB_DIR/out9.log" >&2
    fail "expected update_packages to succeed once /readyz starts passing"
fi
readyz_calls="$(cat "$STUB_DIR/readyz-calls")"
if [ "$readyz_calls" -lt 3 ]; then
    fail "expected at least 3 readyz polls (2 failures + 1 success), got $readyz_calls"
fi
assert_contains "kubectl --kubeconfig /etc/kubernetes/admin.conf get --raw /readyz"
echo "PASS: distro control-plane node polls /readyz until the control plane recovers"

# --- Case 10: distro node, IS_MASTER=false - no /readyz call at all ---
reset_log
: >"$STUB_DIR/readyz-calls"
export IS_MASTER=false
VERSION=1.30.5 parse_inputs
DPKG_QUERY_OUTPUT="$DPKG_QUERY_INSTALLED" APT_SOURCES_DIR="$STUB_DIR/apt-sources-local" update_packages >"$STUB_DIR/out10.log" 2>&1

assert_not_contains "readyz"
echo "PASS: distro worker node never polls /readyz"

echo "PASS: all upgrade-kubelet-kubeadm.sh cases"
