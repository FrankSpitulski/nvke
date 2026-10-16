#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Prepare a machine for the local site, so the operator flow can be typed as
# it is on real hardware: nvkectl generate sitespec, nvkectl bootstrap site,
# deploy/check-site.sh, clusterctl init, deploy/nvke-deploy.sh install, the
# release resources, a tenant cluster. This script stands in for what a data
# center and a build pipeline hand an operator, and for the lab's simulator;
# it runs none of the operator's commands.
#
#   hack/local-site.sh setup    the host, a container node with its hosts.csv and
#                               SPIRE CA, the stack artifacts built from this
#                               checkout, the stack config and credentials, nico-sim,
#                               the files the flow needs; writes a workspace and its env.sh
#   hack/local-site.sh status   what is up
#   hack/local-site.sh down     delete tenant clusters, nico-sim, the node and the workspace
#
# `make local-site-setup`, `local-site-status` and `local-site-down` run
# these; deploy/nvke-deploy-local.sh is the install with the lab's steps. The workspace is NVKE_LOCAL_SITE_DIR (default
# ~/nvke-local-site): hosts.csv, spire-ca.crt and .key, stack-config.json,
# artifacts/stack-artifacts.json, nvke-api-clients.yaml with the lab's networks
# filled in, releases/, tenant.yaml, and env.sh, which exports the NVKECTL_*
# credentials, the CA files, CTR and the NVKE_* input paths for the flow and
# for deploy/nvke-deploy-local.sh. Rerunning setup skips what is
# there. docs/get-started/local-site.md is the page.
#
# Linux, as root, with nerdctl's containerd (cli/hack/bootstrap-e2e-host.sh
# install-runtime) for the node and Docker for nico-sim's tenant nodes. On
# macOS, inside the Colima machine `make -C controller host-up` creates.
set -Eeuo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKSPACE="${NVKE_LOCAL_SITE_DIR:-${HOME}/nvke-local-site}"
SITE="local-site"
NODE="nvkectl-e2e-node1"
NODE_NETWORK="${NVKECTL_E2E_NETWORK:-nvkectl-e2e-nodes}"
NODE_IP="${NVKECTL_E2E_INIT_NODE_IP:-172.26.0.10}"
TENANT_NETWORK="kind"
NICO_SIM_PORT=18080
NVKECTL="${NVKECTL:-${REPO}/cli/bin/nvkectl}"
KUBECONFIG_FILE="${WORKSPACE}/${SITE}.kubeconfig"
ARCH="$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"
EXAMPLES="${REPO}/deploy/examples/local-site"

log() { printf '[local-site] %s\n' "$*" >&2; }
die() { printf 'local-site: %s\n' "$*" >&2; exit 1; }
mk() { make -C "${REPO}" --no-print-directory "$@"; }

usage() {
  sed -n '4,/^set -Eeuo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//' >&2
  exit 2
}

# ---- the host -----------------------------------------------------------------

check_host() {
  case "$(uname -s)" in
    Linux) ;;
    Darwin) die "run this inside the Colima machine: make -C controller host-up, then colima ssh and sudo -i (docs/get-started/local-site.md)" ;;
    *) die "unsupported OS $(uname -s)" ;;
  esac
  [[ "$(id -u)" == 0 ]] || die "run as root: the node containers need iptables rules and a bridge address (sudo -i)"
  [[ -c /dev/vtpmx ]] || die "/dev/vtpmx is missing; load tpm_vtpm_proxy (make -C controller host-up)"
  local missing=()
  for tool in nerdctl kind kubectl helm clusterctl oras curl jq python3 go gcc git make docker openssl iptables; do
    command -v "${tool}" >/dev/null 2>&1 || missing+=("${tool}")
  done
  [[ ${#missing[@]} -eq 0 ]] || die "not on PATH: ${missing[*]}"
  if [[ -z "${CONTAINERD_ADDRESS:-}" && -S /run/containerd-nerdctl/containerd.sock ]]; then
    export CONTAINERD_ADDRESS=/run/containerd-nerdctl/containerd.sock
    export BUILDKIT_HOST="${BUILDKIT_HOST:-unix:///run/buildkit-nerdctl/buildkitd.sock}"
    export CONTAINERD_NAMESPACE="${CONTAINERD_NAMESPACE:-default}"
  fi
  nerdctl info >/dev/null 2>&1 || die "nerdctl cannot reach its containerd; run cli/hack/bootstrap-e2e-host.sh install-runtime and export CONTAINERD_ADDRESS"
  docker info >/dev/null 2>&1 || die "docker cannot reach its daemon; nico-sim runs the tenant nodes on it"
  [[ -n "${GITHUB_TOKEN:-}" ]] || log "warning: GITHUB_TOKEN is not set; clusterctl init reads the GitHub API, which allows anonymous callers 60 requests an hour"
  mkdir -p "${WORKSPACE}/releases"
  chmod 700 "${WORKSPACE}"
}

# ---- the images and the binary ------------------------------------------------

build_images() {
  [[ -x "${NVKECTL}" ]] || { log "building nvkectl"; mk -C cli build >/dev/null; }
  log "building the bootstrap images (make -C cli images)"
  mk -C cli images >/dev/null
}

# ---- the node: what the data center hands an operator --------------------------

node_up() {
  if nerdctl inspect "${NODE}" --format '{{.State.Running}}' 2>/dev/null | grep -qx true; then
    log "node ${NODE} is running"
  else
    log "starting the container node (make -C cli nodes-up; the first run builds the node image)"
    NVKECTL_E2E_NODES=1 NVKECTL_E2E_REUSE=1 mk -C cli nodes-up
  fi
  # nodes-hosts records a SPIRE journal cursor per node under the attestation
  # baseline; right after nodes-up the agent may not have logged yet, so the
  # first reads can find no entries.
  export NVKECTL_E2E_ATTESTATION_BASELINE="${WORKSPACE}/.attestation-baseline.json"
  local attempt hosts err="${WORKSPACE}/.nodes-hosts.err"
  for attempt in 1 2 3 4 5 6; do
    if hosts="$(NVKECTL_E2E_NODES=1 NVKECTL_E2E_HOSTS_CSV="${WORKSPACE}/hosts.csv" mk -C cli nodes-hosts 2>"${err}")"; then break; fi
    [[ "${attempt}" -lt 6 ]] || die "nodes-hosts failed six times: $(tail -n 3 "${err}" | tr '\n' ' ')"
    sleep 5
  done
  rm -f "${err}"
  eval "${hosts}"
  install -m 0644 "${NVKECTL_E2E_SPIRE_CA_CERT}" "${WORKSPACE}/spire-ca.crt"
  install -m 0600 "${NVKECTL_E2E_SPIRE_CA_KEY}" "${WORKSPACE}/spire-ca.key"
}

# ---- the artifacts: what a build hands an operator -----------------------------

build_artifacts() {
  if [[ -f "${WORKSPACE}/artifacts/stack-artifacts.json" ]]; then
    log "artifacts present under ${WORKSPACE}/artifacts"
    return
  fi
  log "building the stack artifacts from the checkout (make local-stack-artifacts)"
  mk local-stack-artifacts OUT="${WORKSPACE}/artifacts" CONTAINER_CLI=nerdctl ARCH="${ARCH}" >/dev/null
}

# ---- the files the flow reads -------------------------------------------------------

network_cidr() {
  # network_cidr CLI NETWORK prints the IPv4 subnet of a container network.
  "$1" network inspect "$2" --format '{{range .IPAM.Config}}{{.Subnet}} {{end}}' 2>/dev/null |
    awk '{for (i=1;i<=NF;i++) if ($i ~ /^[0-9]+(\.[0-9]+){3}\/[0-9]+$/) {print $i; exit}}'
}

write_inputs() {
  cp "${EXAMPLES}/stack-config.json" "${WORKSPACE}/stack-config.json"
  local kind
  for kind in machineimage clusterrelease nodepoolrelease; do
    cp "${EXAMPLES}/releases/${kind}-${ARCH}.yaml" "${WORKSPACE}/releases/${kind}.yaml"
  done
  cp "${EXAMPLES}/tenant-${ARCH}.yaml" "${WORKSPACE}/tenant.yaml"
  docker network inspect "${TENANT_NETWORK}" >/dev/null 2>&1 || docker network create "${TENANT_NETWORK}" >/dev/null
  local host_cidr tenant_cidr
  host_cidr="$(network_cidr nerdctl "${NODE_NETWORK}")"; tenant_cidr="$(network_cidr docker "${TENANT_NETWORK}")"
  [[ -n "${host_cidr}" && -n "${tenant_cidr}" ]] || die "cannot read the ${NODE_NETWORK} or ${TENANT_NETWORK} subnet"
  sed -e "s|REPLACE_HOST_CIDR|${host_cidr}|" -e "s|REPLACE_TENANT_CIDR|${tenant_cidr}|" "${EXAMPLES}/nvke-api-clients.yaml" >"${WORKSPACE}/nvke-api-clients.yaml"
  if [[ ! -f "${WORKSPACE}/env.sh" ]]; then
    (umask 077 && cat >"${WORKSPACE}/env.sh" <<ENV
# shellcheck shell=bash
# The local site's environment: source it in the shell you type the flow in.
# Written by hack/local-site.sh setup; the credentials are the lab's.
export PATH="${REPO}/cli/bin:\${PATH}"
export CONTAINERD_ADDRESS="${CONTAINERD_ADDRESS:-/run/containerd-nerdctl/containerd.sock}"
export BUILDKIT_HOST="${BUILDKIT_HOST:-unix:///run/buildkit-nerdctl/buildkitd.sock}"
export CONTAINERD_NAMESPACE="${CONTAINERD_NAMESPACE:-default}"
export NVKECTL_E2E_NODES=1 NVKECTL_E2E_ATTESTATION_BASELINE="${WORKSPACE}/.attestation-baseline.json"
# The SPIRE CA the node's agent trusts: the bootstrap and the install read it.
export NVKECTL_SPIRE_CA_CERT_FILE="${WORKSPACE}/spire-ca.crt" NVKECTL_SPIRE_CA_KEY_FILE="${WORKSPACE}/spire-ca.key"
# The install's credentials. nico-sim accepts its seeded token; the node login
# and the database password are generated for this site.
export NVKECTL_NICO_TOKEN=nico-simctl-token NVKECTL_NODE_USER=nvke
export NVKECTL_NODE_PASSWORD=$(openssl rand -hex 12) NVKECTL_DATABASE_PASSWORD=$(openssl rand -hex 16)
# The install imports the artifacts' layouts into the node's containerd; the
# node is a container, so the import goes through nerdctl exec.
export CTR="nerdctl exec -i ${NODE} ctr"
# The inputs deploy/nvke-deploy-local.sh install reads.
export NVKE_SITESPEC="${WORKSPACE}/sitespec.yaml" NVKE_STACK_CONFIG="${WORKSPACE}/stack-config.json"
export NVKE_STACK_ARTIFACTS="${WORKSPACE}/artifacts/stack-artifacts.json" NVKE_KUBECONFIG="${KUBECONFIG_FILE}"
cd "${WORKSPACE}"
ENV
    )
  fi
}

# ---- nico-sim: the lab's NICo ----------------------------------------------------

bridge_networks() {
  # The tenant nodes (Docker's network) and the site node (nerdctl's) must
  # reach each other: the node agent, the SPIRE server and the NVKE API on
  # the site, the agent and the API server on the tenant nodes.
  local node_br tenant_br pair
  node_br="br-$(nerdctl network inspect --format '{{.Id}}' "${NODE_NETWORK}" | cut -c1-12)"
  tenant_br="br-$(docker network inspect --format '{{.Id}}' "${TENANT_NETWORK}" | cut -c1-12)"
  for pair in "${node_br} ${tenant_br}" "${tenant_br} ${node_br}"; do
    # shellcheck disable=SC2086 # two words on purpose
    set -- ${pair}
    iptables -C FORWARD -i "$1" -o "$2" -j ACCEPT 2>/dev/null || iptables -I FORWARD 1 -i "$1" -o "$2" -j ACCEPT
  done
}

nico_sim_up() {
  local sim="${REPO}/controller/nico-sim" pid_file="${WORKSPACE}/.nico-sim.pid" gateway
  gateway="$(nerdctl network inspect "${NODE_NETWORK}" --format '{{range .IPAM.Config}}{{.Gateway}} {{end}}' | awk '{print $1}')"
  if [[ -f "${pid_file}" ]] && kill -0 "$(cat "${pid_file}")" 2>/dev/null; then
    log "nico-sim is running"
  else
    [[ -x "${sim}/bin/nico-sim" ]] || { log "building nico-sim"; CGO_ENABLED=1 mk -C controller/nico-sim build >/dev/null; }
    if ! docker image inspect cwe-capd-kindest-node:local >/dev/null 2>&1; then
      log "copying the node image into Docker for the tenant nodes"
      nerdctl save cwe-capd-kindest-node:local | docker load >/dev/null
    fi
    log "starting nico-sim on ${gateway}:${NICO_SIM_PORT}"
    (
      cd "${sim}"
      export NICO_SIM_LISTEN_ADDR="0.0.0.0:${NICO_SIM_PORT}" NICO_SIM_DB_PATH="${WORKSPACE}/.nico-sim.db" NICO_SIM_SEED_PATH="${sim}/internal/seed/default-seed.yaml"
      export NICO_SIM_LOG_FORMAT=text NICO_VMS_VM_BACKEND=mock NICO_VMS_BGP_ENABLED=false
      export NICO_CONTAINERS_ENABLED=true NICO_CONTAINERS_REQUIRE_HEALTHY=true NICO_CONTAINERS_NODE_NETWORK="${TENANT_NETWORK}" NICO_CONTAINERS_NODE_IMAGE=cwe-capd-kindest-node:local
      nohup ./bin/nico-sim >"${WORKSPACE}/.nico-sim.log" 2>&1 &
      echo $! >"${pid_file}"
    )
  fi
  local _
  for _ in $(seq 1 30); do curl -sS -m 1 "http://${gateway}:${NICO_SIM_PORT}/healthz" >/dev/null 2>&1 && break; sleep 1; done
  curl -sS -m 2 "http://${gateway}:${NICO_SIM_PORT}/healthz" >/dev/null || die "nico-sim did not answer; the log is ${WORKSPACE}/.nico-sim.log"
  bridge_networks
}

setup() {
  check_host
  build_images
  node_up
  build_artifacts
  write_inputs
  nico_sim_up
  cat <<DONE

The local site is ready for the operator flow. In a root shell:

  source ${WORKSPACE}/env.sh
  nvkectl generate sitespec ${SITE} --nodes hosts.csv --set cluster.podCidr=10.244.0.0/16 --set cluster.kubeletConfig.systemReservedMemory=512Mi --set spire.upstreamCA=true > sitespec.yaml
  nvkectl bootstrap site -f sitespec.yaml --timeout 25m

Then, as docs/get-started/local-site.md lists them: clusterctl init,
deploy/nvke-deploy-local.sh install, the NetworkPolicy, nvkectl config set-site
--url http://${NODE_IP}:30080, the releases, a tenant cluster.
The workspace holds:
  hosts.csv, spire-ca.crt, spire-ca.key   the node, as a data center hands it over
  artifacts/stack-artifacts.json          the management plane's images and charts, built from this checkout
  stack-config.json                       nico-sim's NICo API
  nvke-api-clients.yaml                   the networks admitted to the NVKE API
  releases/, tenant.yaml                  the release set and a tenant request for ${ARCH}
  env.sh                                  the credentials, CTR and the input paths for the flow
DONE
}

status() {
  local running=false
  nerdctl inspect "${NODE}" --format '{{.State.Running}}' 2>/dev/null | grep -qx true && running=true
  printf 'workspace:   %s\n' "${WORKSPACE}"
  printf 'node:        %s\n' "$([[ "${running}" == true ]] && echo "${NODE} running" || echo "down")"
  printf 'artifacts:   %s\n' "$([[ -f "${WORKSPACE}/artifacts/stack-artifacts.json" ]] && echo built || echo "not built")"
  if [[ -f "${KUBECONFIG_FILE}" ]] && kubectl --kubeconfig "${KUBECONFIG_FILE}" get nodes >/dev/null 2>&1; then
    printf 'management:  up (%s)\n' "${KUBECONFIG_FILE}"
    printf 'stack:       %s\n' "$(kubectl --kubeconfig "${KUBECONFIG_FILE}" -n nke-controller get deployment nke-controller -o jsonpath='{.status.readyReplicas}' 2>/dev/null | grep -q '^[1-9]' && echo up || echo "not installed")"
  else
    printf 'management:  down\n'
  fi
  if [[ -f "${WORKSPACE}/.nico-sim.pid" ]] && kill -0 "$(cat "${WORKSPACE}/.nico-sim.pid")" 2>/dev/null; then
    printf 'nico-sim:    running (pid %s)\n' "$(cat "${WORKSPACE}/.nico-sim.pid")"
  else
    printf 'nico-sim:    down\n'
  fi
}

down() {
  if [[ -f "${KUBECONFIG_FILE}" ]] && command -v "${NVKECTL}" >/dev/null 2>&1; then
    local name
    for name in $("${NVKECTL}" get clusters 2>/dev/null | awk 'NR > 1 { print $1 }'); do
      log "deleting tenant cluster ${name}"
      "${NVKECTL}" delete cluster "${name}" >/dev/null 2>&1 || true
    done
    local _
    for _ in $(seq 1 90); do
      [[ -z "$("${NVKECTL}" get clusters 2>/dev/null | awk 'NR > 1')" ]] && break
      sleep 10
    done
  fi
  if [[ -f "${WORKSPACE}/.nico-sim.pid" ]]; then
    kill "$(cat "${WORKSPACE}/.nico-sim.pid")" 2>/dev/null || true
  fi
  if command -v docker >/dev/null 2>&1; then
    mapfile -t ids < <(docker ps -aq --filter label=nvidia.com/nico-containers.managed=true 2>/dev/null || true)
    [[ ${#ids[@]} -eq 0 ]] || docker rm -f "${ids[@]}" >/dev/null
  fi
  if command -v nerdctl >/dev/null 2>&1 && nerdctl inspect "${NODE}" >/dev/null 2>&1; then
    log "removing the node (make -C cli nodes-down)"
    NVKECTL_E2E_NODES=1 mk -C cli nodes-down >/dev/null || true
  fi
  rm -rf "${WORKSPACE}"
  log "down"
}

case "${1:-}" in
  setup) setup ;;
  status) status ;;
  down) down ;;
  *) usage ;;
esac
