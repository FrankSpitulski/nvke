#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

mode="${1:-}"
[[ "${2:-}" == "--" ]] || {
  printf 'usage: %s <any|reference> -- <go test arguments...>\n' "$0" >&2
  exit 2
}
shift 2
case "${mode}" in
  any | reference) ;;
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
selected = set()
with open(path, encoding="utf-8") as stream:
    for line in stream:
        event = json.loads(line)
        if event.get("Action") == "output":
            sys.stdout.write(event.get("Output", ""))
        if event.get("Action") == "run" and event.get("Test"):
            selected.add(event["Test"])

if status:
    raise SystemExit(status)
if not selected:
    sys.stderr.write("OIDC Go test selection is empty; check GOFLAGS and -run filters\n")
    raise SystemExit(1)
if mode == "reference":
    expected = {f"REF-OIDC-{number}" for number in ("003", "030", "031")}
    observed = {match.group(1) for name in selected if (match := re.search(r"/(REF-OIDC-[0-9]+)$", name))}
    if observed != expected:
        missing = ", ".join(sorted(expected - observed)) or "none"
        extra = ", ".join(sorted(observed - expected)) or "none"
        sys.stderr.write(
            f"OIDC reference selection incomplete: selected {len(observed)} of {len(expected)} cases; "
            f"missing: {missing}; unexpected: {extra}\n"
        )
        raise SystemExit(1)
PY
