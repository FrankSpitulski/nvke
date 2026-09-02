#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

# Plaintext nke-agent does not use the Workload API. Default (spiffe) and
# identity-mode=controller-tls both need the SPIRE agent socket: mTLS X.509
# SVIDs and JWT-SVIDs respectively. Cloud-init still bootstraps SPIRE with
# the trust bundle and SPIRE server endpoint.
args="${AGENT_ARGS:-}"
if [[ "${args}" =~ (^|[[:space:]])--disable-mtls([[:space:]]|$) ]] ||
   [[ "${args}" =~ (^|[[:space:]])--identity-mode=plaintext([[:space:]]|$) ]] ||
   [[ "${args}" =~ (^|[[:space:]])--identity-mode[[:space:]]+plaintext([[:space:]]|$) ]] ||
   [[ "${NKE_IDENTITY_MODE:-}" == "plaintext" ]]; then
  exit 0
fi

SOCKET="${SPIFFE_SOCKET_PATH:-/tmp/spire-agent/public/api.sock}"
SOCKET="${SOCKET#unix://}"

for _ in $(seq 1 60); do
  if [[ -S "${SOCKET}" ]]; then
    exit 0
  fi
  sleep 1
done

echo "nke-agent: timed out waiting for SPIFFE workload API socket ${SOCKET}" >&2
exit 1
