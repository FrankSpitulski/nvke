# nvke-nico-stack

NVKE, SPIRE and NICo in one release — the three charts beside this one,
installed together.

```bash
helm dependency build deploy/charts/nvke-nico-stack
helm upgrade --install nvke-nico-stack deploy/charts/nvke-nico-stack \
  --namespace nvke-system --create-namespace \
  --values deploy/charts/nvke-nico-stack/values-kind.yaml \
  --set nvke.database.password=<pick one> \
  --wait --timeout 12m
```

cert-manager must already be in the cluster: this release creates `Certificate`
and `ClusterIssuer` objects, so their CRDs have to exist before it is applied.
`clusterctl init` installs cert-manager, so a seed that has run it is ready.

`--wait` is not proof. On helm 3.19 this returns `STATUS: deployed` in seconds
with the NVKE controller still in `ImagePullBackOff`. Check the rollout
yourself, as `NOTES.txt` says:

```bash
kubectl -n nke-controller rollout status deployment/nke-controller --timeout=10m
```

`--namespace nvke-system` is not arbitrary: Temporal's chart has no
`namespaceOverride`, so it lands in the release namespace, and
`nvke.nvke-controller.temporal.host` names `nvke-system`. `NOTES.txt` reports
the mismatch if the release goes elsewhere.

## Values

This chart sets only what belongs to the combination. Everything else stays in
the three charts' own `values.yaml`, which this chart does not restate — a copy
here would be a second place to change and a second place to get wrong.

Configure each through its own key:

```bash
--set nvke.database.password=...
--set nvke-spire.global.spire.trustDomain=...
--set nvke-nico.nico.global.image.tag=...
```

`values-kind.yaml` is each chart's own `values-kind.yaml` re-keyed, and nothing
more.

## Storage is off by default

`nvke-storage.enabled` ships false. kind already has a default StorageClass and
a real site brings the CSI driver for its hardware. Turn it on for a bare
cluster that has neither, or every PostgreSQL PVC sits `Pending`.

`NOTES.txt` warns when the cluster has no default StorageClass and the chart is
off; the chart itself refuses to install beside an existing default. See
[`../storage/README.md`](../storage/README.md).

## NICo needs two things outside this release

`nvke-nico.enabled=true` is not enough for a single apply. NICo's chart uses
`pre-install,pre-upgrade` hooks, and helm applies hooks before every manifest
in the release, so both of these must already exist:

```bash
kubectl create namespace forge-system        # the hooks run before a Namespace in this release
--set nvke-nico.postgresql.enabled=false     # and before a StatefulSet in it
--set nvke-nico.database.host=<existing-postgres-host>
```

With `nvke-nico.postgresql.enabled=true`, install [`../nico`](../nico/) as its
own release instead. See [`../nico/README.md`](../nico/README.md#nico-cannot-share-a-release-with-its-namespace-or-its-database).

## NICo is off by default

`nvke-nico.enabled` ships false. Turn it on once the `nico` repository alias is
registered and `nvke-nico.nico.global.image.repository` and `.tag` name images
the cluster can pull. See [`../nico/README.md`](../nico/README.md).

`helm dependency build` does **not** recurse: building this chart packages
[`../nico`](../nico/) exactly as it finds it on disk. Build that chart's own
dependencies first, or the NICo umbrella is absent from the archive and only
its prerequisites install. `nico`'s `validate.yaml` refuses that render rather
than letting it pass.

```bash
helm repo add nico <your NICo chart repository>
helm dependency build deploy/charts/nico        # first
helm dependency build deploy/charts/nvke-nico-stack  # then this
```

Without NICo, leave `nvke-nico.enabled` false. This chart still builds and
installs: the dependency is fetched, but nothing of NICo's is.

## One Temporal, not two

NICo's Temporal is refused here. Temporal's chart has no `namespaceOverride`,
so two of them in one release land in one namespace and take each other's
Service names. `nvke-nico` fails the render with that sentence rather than
letting it happen. To give NICo a Temporal of its own, release
[`../nico`](../nico/) separately, into its own namespace.

## Globals are shared

Helm merges `global:` into every subchart, which is how
`global.spire.*` reaches all four SPIRE subcharts and `global.image.*` reaches
all of NICo's. Two keys are read by more than one chart:

| Key | Read by |
|---|---|
| `global.imagePullSecrets` | NICo's subcharts and Bitnami's PostgreSQL |
| `global.nvkeNicoStack` | `nvke-nico`, to refuse a second Temporal |

Everything else is namespaced by owner and does not collide.
