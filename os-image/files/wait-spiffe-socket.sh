#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

# Plaintext nke-agent (--disable-mtls) does not use the Workload API.
# Otherwise wait for the SPIRE agent socket. Cloud-init plants the SPIRE
# server trust bundle and agent.conf (including the node attestor); nke-agent
# only consumes the Workload API X.509 SVID.
args="${AGENT_ARGS:-}"
if [[ "${args}" =~ (^|[[:space:]])--disable-mtls([[:space:]]|$) ]]; then
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
