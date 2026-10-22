# Helm charts

Five charts that install NVKE, SPIRE, CAPNICo and NICo with `helm` alone.

| Chart | Installs | In the umbrella |
|---|---|---|
| [`nvke/`](nvke/) | The NVKE controller, with the PostgreSQL and Temporal it runs on | yes |
| [`spire/`](spire/) | SPIRE, the OIDC upstream CA chain and the two NodePorts | yes |
| [`nico/`](nico/) | NICo, with the prerequisites its own chart expects to find already in place | yes, off by default |
| [`capnico/`](capnico/) | CAPNICo, the Cluster API provider **for** NICo, with the credentials Secret it reads | **no** — see below |
| [`nvke-nico-stack/`](nvke-nico-stack/) | The first three, in one release | — |

Each chart stands on its own. `nvke-nico-stack` is for the common case — one
release, one namespace, one command.

**CAPNICo is a separate release, and it needs helm 4.** Its published chart
takes every object's namespace from `{{ .Release.Namespace }}` and declares
that namespace itself, so it can only be released into the namespace it lives
in — which rules out being a subchart of an umbrella released elsewhere. See
[`capnico/README.md`](capnico/README.md). The other four work on helm 3.

These charts are an alternative to the [deploy path](../nico/), not a
replacement for it. The deploy path renders per-cluster values and credentials
from a site spec, pins every artifact to a release digest, and imports images
into the node's containerd before it applies anything. A Helm chart does none
of that. Use the deploy path to install a real cluster from a release; use
these charts for a development cluster, or where a GitOps tool already drives
Helm.

## Before you start

| Prerequisite | Why |
|---|---|
| cert-manager | Every chart here creates `Certificate` and `Issuer` objects, so their CRDs have to exist first. `clusterctl init` installs cert-manager, so a seed that has run it is ready |
| A default StorageClass | PostgreSQL and SPIRE both take a PVC. Without a default they sit `Pending` forever. kind ships the rancher local-path provisioner |
| Cluster API, through `clusterctl init` | For driving real clusters, and for CAPNICo to reconcile anything. The NVKE controller starts and answers without it, and logs that `cluster.x-k8s.io` kinds are missing |
| helm 4 | Only for the CAPNICo chart. NVKE pins `v4.2.4` in `make/tool-versions.mk`. The other four charts work on helm 3 |
| A `nico` chart repository | Only for the NICo chart. NICo is published separately from NVKE, so neither `nico/` nor `nvke-nico-stack/` names a registry: `helm repo add nico <your NICo chart repository>` |

```bash
helm repo add jetstack https://charts.jetstack.io
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace --set crds.enabled=true
```

## On a kind cluster

```bash
kind create cluster --name nvke --config - <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: 30080   # the NVKE API
        hostPort: 30080
      - containerPort: 30440   # the SPIRE server
        hostPort: 30440
EOF

helm dependency build deploy/charts/spire
helm dependency build deploy/charts/nvke
helm dependency build deploy/charts/nvke-nico-stack

helm upgrade --install nvke-nico-stack deploy/charts/nvke-nico-stack \
  --namespace nvke-system --create-namespace \
  --values deploy/charts/nvke-nico-stack/values-kind.yaml \
  --set nvke.database.password=<pick one> \
  --set nvke.nvke-controller.profiles.image.repository=<repo> \
  --set nvke.nvke-controller.profiles.image.tag=<tag> \
  --wait --timeout 12m
```

Then:

```bash
kubectl -n nke-controller exec deploy/nke-controller -- wget -qO- http://localhost:8080/readyz
# {"status":"ok","checks":{"database":"ok","temporal":"ok"}}

curl http://127.0.0.1:30080/api/version
```

`/readyz` reports PostgreSQL and Temporal, so an `ok` there means both are
actually reachable — `/api/version` proves only that HTTP is serving.

**Do not read `--wait` as proof.** On helm 3.19 the install above returns
`STATUS: deployed` in seconds with the controller still in
`ImagePullBackOff`. Check the rollout yourself:

```bash
kubectl -n nke-controller rollout status deployment/nke-controller --timeout=10m
```

The controller image is not public. Build it from this checkout and load it
into the node:

```bash
make -C controller docker-build-local
kind load docker-image nvcr.io/j7sbcjl3qgta/nvke-controller:latest --name nvke
helm upgrade ... \
  --set nvke.nvke-controller.image.repository=nvcr.io/j7sbcjl3qgta/nvke-controller \
  --set nvke.nvke-controller.image.tag=latest
```

The profiles image is maintained outside this repository and the chart refuses
to render without one. `--set nvke.nvke-controller.profiles.image.enabled=false`
leaves it out, which is enough to bring the controller up; `/api/releases` then
returns 500 and nothing can be provisioned.

## Adding CAPNICo

CAPNICo reconciles `NicoCluster` and `NicoMachine` objects against a NICo API
that already exists. It is not NICo. Its chart and image are both public, so
it needs no repository alias — but it does need helm 4 and a release of its
own:

```bash
helm dependency build deploy/charts/capnico
helm upgrade --install capnico deploy/charts/capnico \
  --namespace capnico-system --create-namespace \
  --set credentials.create=true \
  --set credentials.endpoint=<nico-api-base-url> \
  --set credentials.orgID=<org> \
  --set credentials.apiName=<api> \
  --set credentials.token=<token>
```

Pick **one** way to install CAPNICo and stay with it: this chart, its published
chart directly, or `clusterctl init --infrastructure nico`. All three create
the same `capnico-system/controller-manager` Deployment under the same names,
and two of them do not give two installs — they give two things claiming the
same objects.

CAPNICo also needs Cluster API, which `clusterctl init` brings. Without it the
manager runs and reconciles nothing.

## Adding NICo

NICo needs its chart repository and images, and both are published separately
from NVKE:

```bash
helm repo add nico <your NICo chart repository>

# In this order: building nvke-nico-stack packages the nico chart as it finds it on
# disk, so that chart's own dependencies have to be in place first.
helm dependency build deploy/charts/nico
helm dependency build deploy/charts/nvke-nico-stack

ssh-keygen -t ed25519 -f ./ssh_host_ed25519_key -N ""

helm upgrade --install nvke-nico-stack deploy/charts/nvke-nico-stack \
  --namespace nvke-system \
  --values deploy/charts/nvke-nico-stack/values-kind.yaml \
  --set nvke.database.password=<pick one> \
  --set nvke-nico.enabled=true \
  --set nvke-nico.database.password=<pick one> \
  --set-file nvke-nico.sshHostKey.privateKey=./ssh_host_ed25519_key \
  --set nvke-nico.nico.global.image.repository=<repo> \
  --set nvke-nico.nico.global.image.tag=<tag>
```

NICo normally signs its service certificates through a cert-manager
`ClusterIssuer` backed by a Vault PKI engine. `values-kind.yaml` replaces that
with a self-signed CA under the same name, so a cluster with no Vault can still
issue them. NICo cannot tell the difference: it only ever names the issuer.
**A self-signed CA is not a substitute for Vault PKI on a real site** — no
revocation, no audit trail, no rotation story, and the CA private key is a
Secret in the cluster.

## What Helm does and does not sequence

One Helm release is one apply. Helm sorts by kind and waits for workloads, and
that is all the ordering there is. Four places needed work:

**CRDs and the objects that use them.** Helm validates every manifest in a
release against the API server *before* it applies any of them, so a
`ClusterSPIFFEID` and the CRD that defines it cannot be in one release. The
SPIRE CRDs are therefore vendored into [`spire/crds/`](spire/crds/) rather than
taken as a chart dependency — a `crds/` directory is applied first, with a
discovery refresh after it. Helm never *upgrades* a `crds/` directory, so
moving to a newer SPIRE chart means refreshing them with
[`spire/hack/sync-crds.sh`](spire/hack/sync-crds.sh) and applying the result by
hand.

**The SPIRE controller manager's webhook.** The chart applies its
`ClusterSPIFFEID` objects in the same release that creates the controller
manager serving the webhook that validates them, so on a fresh install the API
server calls a webhook that is not listening yet. `failurePolicy` is set to
`Ignore` outright. The webhook still validates once it is up; it no longer
blocks admission while it is starting.

**Temporal's namespace.** Temporal's chart has no `namespaceOverride` — its
templates follow `.Release.Namespace` — so it lands wherever its chart is
released. That is why the commands above use `--namespace nvke-system`, and why
`nvke-nico-stack` refuses to install a second Temporal for NICo: two of them in one
release would take each other's Service names. To give NICo a Temporal of its
own, release `nico/` on its own, into its own namespace, with
`temporal.enabled=true`.

**One password, three readers.** PostgreSQL's admin user, both of Temporal's
datastores and the controller's connection string must agree, or the Temporal
schema job crash-loops with `password authentication failed`. The `nvke` chart
owns that Secret and writes it into each of the three namespaces that read it.

Re-running any of these charts is safe: `helm upgrade --install` is idempotent,
and nothing here generates a credential or a key that would change on a second
run.

## Known limits

- **Namespaces are values, not derived.** Several hostnames are written out in
  full in more than one place, because a subchart cannot read a sibling's
  values. Each one carries a `CHANGE WITH …` comment naming the others.
- **NICo's subcharts each carry their own `namespaceOverride`.** There is no
  global that sets them all. `nico/templates/validate.yaml` refuses an install
  where one of them disagrees with `namespaces.nico`, so it cannot drift
  silently.
- **CAPNICo cannot join the umbrella without an upstream change.** Its chart
  would need a `namespaceOverride` threaded through its 29 direct
  `{{ .Release.Namespace }}` references, and its `Namespace` object made
  conditional. Both changes belong to `cluster-api-provider-nico`, not here.
- **`helm dependency build` does not recurse.** Building `nvke-nico-stack` packages
  `nico/` exactly as it finds it on disk, so `nico/`'s own dependencies have to
  be built FIRST — otherwise the NICo umbrella is absent from the archive and
  only its prerequisites install. `nico/templates/validate.yaml` refuses that
  render rather than letting it pass. Build in this order:
  `nico/`, then `nvke-nico-stack/`.
