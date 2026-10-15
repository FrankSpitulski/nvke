# Tenant API contract

`cluster/v1` defines the public `Cluster` representation of the internal
[`NVKECluster`](../../apis/v1/nvkecluster_types.go). The generated Go package is
`clusterv1`; the gRPC service is `cluster.v1.ClusterService`. The
[application service](../../internal/public/tenant/apis/cluster/service.go)
implements the contract using generated bindings and spec mappings. The
[standalone deployment](../../config/api/README.md) runs independently of the controller.

Hand off the generated [OpenAPI 2.0 document](gen/cluster/v1/openapi.yaml) to API
consumers. This standalone YAML includes routes, nested schemas, required fields,
constraints, and lifecycle descriptions; its references resolve within the file.
The gateway generator retains some path/query limits only in descriptions;
those limits and cross-field rules remain part of the contract.
The generated [field reference](gen/cluster/v1/FIELDS.md) also lists spec exposure.

## Contract and ownership

[`cluster.proto`](apis/cluster/v1/cluster.proto) defines create, get, list, and
replace operations, request/resource envelopes, pagination, and public status
enums. Public field names are snake_case. Create and replace use separate spec
types; their requests cannot set status or arbitrary Kubernetes metadata.
`resource_version` carries an opaque datastore concurrency token; it is not the
public API version.

The [exposure policy](apis/cluster/v1/exposure.json) selects internal spec paths,
protobuf tags, and `create`/`read`/`replace` access. Unselected fields stay private.
Its `input` options can narrow writable list bounds and provide a public
description; read projections retain the internal schema's bounds.
These operations describe field representation, not caller authorization.
The contract defines no org/project resource or caller-selected storage namespace.

| Location | Ownership |
| --- | --- |
| `apis/cluster/v1/` | Maintained `cluster.proto`, exposure policy, HTTP/OpenAPI configuration, compatibility lock, and contract tests |
| `apis/cluster/v1/spec.proto` | Generated intermediate protobuf schema imported by `cluster.proto` |
| `gen/cluster/v1/` | Generated protobuf/gRPC/gateway Go, route metadata, OpenAPI, and field reference |
| [`mapping`](../../internal/public/tenant/apis/cluster/mapping) | Generated private spec mappings, maintained helpers, and projection tests |

The compiler in [`hack/public-api`](../../hack/public-api) derives spec types,
requiredness, descriptions, and validation constraints from the
[internal CRD](../../config/crd/bases/internal.nvke.nvidia.com_nvkeclusters.yaml).
[`gateway.yaml`](apis/cluster/v1/gateway.yaml) defines HTTP bindings;
[`openapi-options.yaml`](apis/cluster/v1/openapi-options.yaml) configures OpenAPI
output. Generated HTTP route metadata is checked against the compiled protobuf
descriptors.

Generated files have ownership headers and must be regenerated rather than
edited. For spec schema review, read [`spec.proto`](apis/cluster/v1/spec.proto).
The escaped `rawDesc` constants in generated Go encode that schema for the
protobuf runtime.

Future operator contracts can use `public/operator` and
`internal/public/operator`; extract shared definitions or helpers when both
audiences need them.

## Runtime integration decision

The [server](../../internal/public/tenant/server/server.go) connects the generated
HTTP gateway to its gRPC listener through a generated client. HTTP and native gRPC
share RPC admission and interceptors, with each call counted once. HTTP parsing
guards remain before the gateway. Tilt and standalone deployments use this same
wiring in one process. Authentication and authorization are still deferred, and
future identity verification must cover both transports.

## Access and JSON

The MVP requires `--evaluation-no-auth` and serves one deployment-configured
namespace. It has no caller authentication or authorization and must remain in a
trusted evaluation environment. Future scope must come from verified identity
and an authorized storage mapping, never unverified claims or caller-selected
namespace headers. See the [deployment security boundary](../../config/api/README.md).

HTTP accepts snake_case field names and rejects unknown fields, camelCase aliases,
explicit nulls, duplicate keys and unsupported body/query forms. Requests are
bounded by size, deadlines and shared RPC admission. Protobuf JSON represents
`int64` generation values as decimal strings.

## Resource operations

Create supplies `name` and `spec`, for example:

```json
{
  "name": "example",
  "spec": {
    "release": {"channel": {"name": "stable"}},
    "control_plane_class": "standard",
    "cluster_network": {
      "pods": {"cidr_blocks": ["10.244.0.0/16"]},
      "services": {"cidr_blocks": ["10.96.0.0/12"]}
    }
  }
}
```

The channel and class must resolve to configured provisioning selections.
Create accepts one canonical IPv4 CIDR per network, with no overlap. Worker VPC
and subnet selections remain private until the provisioning integration supports them.

Replace supplies the last `resource_version` and the complete writable spec:

```json
{
  "resource_version": "123",
  "spec": {
    "release": {"channel": {"name": "stable", "version": "approved-version"}},
    "control_plane_class": "standard"
  }
}
```

Replacement is allowed before a provisioning checkpoint exists and while the
cluster is not deleting. It preserves create-only and private fields. Omitting
optional channel `version` removes it. The service sends a merge patch guarded
by the stored resource version, so concurrent status or metadata changes also
produce a conflict. Read the current resource before retrying.

Duplicate create and stale replacement return HTTP 409 (`ALREADY_EXISTS` and
`ABORTED`). Invalid input and lifecycle restrictions return HTTP 400
(`INVALID_ARGUMENT` and `FAILED_PRECONDITION`). Persistence does not establish
provisioning success. GET and LIST project stored configuration and status,
without waiting for reconciliation.

`status.state` describes the current lifecycle assessment. `status.reason` is
omitted when no reviewed detail applies, including ordinary READY and DELETING
states. `REASON_PROVISIONING` requires an explicit provisioning observation.
Missing or stale observations remain PENDING, and unavailable readiness is
UNKNOWN. Private diagnostics never become public reasons. `Cluster.uid` is stable for
the resource lifetime and distinguishes a later resource created with the same name.

## Generate and test

Change the internal Go types, maintained proto, or configuration and regenerate.
For internal type changes, first [regenerate the CRD](../../DEVELOPMENT.md#build-and-tests).
From `management-engine`:

```sh
make generate-public-api
make test-public-contract test-unit
```

`test-public-contract` runs `check-public-api` before testing the descriptors,
compiler, and HTTP bindings. `test-unit` includes the mapping tests; `make test`
includes both suites. `check-public-api` can also be run alone to verify drift.

Generation installs pinned local tools, checks Buf formatting/lint, and generates
clients, gateway handlers, and OpenAPI. Buf uses the repository-wide pin; protobuf
and gateway plugins follow their runtime versions in `go.mod`. Annotation schemas
come from that same pinned gateway module in the temporary generation tree. The
drift check compares a clean temporary output tree and detects stale artifacts.
Normal generation does not update the compatibility registry.

## Compatibility

This initial contract has no published baseline. `check-public-api` verifies
reproducibility and the exposure/CRD lock; it does not run historical `buf breaking`
comparisons. `buf.yaml` configures those rules for a future baseline check.
Envelope descriptor assertions cover field numbers until that gate is available.

[`wire-lock.json`](apis/cluster/v1/wire-lock.json) records selected paths, public
names, tags, operations, requiredness, schema constraints, ancestor admission
rules, and private create defaults. Removing or changing an entry requires a new
public version. The replacement field set is frozen: adding a field would let an
older replacement client remove its stored value by omission. New required create
fields are also rejected. Reviewed read-only and optional create additions can
be registered explicitly:

```sh
make public-api-tools
bash hack/public-api/generate.sh --update-lock
make check-public-api
```

Additions still require JSON/client and operation-semantics review. In particular,
strict ProtoJSON readers reject unknown enum names even though binary protobuf
preserves unknown numeric enum values. New status values need compatibility review.

## Mapping boundary

The compiler supports objects, strings, and atomic string lists. Parents must be
selected before children, and children cannot gain operations their parent lacks.
Required fields beneath populated objects need create exposure or an explicit
private default. Unpopulated optional private objects are not inspected, and CRD
defaults are not interpreted.

Writable objects require a required child exposed for the same operation, and
writable lists must be required. These restrictions preserve presence despite
protobuf's inability to distinguish an absent repeated field from an empty one.
Optional read-only lists are supported. Unsupported selected schema shapes fail
generation; internal schema growth never exposes fields automatically.

Mappings validate scalar constraints and list cardinality, project selected spec
fields, and retain private/create-only fields during replacement. They do not
enforce authentication, authorization, networking semantics, lifecycle, or
optimistic concurrency. CEL rules are recorded for compatibility review but are
not evaluated by mappings; the datastore remains authoritative for admission.

### Private create values

The [exposure policy](apis/cluster/v1/exposure.json) supplies required internal
configuration that tenants cannot set at this time. The compiler rejects defaults
on public create fields, so these assignments never override tenant input.

| Stored field | Value | Rationale |
| --- | --- | --- |
| `apiServerAccess.publicAccessEnabled` | `false` | Public API-server access configuration is unsupported by the management-engine provisioning integration at this time. |
| `clusterNetwork.serviceDomain` | `cluster.local` | Matches the DNS domain configured by CWE's kubeadm renderer. Custom service domains are unsupported at this time. |

The [provisioning validation](../../controllers/nvkecluster_resolve.go) rejects
public access and domains other than `cluster.local`. CWE selects that domain in
its [defaults](../../../controller/internal/kubeadmconfig/render.go) and applies it
to [kubeadm networking](../../../controller/internal/kubeadmconfig/init.go).
An empty service domain is invalid under the [CRD](../../config/crd/bases/internal.nvke.nvidia.com_nvkeclusters.yaml).

These are service-selected spec values, not observations of a provisioned cluster.
They are excluded from tenant reads and writes. Replacement preserves stored values.
