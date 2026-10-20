#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Stop the owned process group before tearing down the cluster it is driving.
# The independent grace also covers a supplied executable ignoring termination.
stop_owned_group() {
  local pid="$1"
  if [ -z "${pid}" ]; then return 0; fi
  kill -- "-${pid}" 2>/dev/null || true
  local deadline=$((SECONDS + 5))
  while kill -0 -- "-${pid}" 2>/dev/null && [ "${SECONDS}" -lt "${deadline}" ]; do
    sleep 0.1
  done
  kill -KILL -- "-${pid}" 2>/dev/null || true
  wait "${pid}" 2>/dev/null || true
}

