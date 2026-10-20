#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# A terminal marker owned by this suite, not a copied inventory of assertions.
# All interpolated values are suite-owned enums, booleans, numbers or hex hashes;
# caller paths, versions, kubeconfigs and environment values never enter JSON.
write_bootstrap_report() {
  if [ -z "${NVKECTL_E2E_REPORT:-}" ]; then return 0; fi
  local status="$1" after="$2" cleanup_status="$3" completed="$4" phase="$5" before="$6" temporary
  temporary="$(mktemp "${NVKECTL_E2E_REPORT}.XXXXXX")" || return 1
  if ! printf '{"apiVersion":"nvke.dev/bootstrap-e2e/v1","suite":"bootstrap","completed":%s,"phase":"%s","exitCode":%d,"artifactSha256Before":"%s","artifactSha256After":"%s","cleanup":"%s"}\n' \
    "${completed}" "${phase}" "${status}" \
    "${before}" "${after}" "${cleanup_status}" >"${temporary}"; then
    rm -f "${temporary}"
    return 1
  fi
  mv "${temporary}" "${NVKECTL_E2E_REPORT}"
}
