# nvke-stack

NVKE, SPIRE and NICo in one release — the three charts beside this one,
installed together.

```bash
helm dependency build deploy/charts/nvke-stack
helm upgrade --install nvke-stack deploy/charts/nvke-stack \
  --namespace nvke-system --create-namespace \
  --values deploy/charts/nvke-stack/values-kind.yaml \
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
helm dependency build deploy/charts/nvke-stack  # then this
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
| `global.nvkeStack` | `nvke-nico`, to refuse a second Temporal |

Everything else is namespaced by owner and does not collide.
