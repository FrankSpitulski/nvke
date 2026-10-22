# Helm charts

Five charts that install NVKE, SPIRE and NICo with `helm` alone.

| Chart | Installs | In the umbrella |
|---|---|---|
| [`storage/`](storage/) | A default StorageClass — Rancher's local-path-provisioner, vendored | yes, off by default |
| [`nvke/`](nvke/) | The NVKE controller, with the PostgreSQL and Temporal it runs on | yes |
| [`spire/`](spire/) | SPIRE, the OIDC upstream CA chain and the two NodePorts | yes |
| [`nico/`](nico/) | NICo, with the prerequisites its own chart expects to find already in place | yes, off by default |
| [`nvke-nico-stack/`](nvke-nico-stack/) | The four above, in one release | — |

Each chart stands on its own. `nvke-nico-stack` is for the common case — one
release, one namespace, one command.

**CAPNICo is not here.** It is a Cluster API infrastructure provider, so
`clusterctl init --infrastructure nico` installs it — the same command that
brings Cluster API core and cert-manager. See [below](#cluster-api-cert-manager-and-capnico).

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
| cert-manager | Every chart here creates `Certificate` and `Issuer` objects, so their CRDs have to exist first. **`clusterctl init` installs it** — see below |
| CAPNICo | Only for driving NICo hardware. **`clusterctl init --infrastructure nico` installs it** — see below |
| A default StorageClass | PostgreSQL and SPIRE both take a PVC. Without a default they sit `Pending` forever. kind ships the rancher local-path provisioner, and a cluster with real storage brings its own CSI driver. For a bare cluster that has neither, [`storage/`](storage/) supplies one — set `nvke-storage.enabled=true` |
| Cluster API, through `clusterctl init` | For driving real clusters. The NVKE controller starts and answers without it, and logs that `cluster.x-k8s.io` kinds are missing |
| A `nico` chart repository | Only for the NICo chart. NICo is published separately from NVKE, so neither `nico/` nor `nvke-nico-stack/` names a registry: `helm repo add nico <your NICo chart repository>` |

### Cluster API, cert-manager and CAPNICo

One command brings all three. Do not install any of them separately: a second
installation does not give you two cert-managers or two CAPNICos — it gives
you two releases claiming the same cluster-scoped objects.

```bash
# v1.13.6 is CAPI_VERSION in make/tool-versions.mk. Read it from there rather
# than from this line, which will go stale.
clusterctl init \
  --core cluster-api:v1.13.6 \
  --bootstrap kubeadm:v1.13.6 \
  --control-plane kubeadm:v1.13.6 \
  --infrastructure nico:<version> \
  --config ./clusterctl.yaml \
  --wait-providers
```

`--infrastructure nico` needs the provider in your `clusterctl` configuration,
because it is not one of the providers `clusterctl` knows by default. Every
CAPNICo release publishes the `infrastructure-components.yaml` and
`metadata.yaml` it points at:

```yaml
# clusterctl.yaml
providers:
  - name: nico
    url: https://github.com/dsx-ai-factory/cluster-api-provider-nico/releases/download/<version>/infrastructure-components.yaml
    type: InfrastructureProvider
```

**Pass `--config` explicitly.** `clusterctl` resolves its default
configuration path through the OS convention, so
`~/.config/cluster-api/clusterctl.yaml` is not read on macOS. The failure is
unhelpful: `failed to get configuration for the InfrastructureProvider with
name nico`.

Then create the credentials Secret CAPNICo reads. CAPNICo owns it, and no
chart here writes it:

```bash
kubectl create secret generic nico-credentials -n capnico-system \
  --from-literal=endpoint=<nico-api-base-url> \
  --from-literal=orgID=<org> \
  --from-literal=apiName=<api> \
  --from-literal=token=<token>
```

Leave `--infrastructure nico` off on a cluster that drives no NICo hardware.
Everything else here still installs; nothing reconciles `NicoCluster` or
`NicoMachine`.

See [infrastructure-providers.md](../../docs/site-bootstrap/infrastructure-providers.md)
for the full provider setup, including why `nvkectl get providers` reporting
`CONFIGURED` is a narrower claim than it looks.

`make/tool-versions.mk` also pins `CERT_MANAGER_VERSION`, which records the
cert-manager version `clusterctl init` brings at that CAPI version. It moves
with `CAPI_VERSION`.

Running `clusterctl init` again later is supported and skips what is already
present, so a cluster that has it needs nothing here.

On a cluster that will never run Cluster API, follow
[cert-manager's own installation guide](https://cert-manager.io/docs/installation/helm/)
rather than a command copied from here, so you get the version and flags that
the project supports at the time you read it.

cert-manager is a prerequisite rather than a chart here, and cannot be made
one. It renders its six CRDs as ordinary templates, not in a `crds/`
directory — 12,749 lines of them. Helm validates every manifest in a release
against the API server before applying any of them, so a `Certificate` and the
CRD defining it cannot be in the same release. Vendoring those CRDs into a
`crds/` directory, as [`spire/`](spire/) does for SPIRE's three, would make
this repository the owner of cert-manager's CRD lifecycle. That is not a trade
worth making for a prerequisite `clusterctl init` already installs.

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

## Adding a StorageClass

A bare cluster ships no StorageClass, so every PostgreSQL PVC sits `Pending`.
kind already has one and a real site brings the CSI driver for its hardware, so
the chart is off by default:

```bash
helm upgrade --install nvke-nico-stack deploy/charts/nvke-nico-stack \
  --namespace nvke-system --create-namespace \
  --set nvke-storage.enabled=true \
  --set nvke.database.password=<pick one>
```

It refuses to install beside an existing default StorageClass, because
Kubernetes has no rule for choosing between two. The umbrella's `NOTES.txt`
warns the other way round too — when the cluster has no default and the chart
is off.

**Local storage is a lab default.** A volume is a directory on one node: it
does not move, does not replicate, and does not survive the node. See
[`storage/README.md`](storage/README.md).

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
- **`helm dependency build` does not recurse.** Building `nvke-nico-stack` packages
  `nico/` exactly as it finds it on disk, so `nico/`'s own dependencies have to
  be built FIRST — otherwise the NICo umbrella is absent from the archive and
  only its prerequisites install. `nico/templates/validate.yaml` refuses that
  render rather than letting it pass. Build in this order:
  `nico/`, then `nvke-nico-stack/`.
