# nico

NICo for an NVKE cluster: the NICo umbrella chart, with the prerequisites its
own `PREREQUISITES.md` expects to find already in place.

NICo is published separately from NVKE, so this chart names no registry. Both
its chart repository and its images are inputs:

```bash
helm repo add nico <your NICo chart repository>
helm dependency build deploy/charts/nico

ssh-keygen -t ed25519 -f ./ssh_host_ed25519_key -N ""

helm upgrade --install nvke-nico deploy/charts/nico \
  --namespace forge-system --create-namespace \
  --values deploy/charts/nico/values.yaml \
  --values deploy/charts/nico/values-kind.yaml \
  --set database.password=<pick one> \
  --set-file sshHostKey.privateKey=./ssh_host_ed25519_key \
  --set nico.global.image.repository=<repo> \
  --set nico.global.image.tag=<tag>
```

## What this chart adds

NICo's own chart assumes a prepared cluster. This chart supplies what sections
2 to 5 of its `PREREQUISITES.md` ask for:

| Created | For |
|---|---|
| A PostgreSQL instance | `nico-api`. The schema is created and migrated by the migration job in the `nico-api` subchart |
| `nico.forge-pg-cluster.credentials` | `nico-api`'s six database keys |
| `nico-vault-approle-tokens`, `nico-vault-token` | Mounted by NICo's services whether or not a Vault exists |
| `ssh-host-key` | `nico-ssh-console-rs` |
| A `vault-nico-issuer` ClusterIssuer | Every NICo service certificate |
| Its own Temporal, optionally | NICo's workflows, in a namespace of its own |

The cluster operators in section 1 are not installed: a seed already has
cert-manager, and Vault is a site decision.

## Replacing Vault PKI

NICo signs its service certificates through a cert-manager `ClusterIssuer`
backed by a Vault PKI engine, and expects it to be called `vault-nico-issuer`.

With a Vault, leave `certManager.issuer.create` false and point
`certManager.issuer.name` at the ClusterIssuer that Vault backs. Without one —
a kind cluster, a laptop, a test lane — set it true and this chart creates a
self-signed CA chain under the same name. NICo cannot tell the difference: it
only ever names the issuer.

**This is not a substitute for Vault PKI on a real site.** A self-signed CA has
no revocation, no audit trail and no rotation story, and its private key lives
in a Secret in this cluster.

## The SSH host key is supplied, never generated

`nico-ssh-console-rs` presents it to every client that opens a machine console.
Helm would mint a new key on every `helm upgrade`, and every client holding the
old one would then report a host-key mismatch. Generate it once, keep it, and
pass it with `--set-file`. The Secret carries
`helm.sh/resource-policy: keep` for the same reason.

## Namespaces

Each NICo subchart resolves its namespace from its own `namespaceOverride`, and
there is no global that sets them all — so `values.yaml` pins every one of them
to `namespaces.nico`. Without that, NICo lands in whichever namespace this
chart is released into, which inside the `nvke-nico-stack` umbrella is the
umbrella's namespace rather than NICo's; the Secrets this chart writes would
then be in one namespace and the services that mount them in another.

`templates/validate.yaml` refuses an install where any subchart disagrees with
`namespaces.nico`, so this cannot drift silently.

## NICo cannot share a release with its namespace or its database

NICo's chart runs its config ConfigMap and its schema migration Job as
`pre-install,pre-upgrade` hooks. Helm applies hooks **before every regular
manifest in the release**, so anything those hooks depend on has to exist
before the release is applied. Two things do:

**The namespace.** A `Namespace` declared in this release is created after the
hooks that need it, so this chart does not declare one.
`templates/validate.yaml` refuses the install when it is missing. Released on
its own, `--create-namespace` covers it. Inside an umbrella released elsewhere,
run `kubectl create namespace forge-system` first.

**PostgreSQL.** The migration Job connects to the database. With
`postgresql.enabled=true` that database is a StatefulSet in this same release,
applied only after the hook has already failed. For a single `helm` apply, set
`postgresql.enabled=false` and point `database.host` at a PostgreSQL that
already exists. With `postgresql.enabled=true`, install this chart as its own
release so the database is in place before NICo's hooks run on a later upgrade.

This is how upstream runs it too: NICo's own `setup.sh` installs PostgreSQL
from a separate release before the NICo chart, and the NICo chart ships no
database of its own.

## NICo's Temporal

`temporal.enabled` is off by default, and **refused inside the `nvke-nico-stack`
umbrella**. Temporal's chart has no `namespaceOverride`, so NICo's Temporal and
the NVKE controller's would land in the same namespace and take each other's
Service names. To give NICo one of its own, release this chart on its own, into
its own namespace, with `temporal.enabled=true`. Its Services are named
`nico-temporal-*` either way.

## What a seed does not get

DPF and RMS are not part of the NICo chart and are not installed. The optional
subcharts — `nico-dsx-exchange-consumer`, `nico-ntp`, `unbound`,
`nico-machine-a-tron`, `nico-postgres-exporter` — are off: a seed runs a
reduced NICo.

## The resource pools are placeholders

`nico-api` **exits at startup** without resource pools, so
`nico.nico-api.siteConfig` is not optional. The four pools in `values.yaml` are
the required minimum and their ranges are examples. Replace them with the
site's address plan.
