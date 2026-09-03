#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Shell-level unit test for scripts/upgrade-kubelet-kubeadm.sh. Sources the
# script (which only defines functions when sourced) with dpkg-query/apt-get/
# apt-mark/curl/gpg/apt-key/chmod/tee/rm/mkdir/systemctl/kubectl/kubeadm/
# kubelet/crictl stubbed out via PATH, then calls the phase functions (and,
# once, main) directly and asserts on what they invoked.
#
# Every stub logs its invocation to $CALL_LOG before doing anything else, and
# none touch the real filesystem - the real script writes under /etc/apt and
# /etc/kubernetes as root, which would otherwise no-op or mutate the host.
#
# Usage: upgrade-kubelet-kubeadm_test.sh <path-to-upgrade-kubelet-kubeadm.sh>
# Exit 0 and prints PASS lines on success; exits 1 on the first failing
# assertion.

set -uo pipefail

if [ $# -ne 1 ]; then
    echo "usage: $0 <path-to-upgrade-kubelet-kubeadm.sh>" >&2
    exit 2
fi
SCRIPT_UNDER_TEST="$1"

# Captured before PATH is shadowed below, so the EXIT trap and fixture setup
# use the real rm/mkdir, not the stubs (which would leak $STUB_DIR on exit).
REAL_RM="$(command -v rm)"
REAL_MKDIR="$(command -v mkdir)"

STUB_DIR="$(mktemp -d)"
trap '"$REAL_RM" -rf "$STUB_DIR"' EXIT

CALL_LOG="$STUB_DIR/calls.log"
ENV_LOG="$STUB_DIR/env.log"
# One fixture directory per apt-sources scenario, avoiding delete-and-recreate.
"$REAL_MKDIR" -p "$STUB_DIR/apt-sources-local" "$STUB_DIR/apt-sources-empty"
: >"$STUB_DIR/apt-sources-local/nke-local.list"
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

# dpkg-query stub: prints DPKG_QUERY_OUTPUT verbatim, driving is_distro_node()
# through its "installed"/"rc"/half-configured paths without a real dpkg db.
make_stub dpkg-query 'if [ -n "${DPKG_QUERY_OUTPUT:-}" ]; then echo "$DPKG_QUERY_OUTPUT"; fi
exit 0'
make_stub apt-get 'echo "DEBIAN_FRONTEND=${DEBIAN_FRONTEND:-}" >> "'"$ENV_LOG"'"
exit 0'
make_stub apt-mark 'exit 0'
make_stub apt-key 'exit 0'
make_stub curl 'exit 0'
# gpg stub: honors "-o <file>" by writing under $STUB_DIR instead of the real
# path. Not built with make_stub, which logs before reading stdin - gpg is
# the read end of `curl | gpg`, and logging only after `cat >/dev/null`
# ensures the call log order matches the pipeline's data-flow order rather
# than an unrelated process-start race with curl.
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
# kubectl stub: every call succeeds except "get --raw /readyz", which fails
# for the first KUBECTL_READYZ_FAIL_COUNT polls (counted via a fixture file,
# since wait_for_control_plane_ready() polls in a loop).
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
# Stubbed so the polling loops don't call real sleep - harmless but slow.
make_stub sleep 'exit 0'

export PATH="$STUB_DIR:$PATH"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

# Guard: fail loudly if any name resolves to something other than our stub,
# rather than silently exercising the real command in the assertions below.
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
# Sourcing runs the script's own `set -e`; turn it back off so a passing
# assertion (e.g. grep -qF finding nothing, as expected) doesn't abort the test.
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

# --- Case 2: distro node, Phase 1, neither local repo exists - hard error ---
reset_log
# Run in a subshell: update_packages's error() calls exit, which must not
# end this test script.
(
    DPKG_QUERY_OUTPUT="$DPKG_QUERY_INSTALLED" APT_SOURCES_DIR="$STUB_DIR/apt-sources-empty" update_packages
) >"$STUB_DIR/out2.log" 2>&1
rc=$?

if [ "$rc" -eq 0 ]; then
    fail "expected update_packages to fail when no local NKE repo exists"
fi
if ! grep -q "ERROR: No local NKE apt repository found" "$STUB_DIR/out2.log"; then
    fail "expected a clear error message when no local NKE repo exists"
fi
echo "PASS: distro node - Phase 1 errors out when no local NKE repo is configured"

# --- Case 3: distro node, Phase 3 (update_kubelet_packages) ---
reset_log
DPKG_QUERY_OUTPUT="$DPKG_QUERY_INSTALLED" update_kubelet_packages >"$STUB_DIR/out3.log" 2>&1

assert_not_contains "apt-mark unhold kubelet kubectl"
assert_not_contains "apt-get "
assert_not_contains "apt-mark hold"
echo "PASS: distro node - Phase 3 skips the apt install and hold steps"

# --- Case 4: "rc" (config-files) and "half-configured" dpkg-query rows - neither is a distro node ---
reset_log
VERSION=1.30.5 parse_inputs
DPKG_QUERY_OUTPUT="config-files nke-kubeadm-distro-1.35.3" APT_SOURCES_DIR="$STUB_DIR/apt-sources-local" update_packages >"$STUB_DIR/out4a.log" 2>&1

assert_contains "curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.30/deb/Release.key"
assert_not_contains "apt-get install -qqy -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold nke-kubeadm-distro"
echo "PASS: a removed-but-not-purged (rc) nke-kubeadm-distro row is NOT treated as a distro node"

reset_log
DPKG_QUERY_OUTPUT="half-configured nke-kubeadm-distro-1.35.3" APT_SOURCES_DIR="$STUB_DIR/apt-sources-local" update_packages >"$STUB_DIR/out4b.log" 2>&1

assert_contains "curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.30/deb/Release.key"
assert_not_contains "apt-get install -qqy -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold nke-kubeadm-distro"
echo "PASS: a half-configured nke-kubeadm-distro row is NOT treated as a distro node"

# --- Case 5: non-distro node, Phase 1 (update_packages) - exact previous sequence ---
reset_log
VERSION=1.30.5 parse_inputs
update_packages >"$STUB_DIR/out5.log" 2>&1

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

# --- Case 6: non-distro node, Phase 3 (update_kubelet_packages) - exact previous sequence ---
reset_log
update_kubelet_packages >"$STUB_DIR/out6.log" 2>&1

cat >"$STUB_DIR/phase3.golden" <<'EOF'
dpkg-query -W -f=${db:Status-Status} ${Package}\n nke-kubeadm-distro-*
apt-mark unhold kubelet kubectl
apt-get update -o Dir::Etc::sourcelist=sources.list.d/kubernetes.list -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 -qq
apt-get install -qqy kubelet=1.30.5-* kubectl=1.30.5-*
apt-mark hold kubelet kubectl
EOF
assert_golden "$STUB_DIR/phase3.golden"
echo "PASS: non-distro node - Phase 3 behaves exactly as before"

# --- Case 7: full main() run, non-distro, cordon+drain - drain precedes the first apt-get call ---
reset_log
(
    CORDON_NODE=true DRAIN_NODE=true NODE_NAME=test-node VERSION=1.31.2 \
        KUBEADM_PATCHES_DIR="$STUB_DIR/patches" main
) >"$STUB_DIR/out7.log" 2>&1
# main() may exit non-zero here (verify_upgrade's stubbed kubelet always
# reports 1.30.5, never matching VERSION=1.31.2) - only call ordering matters.

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

# --- Case 8: distro node, IS_MASTER=true - waits for /readyz, retrying past failures ---
reset_log
: >"$STUB_DIR/readyz-calls"
# export, not a parse_inputs prefix: bash reverts prefix assignments once the
# call returns, which would leave IS_MASTER at whatever an earlier case set.
export IS_MASTER=true
VERSION=1.30.5 parse_inputs
KUBECTL_READYZ_FAIL_COUNT=2 DPKG_QUERY_OUTPUT="$DPKG_QUERY_INSTALLED" APT_SOURCES_DIR="$STUB_DIR/apt-sources-local" \
    update_packages >"$STUB_DIR/out8.log" 2>&1
rc=$?

if [ "$rc" -ne 0 ]; then
    cat "$STUB_DIR/out8.log" >&2
    fail "expected update_packages to succeed once /readyz starts passing"
fi
readyz_calls="$(cat "$STUB_DIR/readyz-calls")"
if [ "$readyz_calls" -lt 3 ]; then
    fail "expected at least 3 readyz polls (2 failures + 1 success), got $readyz_calls"
fi
assert_contains "kubectl --kubeconfig /etc/kubernetes/admin.conf get --raw /readyz"
echo "PASS: distro control-plane node polls /readyz until the control plane recovers"

# --- Case 9: distro node, IS_MASTER=false - no /readyz call at all ---
reset_log
: >"$STUB_DIR/readyz-calls"
export IS_MASTER=false
VERSION=1.30.5 parse_inputs
DPKG_QUERY_OUTPUT="$DPKG_QUERY_INSTALLED" APT_SOURCES_DIR="$STUB_DIR/apt-sources-local" update_packages >"$STUB_DIR/out9.log" 2>&1

assert_not_contains "readyz"
echo "PASS: distro worker node never polls /readyz"

echo "PASS: all upgrade-kubelet-kubeadm.sh cases"
