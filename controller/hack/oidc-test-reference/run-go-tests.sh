#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

mode="${1:-}"
[[ "${2:-}" == "--" ]] || {
  printf 'usage: %s <any|reference|wiring> -- <go test arguments...>\n' "$0" >&2
  exit 2
}
shift 2
case "${mode}" in
  any | reference | wiring) ;;
  *)
    printf 'unsupported OIDC test selection %q\n' "${mode}" >&2
    exit 2
    ;;
esac

result="$(mktemp "${TMPDIR:-/tmp}/nke-oidc-go-test.XXXXXX.json")"
trap 'rm -f "${result}"' EXIT
set +e
go test -json "$@" >"${result}"
go_status=$?
set -e

python3 - "${mode}" "${result}" "${go_status}" <<'PY'
import json
import re
import sys

mode, path, status_text = sys.argv[1:]
status = int(status_text)
passed = set()
with open(path, encoding="utf-8") as stream:
    for line in stream:
        event = json.loads(line)
        if event.get("Action") == "output":
            sys.stdout.write(event.get("Output", ""))
        if event.get("Action") == "pass" and event.get("Test"):
            passed.add(event["Test"])

if status:
    raise SystemExit(status)
if not passed:
    sys.stderr.write("OIDC Go test selection completed no passing tests; check GOFLAGS, -run filters, and skips\n")
    raise SystemExit(1)
expected = set()
observed = set()
selection_name = ""
if mode == "reference":
    selection_name = "OIDC reference"
    expected = {f"SNAP-OIDC-{number}" for number in ("001", "002", "003")}
    observed = {match.group(1) for name in passed if (match := re.search(r"/(SNAP-OIDC-[0-9]+)$", name))}
elif mode == "wiring":
    selection_name = "OIDC snapshot wiring"
    expected = {
        "TestOIDCSnapshotWiringSharedProviderControl",
        "TestOIDCSnapshotWiringJoinWithPinnedEmptyProvidersKeepsSharedProvider",
        "TestOIDCSnapshotWiringInitUsesPinnedProviders",
        "TestOIDCSnapshotWiringInitStopsOnPinError",
        "TestOIDCSnapshotWiringJoinUsesPinnedProviders",
    }
    observed = {name for name in passed if name.startswith("TestOIDCSnapshotWiring")}

if expected:
    if observed != expected:
        missing = ", ".join(sorted(expected - observed)) or "none"
        extra = ", ".join(sorted(observed - expected)) or "none"
        sys.stderr.write(
            f"{selection_name} selection incomplete: selected {len(observed)} of {len(expected)} cases; "
            f"missing: {missing}; unexpected: {extra}\n"
        )
        raise SystemExit(1)
PY
