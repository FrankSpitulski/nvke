# capnico

CAPNICo, the Cluster API infrastructure provider for NICo: its published chart,
plus the credentials Secret CAPNICo reads but does not create.

CAPNICo is **not** NICo. It reconciles `NicoCluster` and `NicoMachine` objects
against a NICo API that already exists. NICo itself is [`../nico`](../nico/).

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

Unlike NICo's, CAPNICo's chart and the image it references are both **public**,
and the chart is stamped at publish time with the matching controller image. No
repository alias, no image override, no pull secret.

## This chart needs Helm 4

NVKE pins `v4.2.4` in `make/tool-versions.mk`. Verified on helm 4.2.4 and
reproduced as failing on helm 3.19.

CAPNICo's chart declares its own namespace, as `{{ .Release.Namespace }}` with
`helm.sh/resource-policy: keep`. On helm 3 there is no working combination:

| | helm 3.19 | helm 4.2.4 |
|---|---|---|
| with `--create-namespace` | `namespaces "capnico-system" already exists` | works |
| without it | `create: failed to create: namespaces "capnico-system" not found` — helm cannot write its release record | — |

So pass `--create-namespace`, and never `kubectl create namespace` first. No
error message mentions the helm version.

The credentials Secret is the one thing this chart adds. The documented manual
route creates it *after* the install, in a second step, because the namespace
does not exist until then. Inside one release that ordering is free — helm
applies a `Namespace` before a `Secret` — so this chart does both at once.

## It is not in the nvke-nico-stack umbrella

Every object in CAPNICo's chart takes its namespace from
`{{ .Release.Namespace }}` directly — 29 references across 25 template files,
with no `namespaceOverride`. The chart's own
`cluster-api-provider-nico.namespaceName` helper exists but is referenced
nowhere.

As a subchart of an umbrella released into `nvke-system`, all 27 CAPNICo
objects would therefore land in `nvke-system`, and the chart's `Namespace`
object would collide with the umbrella's own namespace:

```
namespaces "nvke-system" already exists
namespaces "capnico-system" not found
```

Release this chart on its own. See
[`../nvke-nico-stack/README.md`](../nvke-nico-stack/README.md).

## Pick one route and stay with it

There is more than one way to install CAPNICo, and they do not mix:

- this chart
- its published chart installed directly
- `clusterctl init --infrastructure nico`

All three create the same `capnico-system/controller-manager` Deployment under
the same names. Installing two does not give two installs; it gives two things
claiming the same objects. Helm over a `clusterctl` install fails outright on
ownership metadata. `clusterctl` over a Helm install patches Helm's resources
and adds its own inventory entry, leaving the release and the cluster
disagreeing about what is installed.

See [infrastructure-providers.md](../../../docs/site-bootstrap/infrastructure-providers.md).

## The annotations are a two-sided contract

`--reboot-annotation` and `--repair-annotation` name what CAPNICo watches for
and what the NVKE controller writes. They have to agree with the controller's
`identity.fieldManagerRebootMachineName` and
`identity.fieldManagerMachineHealthIssueName` chain. Change them on one side
only and nothing reboots or repairs, with no error on either side.

## Cluster API is a prerequisite

`clusterctl init` installs Cluster API core and the kubeadm providers, and
cert-manager with them. CAPNICo installs and runs without it, and reconciles
nothing — the manager logs against kinds the API server does not know.

## `CONFIGURED` is a narrow claim

`nvkectl get providers` reads `CONFIGURED` when the CRDs are established, the
controller is running and a credentials Secret is present. It does **not**
check that Cluster API is installed, that the credentials work, or that the
backend answers. CAPNICo ships its own CRDs, so `nico` reads `CONFIGURED` even
on a cluster with no Cluster API at all.

The `CREDENTIALS` column reports whether the credentials work, from a check
CAPNICo runs every few minutes.

Before creating anything, check that the instance types have allocations. An
instance type with no allocation looks healthy and provisions nothing, and
machines retry forever in `InstanceTypeUnavailable`.
