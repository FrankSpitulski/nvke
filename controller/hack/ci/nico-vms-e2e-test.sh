#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "${TEST_DIR}"' EXIT

export NKE_E2E_LIBRARY_ONLY=true
export NKE_E2E_STATE_DIR="${TEST_DIR}/state"
# shellcheck source=controller/hack/ci/nico-vms-e2e.sh
source "${SCRIPT_DIR}/nico-vms-e2e.sh"
unset NKE_E2E_LIBRARY_ONLY

mkdir -p "${STATE_DIR}" "${TEST_DIR}/profiles/osProfiles/amd64" "${TEST_DIR}/profiles/osProfiles/arm64"
CWE_PROFILES_DIR="${TEST_DIR}/profiles"
export CWE_PROFILES_DIR

for profile in amd64 arm64; do
  # The literal iPXE variable is expanded by nico-sim, not this test shell.
  # shellcheck disable=SC2016
  printf '%s\n' \
    '#!ipxe' \
    'set image-url https://example.test/node.qcow2' \
    'chain https://example.test/boot image_url=${image-url}' \
    >"${CWE_PROFILES_DIR}/osProfiles/${profile}/ipxeScript"
done

valid_image="${TEST_DIR}/node.qcow2"
qemu-img create -q -f qcow2 "${valid_image}" 1M

expect_failure() {
  local name="$1" expected="$2" path="$3" tag="${4:-}" output

  if output="$(
    (
      NKE_E2E_OS_IMAGE_PATH="${path}"
      NKE_E2E_OS_IMAGE_TAG="${tag}"
      export NKE_E2E_OS_IMAGE_PATH NKE_E2E_OS_IMAGE_TAG
      select_local_os_image
    ) 2>&1
  )"; then
    printf 'FAIL: %s unexpectedly passed\n' "${name}" >&2
    exit 1
  fi
  [[ "${output}" == *"${expected}"* ]] || {
    printf 'FAIL: %s: expected %q in output: %s\n' "${name}" "${expected}" "${output}" >&2
    exit 1
  }
  printf 'PASS: %s\n' "${name}"
}

expect_failure "relative path" "must be an absolute path" "node.qcow2"
expect_failure "missing path" "is not a regular file" "${TEST_DIR}/missing.qcow2"
expect_failure "directory path" "is not a regular file" "${TEST_DIR}"

plain_image="${TEST_DIR}/plain.img"
printf 'not a qcow2\n' >"${plain_image}"
expect_failure "non-qcow2 path" "must be qcow2" "${plain_image}"

unreadable_image="${TEST_DIR}/unreadable.qcow2"
cp "${valid_image}" "${unreadable_image}"
chmod 000 "${unreadable_image}"
expect_failure "unreadable path" "is not readable" "${unreadable_image}"
chmod 600 "${unreadable_image}"

space_image="${TEST_DIR}/node image.qcow2"
cp "${valid_image}" "${space_image}"
expect_failure "whitespace path" "cannot contain whitespace" "${space_image}"
expect_failure "path and tag conflict" "cannot both be set" "${valid_image}" "candidate"

NKE_E2E_OS_IMAGE_PATH="${valid_image}"
NKE_E2E_OS_IMAGE_TAG=""
export NKE_E2E_OS_IMAGE_PATH NKE_E2E_OS_IMAGE_TAG
select_local_os_image
repoint_os_profiles "${qcow}"

expected_hash="$(sha256sum -- "${valid_image}" | awk '{print $1}')"
grep -Fxq "${expected_hash}  ${valid_image}" "${STATE_DIR}/os-image.sha256"
for profile in amd64 arm64; do
  grep -Fxq "set image-url ${valid_image}" "${CWE_PROFILES_DIR}/osProfiles/${profile}/ipxeScript"
done
printf 'PASS: local qcow2 selection, SHA256 diagnostic, and shared profile rewrite\n'
