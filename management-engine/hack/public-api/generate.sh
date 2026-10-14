#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

module_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
go_command=${GO:-go}
check=false
compiler_args=()
case "${1:-}" in
    '') ;;
    --check) check=true; compiler_args+=(-check) ;;
    --update-lock) compiler_args+=(-update-lock) ;;
    *) printf 'Usage: generate.sh [--check|--update-lock]\n' >&2; exit 2 ;;
esac
cd "$module_root"
"$go_command" run ./hack/public-api "${compiler_args[@]}"
export PATH="$module_root/bin:$PATH"
generated_dir=$(mktemp -d)
trap 'rm -rf "$generated_dir"' EXIT
while IFS= read -r source; do
    mkdir -p "$generated_dir/$(dirname "$source")"
    cp "$source" "$generated_dir/$source"
done < <(find public/tenant/apis -type f \( -name '*.proto' -o -name '*.yaml' \) | sort)
(
    cd "$generated_dir/public/tenant/apis"
    "$module_root/bin/buf" format --diff --exit-code
    "$module_root/bin/buf" lint
    "$module_root/bin/buf" generate
    "$module_root/bin/buf" build --as-file-descriptor-set -o "$generated_dir/descriptor.pb"
)
# grpc-gateway does not copy the proto's repository license header.
while IFS= read -r gateway_file; do
    {
        cat "$module_root/hack/boilerplate.go.txt"
        printf '\n'
        cat "$gateway_file"
    } >"$generated_dir/gateway.go"
    mv "$generated_dir/gateway.go" "$gateway_file"
done < <(find "$generated_dir/public/tenant/gen" -name '*.pb.gw.go' | sort)
artifacts() {
    find "$1" -type f \( -name '*.pb.go' -o -name '*.pb.gw.go' -o -name '*.swagger.json' \) | sort
}
while IFS= read -r generated; do
    target=${generated#"$generated_dir/"}
    if "$check"; then
        if ! cmp -s "$target" "$generated"; then
            printf 'Generated artifact differs: %s; run make generate-public-api\n' "$target" >&2
            exit 1
        fi
    else
        mkdir -p "$(dirname "$target")"
        cp "$generated" "$target"
    fi
done < <(artifacts "$generated_dir/public/tenant/gen")
while IFS= read -r target; do
    if [[ ! -f "$generated_dir/$target" ]]; then
        if "$check"; then
            printf 'Stale generated artifact: %s; run make generate-public-api\n' "$target" >&2
            exit 1
        fi
        rm "$target"
    fi
done < <(artifacts public/tenant/gen)
route_args=(-http-descriptor "$generated_dir/descriptor.pb")
if "$check"; then route_args+=(-check); fi
"$go_command" run ./hack/public-api "${route_args[@]}"
