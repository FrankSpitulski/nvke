# Tenant API contract

`cluster/v1` is the tenant workload-cluster projection. Its public resource
name is `Cluster`; its storage resource is the internal `NVKECluster`. The generated
Go client uses package alias `clusterv1`; the gRPC service is `cluster.v1.ClusterService`.

See the generated [field reference](gen/cluster/v1/FIELDS.md) and
[OpenAPI 2.0 description](gen/cluster/v1/cluster.swagger.json) for exposed
fields, routes, and schemas. The field reference includes CRD constraints;
admission, strict JSON, and lifecycle rules are described below. For setup, see the
[development guide](../../DEVELOPMENT.md#stateless-public-api-in-tilt) and
[deployment guide](../../config/api/README.md).

## Access and JSON

REST and gRPC use the same resource service for validation and persistence.
Only the tenant API is registered. It serves one configured datastore namespace
under explicit `--evaluation-no-auth`, without validating bearer tokens or
deriving scope from claims. Requests cannot select an org, project, or datastore
namespace through a path, query, or body field; no org/project resource is defined.
The exposure policy restricts representation, not caller access.

Future caller scope must come from verified authentication context and an
authorized mapping to storage. The DSX organization/project contract is not yet
confirmed, so this API does not assume its resource hierarchy or claim names.
Unverified token claims and caller-supplied scope headers must not select storage.

All JSON names are snake_case, including names configured on the protobuf
descriptors. CamelCase aliases and unknown fields are rejected. Runtime guards
also reject nulls and unsupported body/query forms before the generated gateway.
Standard protobuf JSON represents `int64` generation values as decimal strings.
Opaque `resource_version` is always a string used for concurrency, not an API
schema version. Requests are limited to 64 KiB; application requests and
datastore operations are bounded by five seconds. List pages contain at most
100 objects.

## Status

`status.state` and `status.reason` are output-only protobuf enums with generated
client constants and OpenAPI values. JSON uses their `UPPER_SNAKE_CASE` symbols,
for example:

```json
{
  "state": "STATE_PENDING",
  "reason": "REASON_AWAITING_OBSERVATION",
  "observed_generation": "2"
}
```

The service always returns a concrete state and reason. The zero
`STATE_UNSPECIFIED` and `REASON_UNSPECIFIED` values are protobuf defaults, never
service outputs. `STATE_UNKNOWN` means an operational condition prevents a
current readiness observation; it is distinct from initial or stale observations,
which remain `STATE_PENDING`. Deletion stays `STATE_DELETING`, including when
`REASON_CLEANUP_BLOCKED` reports an unknown create outcome. Private controller
messages and unreviewed reason strings are never copied into these fields.
Create and replace request types have no status field.

Regenerate clients from earlier MVP drafts: status wire types and request field
numbers changed. Clients must handle unrecognized future enum values rather than
treating them as readiness or success.
Binary protobuf and numeric ProtoJSON preserve unknown enum numbers, but strict
ProtoJSON readers reject unknown enum names. New output values therefore require
client compatibility review; Buf checks alone do not ensure older JSON clients
can consume them. The server uses standard symbolic enum JSON, without a custom
translation layer.

## Create

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

Channel/class selections and networks must also satisfy the application service's
current controller capabilities. They are illustrative values, not configured
provider bindings. Public create accepts exactly one canonical IPv4 CIDR each
for pods and services; the two ranges must not overlap. Generated descriptions
from the internal CRD also mention broader shapes such as dual-stack networking,
which this public version does not support.
The exposure policy supplies the private create defaults `cluster.local` and
disabled public access to the workload API server.

This version exposes the currently supported control-plane subset. Worker
network selection (`spec.network.vpcID`) and node pools
(`spec.nodePools[].vpcSubnetID`) exist in the internal CRD, but the current
controller rejects them and the public projection does not expose them. VPCs
and subnets must eventually reference infrastructure created by Launch Layer;
pod/service CIDRs do not replace those references. Adding worker provisioning
requires controller support and a separate public contract review.

## Replace

Replace supplies the exact resource version last read and the complete
update-writable spec:

```json
{
  "resource_version": "123",
  "spec": {
    "release": {"channel": {"name": "stable", "version": "approved-version"}},
    "control_plane_class": "standard"
  }
}
```

Replace is allowed only before the controller records a provisioning checkpoint
and while the cluster is not deleting. It cannot change `cluster_network`.
Omitting optional channel `version` removes that value; internal siblings and
create-only fields are retained. The service uses a merge patch with a
`resource_version` precondition, not an arbitrary caller-supplied patch. Generated
mapping does not enforce lifecycle or concurrency. Successful persistence does
not establish controller reconciliation or readiness.

A stale `resource_version` returns gRPC `ABORTED` / HTTP 409. Read the current
resource and reconsider the change before retrying. A duplicate create returns
`ALREADY_EXISTS` / HTTP 409. Lifecycle restrictions return `FAILED_PRECONDITION`
/ HTTP 400 through the standard gateway mapping; invalid input also returns
HTTP 400, with gRPC code `INVALID_ARGUMENT`.

## Generation and compatibility

The internal CRD schema in
`config/crd/bases/internal.nvke.nvidia.com_nvkeclusters.yaml` supplies field types,
requiredness, descriptions, and validation constraints. Handwritten
[`apis/cluster/v1/exposure.json`](apis/cluster/v1/exposure.json) selects
paths, protobuf tags, and `create`/`read`/`replace` access; unselected fields stay private.

The tenant contract lives under `public/tenant`, separately from the internal
Kubernetes types in [`apis/v1`](../../apis/v1). Ownership is explicit:

| Location | Ownership |
| --- | --- |
| `apis/cluster/v1/` | Maintained `cluster.proto`, exposure policy, HTTP/OpenAPI configuration, compatibility lock, and contract tests |
| `apis/cluster/v1/spec.proto` | Generated intermediate protobuf schema; its header marks it as generated |
| `gen/cluster/v1/` | Generated protobuf/gRPC/gateway Go, route metadata, OpenAPI, and field reference; regenerate rather than edit |
| `../../internal/public/tenant/apis/cluster/mapping/projection_generated.go` | Generated private mappings, colocated with the helpers they use |

`cluster.proto` defines resource/request envelopes, pagination, service methods,
and public status. `gateway.yaml` defines HTTP bindings; `openapi.yaml` configures
OpenAPI output. HTTP route metadata is derived from the bindings and validated
against the compiled descriptors. The generated `spec.proto` stays beside its
importing contract; generated client/server bindings stay under `gen/`. All
packages remain in the management-engine Go module.

The entrypoint is `cmd/tenant`. Resource operations and their mappings live in
`internal/public/tenant/apis/cluster`; configuration, limits, and server startup
live alongside `apis` under `internal/public/tenant`. Integration tests live in
`test/public/tenant`.

Future operator contracts and implementation belong under `public/operator` and
`internal/public/operator`, with an entrypoint in `cmd/operator`. Shared contract
definitions belong in `public/apis`, their generated bindings in `public/gen`,
and reusable server support in focused packages under `internal/public`.
Generation tooling remains in `hack/public-api`; extract shared packages when
both audiences need them.

Change the internal Go types, maintained proto, or configuration and regenerate.
For internal Go type changes, first [regenerate the CRD](../../DEVELOPMENT.md#build-and-tests).

From `management-engine`:

```sh
make generate-public-api
make check-public-api
```

Both targets install pinned local tools through `make public-api-tools`, then
run generation, Buf formatting, and lint checks. Buf uses the repository-wide
pin; protobuf and gateway plugins follow their runtime versions in `go.mod`.
Two documented Buf lint exceptions allow methods to return the shared `Cluster`
resource. The drift check compares a clean temporary output tree, including
stale artifacts after source removal. Normal generation does not update the
compatibility registry.

This initial MVP has no published public-contract baseline. `check-public-api`
checks reproducibility and the exposure/CRD lock, but does not run historical
`buf breaking` comparisons; `buf.yaml` only configures those rules. Regeneration
alone does not establish compatibility with older clients. After the first
reviewed contract lands, CI should compare against an immutable committed
contract, then the latest published API tag once available. Keep envelope
descriptor assertions until that gate covers them, and retain the lock and
behavior tests for constraints that protobuf descriptors cannot express.

For a reviewed compatible additive field, update `exposure.json`, inspect the
internal schema and operation semantics, then explicitly register it:

```sh
make public-api-tools
bash hack/public-api/generate.sh --update-lock
make check-public-api
```

`wire-lock.json` records selected paths, public names, tags, operations,
requiredness, schema constraints, and ancestor admission rules. It also freezes
private create defaults and their constraints. Removing or changing a registered
entry fails even with `--update-lock` and requires a new API version; tags cannot
be reused within a message. Additions require review of JSON/client compatibility,
controller behavior, and deployment policy beyond protobuf wire decoding.

The v1 replacement field set is frozen: adding a replacement field would let an
older client silently remove its stored value by omission. Newly exposed required
create fields are also rejected, including with `--update-lock`. Read-only and
optional create additions may be registered after review. Evolving replacement
semantics requires a new public version or a separately reviewed operation with
explicit field selection.

## Exposure compiler

An object must be selected before its children, and a child cannot gain an
operation its parent lacks. Required fields beneath populated objects need
public create exposure or an explicit private default. The compiler does not
inspect unpopulated optional private objects or interpret CRD defaults.

The compiler intentionally supports only objects, strings and atomic string
lists needed by this version. Each writable object must have a required child
exposed for the same operation; child objects follow this rule too. This ensures
the mapped fields preserve object presence. Private defaults do not satisfy this
conservative restriction. Objects that could be empty require separately defined
presence semantics before exposure. Writable lists must be required: repeated
protobuf fields cannot preserve absent versus empty input, so optional writable
lists fail generation until their presence semantics are defined. Optional
read-only lists remain supported. Selected unsupported shapes fail generation;
unknown schema fields are never exposed automatically. It derives scalar
requiredness, length/pattern limits and list cardinality validation. CEL rules
are retained in the compatibility record, but are not executed by the mapper:
the service checks supported domain behavior and the datastore remains
authoritative for admission. Collection membership semantics for richer lists,
operator contracts, general schema interpretation and OpenAPI 3 generation are
outside this version's generator.
