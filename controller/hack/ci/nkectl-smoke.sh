#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Drive nkectl against the CWE a backend E2E lane already deployed. Source it
# from ci/backend-e2e.sh; do not run it.
#
# No lane runs any nkectl command but `version` and `bootstrap site`, so nothing
# checks the CLI and the generated client agree against a real server.
#
# Needs from the caller: log, runner_exec, RUNNER_NAME, NKECTL_BIN.

# The CWE service the suite deploys, and the ServiceAccount it mints its token
# from (controller/test/e2e/internal/authtoken).
NKECTL_SMOKE_CWE_NAMESPACE=cloud-workflow-engine
NKECTL_SMOKE_CWE_SERVICE=cloud-workflow-engine
NKECTL_SMOKE_CWE_PORT=8080
NKECTL_SMOKE_TOKEN_NAMESPACE=nke-clients
NKECTL_SMOKE_TOKEN_SERVICE_ACCOUNT=nke-client
NKECTL_SMOKE_AUTH_CONFIGMAP=nke-auth-config

nkectl_smoke_build() {
  log "compiling nkectl for linux/${1}"
  (cd ../cli && CGO_ENABLED=0 GOOS=linux GOARCH="${1}" go build -o "${NKECTL_BIN}" ./cmd/nkectl)
}

nkectl_smoke_stage() {
  docker cp "${NKECTL_BIN}" "${RUNNER_NAME}:/usr/local/bin/nkectl"
}

nkectl_exec() {
  local token="$1"
  shift
  docker exec \
    -e KUBECONFIG=/state/management.kubeconfig \
    -e "NKECTL_DEV_TOKEN=${token}" \
    "${RUNNER_NAME}" nkectl "$@"
}

# Port-forward rather than the apiserver service proxy: the proxy does not
# forward a bearer token to CWE, which is why wait_for_zero_tenant_clusters
# counts CAPI objects instead of calling the API.
nkectl_smoke() {
  local token out rc audience

  # CWE checks the token's audience, so an unscoped `kubectl create token` is
  # rejected with audience_mismatch. Read the expected value rather than
  # hardcoding it, the way test/e2e/internal/authtoken does.
  audience="${NKE_E2E_AUDIENCE:-}"
  if [[ -z "${audience}" ]]; then
    audience="$(runner_exec kubectl -n "${NKECTL_SMOKE_CWE_NAMESPACE}" get configmap \
      "${NKECTL_SMOKE_AUTH_CONFIGMAP}" -o "jsonpath={.data.auth\\.yaml}" 2>/dev/null |
      sed -n 's/^audiences:[^"]*"\([^"]*\)".*/\1/p' | head -1)"
  fi
  if [[ -z "${audience}" ]]; then
    log "nkectl smoke: no audience in ${NKECTL_SMOKE_CWE_NAMESPACE}/${NKECTL_SMOKE_AUTH_CONFIGMAP}; set NKE_E2E_AUDIENCE" >&2
    return 1
  fi

  if ! token="$(runner_exec kubectl create token "${NKECTL_SMOKE_TOKEN_SERVICE_ACCOUNT}" \
    -n "${NKECTL_SMOKE_TOKEN_NAMESPACE}" --audience "${audience}" --duration=1h 2>&1)"; then
    log "nkectl smoke: could not mint a token for ${NKECTL_SMOKE_TOKEN_NAMESPACE}/${NKECTL_SMOKE_TOKEN_SERVICE_ACCOUNT}: ${token}" >&2
    return 1
  fi

  docker exec -d -e KUBECONFIG=/state/management.kubeconfig "${RUNNER_NAME}" \
    kubectl -n "${NKECTL_SMOKE_CWE_NAMESPACE}" port-forward \
    "svc/${NKECTL_SMOKE_CWE_SERVICE}" "${NKECTL_SMOKE_CWE_PORT}:${NKECTL_SMOKE_CWE_PORT}"

  runner_exec nkectl config set-site --name e2e \
    --url "http://127.0.0.1:${NKECTL_SMOKE_CWE_PORT}" \
    --kubeconfig /state/management.kubeconfig >/dev/null

  # Doubles as the readiness wait for the forward.
  rc=1
  for _ in $(seq 1 15); do
    if out="$(nkectl_exec "${token}" get clusters 2>&1)"; then
      rc=0
      break
    fi
    sleep 2
  done
  if ((rc != 0)); then
    log "nkectl smoke: 'get clusters' never succeeded: ${out}" >&2
    return 1
  fi
  if ! grep -q 'KUBERNETES VERSION' <<<"${out}"; then
    log "nkectl smoke: 'get clusters' printed no table header: ${out}" >&2
    return 1
  fi

  # Control: without this, a CWE that stopped enforcing auth would leave the
  # check above green and prove nothing about the token.
  if [[ "${NKE_E2E_REQUIRE_AUTHN:-}" == "true" ]]; then
    if out="$(nkectl_exec "" get clusters 2>&1)"; then
      log "nkectl smoke: 'get clusters' succeeded with no token, so the run above proves nothing: ${out}" >&2
      return 1
    fi
  else
    log "nkectl smoke: SKIP the no-token control, NKE_E2E_REQUIRE_AUTHN is not true"
  fi

  if ! out="$(nkectl_exec "${token}" get clusters -o json 2>&1)"; then
    log "nkectl smoke: 'get clusters -o json' failed: ${out}" >&2
    return 1
  fi
  case "${out}" in
    '['*|'{'*|null*) ;;
    *) log "nkectl smoke: -o json did not emit JSON: ${out}" >&2; return 1 ;;
  esac

  log "PASS: nkectl reached CWE, authenticated, and listed clusters"
}
