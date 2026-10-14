#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "$*" >&2; exit 1; }
: "${DEV_NAME:=nvke}"
[[ "$DEV_NAME" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ && ${#DEV_NAME} -le 32 ]] || fail "DEV_NAME must be a lowercase DNS label of at most 32 characters."
dev_dir="$PWD/.local/dev/$DEV_NAME"
export KUBECONFIG="$dev_dir/kubeconfig"
export HOST_CONTEXT="kind-$DEV_NAME-management" STATE_CONTEXT="kind-$DEV_NAME-state"
export HOST_NAMESPACE=management-engine-system STATE_NAMESPACE=management-state-system
host=(kubectl --context "$HOST_CONTEXT" --request-timeout=30s)
state=(kubectl --context "$STATE_CONTEXT" --request-timeout=30s)
clusters=("$DEV_NAME-management" "$DEV_NAME-state")
umask 077
: "${DEV_PUBLIC_API:=1}"
[[ "$DEV_PUBLIC_API" == 0 || "$DEV_PUBLIC_API" == 1 ]] || fail "DEV_PUBLIC_API must be 0 or 1."

api_namespace() {
  if [[ ${DEV_CWE:-} == 1 || -f "$dev_dir/cwe/owned" ]]; then printf '%s' cwe-integration; else printf '%s' org-example; fi
}

sync_api_state() {
  [[ "$DEV_PUBLIC_API" == 1 ]] || return 0
  local overlay=config/api/state
  if [[ ${DEV_CWE:-} == 1 || -f "$dev_dir/cwe/owned" ]]; then overlay=config/api/cwe-state; fi
  make install-api-state STATE_CONTEXT="$STATE_CONTEXT" API_STATE_OVERLAY="$overlay"
}

check_tilt() {
  local version
  command -v tilt >/dev/null || fail "Tilt 0.36+ is required. Use the shared toolchain described in DEVELOPMENT.md."
  version=$(tilt version)
  [[ "$version" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+) ]] || fail "Cannot read Tilt version: $version"
  (( BASH_REMATCH[1] > 0 || BASH_REMATCH[2] >= 36 )) ||
    fail "Tilt 0.36+ is required; found $version at $(command -v tilt). Use the shared toolchain described in DEVELOPMENT.md."
  [[ -z "${DEV_VALUES:-}" || -r "$DEV_VALUES" ]] ||
    fail "DEV_VALUES=$DEV_VALUES is not readable. Configure a local copy of config/samples/cwe-values.yaml, or omit DEV_VALUES to run without CWE."
}

start_tilt() {
  local tiltfile=$1 port=$2 running_file running_context url
  tiltfile="$(cd "$(dirname "$tiltfile")" && pwd)/$(basename "$tiltfile")"
  url="http://${TILT_HOST:-localhost}:$port/"
  if running_file=$(tilt get tiltfile '(Tiltfile)' --port "$port" -o 'jsonpath={.spec.path}' 2>/dev/null); then
    running_context=$(tilt get cluster default --port "$port" -o 'jsonpath={.status.connection.kubernetes.context}')
    [[ "$running_file" == "$tiltfile" && "$running_context" == "$HOST_CONTEXT" ]] ||
      fail "Tilt at $url belongs to a different Tiltfile or Kubernetes context. Stop that session or choose another TILT_PORT."
    echo "Tilt is already running for $HOST_CONTEXT at $url"
    if command -v open >/dev/null; then
      open "$url" || true
    elif command -v xdg-open >/dev/null; then
      xdg-open "$url" || true
    fi
    return
  fi
  exec tilt up --file "$tiltfile" --context "$HOST_CONTEXT" --port "$port"
}

require_cluster() {
  local cluster=$1 nodes
  nodes=$(kind get nodes --name "$cluster")
  [[ -n "$nodes" ]] || fail "$cluster is missing. Complete setup with make dev-up DEV_NAME=$DEV_NAME before running this command."
  [[ "$nodes" == "$cluster-control-plane" ]] || fail "$cluster must have exactly one control-plane node."
}

connect() (
  local cluster host_uid state_uid scratch
  for cluster in "${clusters[@]}"; do require_cluster "$cluster"; done
  mkdir -p "$dev_dir"
  scratch=$(mktemp "$dev_dir/kubeconfig.XXXXXX")
  trap 'rm -f "$scratch"' EXIT
  for cluster in "${clusters[@]}"; do
    kind export kubeconfig --name "$cluster" --kubeconfig "$scratch"
  done
  # Publish atomically: concurrent CWE commands must never see the state context.
  kubectl --kubeconfig "$scratch" config use-context "$HOST_CONTEXT" >/dev/null
  mv "$scratch" "$KUBECONFIG"
  host_uid=$("${host[@]}" get namespace kube-system -o jsonpath='{.metadata.uid}')
  state_uid=$("${state[@]}" get namespace kube-system -o jsonpath='{.metadata.uid}')
  [[ -n "$host_uid" && -n "$state_uid" && "$host_uid" != "$state_uid" ]] || fail "Host and state must resolve to distinct clusters."
)

sync_state() {
  make install-state STATE_CONTEXT="$STATE_CONTEXT"
  "${state[@]}" wait --for=condition=Established --timeout=60s -f config/crd/bases
  sync_api_state
}

refresh_identity() (
  local identity_namespace=$1 identity=$2 secret_name=$3 mount_path=$4 page_key=${5:-}
  local node address ca token_response token expiration scratch
  "${host[@]}" create namespace "$HOST_NAMESPACE" --dry-run=client -o yaml | "${host[@]}" apply -f -
  # Pods use the state node's Docker IP and verify TLS against its certificate hostname.
  node="$DEV_NAME-state-control-plane"
  address=$(docker inspect --format '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' "$node")
  [[ -n "$address" ]] || fail "State node has no IPv4 address on Docker's kind network."
  ca=$("${state[@]}" config view --raw --minify --flatten -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
  [[ -n "$ca" ]] || fail "State kubeconfig must supply a certificate authority."
  scratch=$(mktemp -d "$dev_dir/credentials.XXXXXX")
  trap 'rm -rf "$scratch"' EXIT
  config=(kubectl --kubeconfig "$scratch/kubeconfig" config)
  "${config[@]}" set-cluster management-state --server="https://$address:6443" --tls-server-name="$node" >/dev/null
  "${config[@]}" set clusters.management-state.certificate-authority-data "$ca" >/dev/null
  # client-go rereads this file when the mounted Secret is refreshed.
  "${config[@]}" set "users.$identity.tokenFile" "$mount_path/token" >/dev/null
  "${config[@]}" set-context management-state --cluster=management-state --user="$identity" >/dev/null
  "${config[@]}" use-context management-state >/dev/null
  token_response=$("${state[@]}" -n "$identity_namespace" create token "$identity" \
    --duration=24h -o jsonpath='{.status.token}{"\n"}{.status.expirationTimestamp}')
  token=${token_response%%$'\n'*}
  expiration=${token_response#*$'\n'}
  [[ -n "$token" && -n "$expiration" && "$token" != "$expiration" ]] || fail "Token response is missing a token or expiration."
  printf '%s' "$token" >"$scratch/token"
  local files=(--from-file=kubeconfig="$scratch/kubeconfig" --from-file=token="$scratch/token")
  if [[ -n "$page_key" ]]; then files+=(--from-file=page-token-key="$page_key"); fi
  "${host[@]}" -n "$HOST_NAMESPACE" create secret generic "$secret_name" "${files[@]}" \
    --dry-run=client -o yaml | "${host[@]}" apply --server-side --field-manager=management-engine-dev -f -
  echo "$identity datastore credentials expire at $expiration; renew with make dev-refresh DEV_NAME=$DEV_NAME."
)

refresh_api_credentials() (
  [[ "$DEV_PUBLIC_API" == 1 ]] || return 0
  local key="$dev_dir/api-page-token-key" candidate
  if [[ ! -f "$key" ]]; then
    candidate=$(mktemp "$dev_dir/api-page-token-key.XXXXXX")
    trap 'rm -f "$candidate"' EXIT
    openssl rand 32 >"$candidate"
    # Preserve the winner if concurrent refreshes initialize this environment.
    ln "$candidate" "$key" 2>/dev/null || [[ -f "$key" ]]
  fi
  refresh_identity "$(api_namespace)" public-api public-api-state-client /etc/public-api "$key"
)

refresh_credentials() {
  refresh_identity "$STATE_NAMESPACE" management-engine-controller-manager management-state-client /etc/management-api
  refresh_api_credentials
}

case "${1:-}" in
  check|tilt)
    [[ ! -f "$dev_dir/cwe/owned" ]] || fail "This environment belongs to the combined CWE stack; run make dev-cwe DEV_NAME=$DEV_NAME."
    check_tilt
    if [[ "$1" == tilt ]]; then
      connect
      start_tilt Tiltfile "${TILT_PORT:-10351}"
    fi
    ;;
  up)
    : "${KIND_NODE_IMAGE:?Run make dev-up to use the pinned node image}"
    existing=$(kind get clusters)
    # Check all existing clusters before creating or changing either cluster.
    for cluster in "${clusters[@]}"; do
      if grep -Fxq -- "$cluster" <<<"$existing"; then
        require_cluster "$cluster"
        actual=$(docker inspect --format '{{.Image}}' "$cluster-control-plane")
        expected=$(docker image inspect --format '{{.Id}}' "$KIND_NODE_IMAGE")
        [[ "$actual" == "$expected" ]] || fail "$cluster uses a different node image; run make dev-down DEV_NAME=$DEV_NAME before recreating it."
      fi
    done
    mkdir -p "$dev_dir"
    for cluster in "${clusters[@]}"; do
      if ! grep -Fxq -- "$cluster" <<<"$existing"; then
        config_args=()
        if [[ ${DEV_CWE:-} == 1 && "$cluster" == "$DEV_NAME-management" ]]; then
          config_args=(--config config/dev/cwe-kind.yaml)
        fi
        kind create cluster --name "$cluster" --image "$KIND_NODE_IMAGE" --kubeconfig "$KUBECONFIG" "${config_args[@]}" --wait 120s
      fi
    done
    connect
    make chart
    sync_state
    refresh_credentials
    echo "Ready: make dev DEV_NAME=$DEV_NAME"
    ;;
  sync) connect; sync_state ;;
  refresh) connect; refresh_credentials ;;
  api-credentials) connect; sync_api_state; refresh_api_credentials ;;
  api-test)
    [[ "$DEV_PUBLIC_API" == 1 ]] || fail "The public API must be enabled in Tilt."
    connect
    "${host[@]}" -n "$HOST_NAMESPACE" rollout status deployment/public-api --timeout=120s
    "${host[@]}" -n "$HOST_NAMESPACE" rollout status deployment/management-engine-controller-manager --timeout=120s
    manager_args=$("${host[@]}" -n "$HOST_NAMESPACE" get deployment management-engine-controller-manager \
      -o 'jsonpath={range .spec.template.spec.containers[?(@.name=="manager")].args[*]}{@}{"\n"}{end}')
    export PUBLIC_API_CONTROLLER_MODE=unconfigured
    if grep -Eq '^--cwe-endpoint(=.+)?$' <<<"$manager_args"; then export PUBLIC_API_CONTROLLER_MODE=cwe; fi
    export PUBLIC_API_SMOKE=1
    export PUBLIC_API_HTTP="http://127.0.0.1:${API_HTTP_LOCAL_PORT:-18082}"
    export PUBLIC_API_GRPC="127.0.0.1:${API_GRPC_LOCAL_PORT:-19090}"
    PUBLIC_API_NAMESPACE=$("${host[@]}" -n "$HOST_NAMESPACE" get configmap public-api-settings -o jsonpath='{.data.namespace}')
    export PUBLIC_API_NAMESPACE
    results="$dev_dir/public-api/results"
    mkdir -p "$results"
    "${host[@]}" -n "$HOST_NAMESPACE" get deployment public-api -o json >"$results/deployment.json"
    "${host[@]}" -n "$HOST_NAMESPACE" get deployment management-engine-controller-manager -o json >"$results/controller.json"
    go test -count=1 -timeout=3m -json -run '^TestDeployedPublicAPI$' ./test/public/tenant | tee "$results/smoke.json"
    ;;
  test)
    connect
    exec bash test/install.sh
    ;;
  down)
    existing=$(kind get clusters)
    for cluster in "${clusters[@]}"; do
      if grep -Fxq -- "$cluster" <<<"$existing"; then require_cluster "$cluster"; fi
    done
    for cluster in "${clusters[@]}"; do
      kind delete cluster --name "$cluster" --kubeconfig "$KUBECONFIG"
    done
    rm -f "$KUBECONFIG" "$dev_dir/api-page-token-key"
    # Integration runs retain diagnostics in this directory after teardown.
    rmdir "$dev_dir" 2>/dev/null || true
    ;;
  cwe-*)
    # shellcheck source=management-engine/hack/cwe.sh
    source hack/cwe.sh
    cwe_main "$1"
    ;;
  *) fail "Usage: $0 {check|up|sync|refresh|api-credentials|api-test|tilt|test|down|cwe-up|cwe-tilt|cwe-test|cwe-credentials|cwe-down}" ;;
esac
