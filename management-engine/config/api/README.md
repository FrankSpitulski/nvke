# Public API evaluation deployment

These Kustomize components deploy the stateless tenant API independently of the
controller. The `public-api` deployment runs the `tenant-api` binary. These are
ordinary Kubernetes manifests; Tilt only builds the image,
selects the overlay, prepares local credentials and forwards ports. The existing
controller-manager Helm chart is unchanged.

| Component | Cluster | Purpose |
| --- | --- | --- |
| `base` | Services | Two API replicas, ClusterIP Service, settings and host identity |
| `state` | Datastore | `org-example` namespace and API-only get/list/create/patch identity |
| `cwe` | Services | Select the existing `cwe-integration` datastore namespace |
| `cwe-state` | Datastore | Bind the API identity in `cwe-integration` |

The manifests explicitly enable `--evaluation-no-auth`. They are for a trusted
local environment, with no Ingress, external gateway or LoadBalancer. Both API
listeners use cleartext within that evaluation boundary. Do not expose them to
tenants before caller authentication and authorization are implemented.

The ingress NetworkPolicy denies connections from other Pods when the cluster's
network plugin enforces NetworkPolicy. Local access uses the Kubernetes API's
port forwarding; node-originated health probes remain available. NetworkPolicy
does not authenticate callers and does not isolate a Kind environment whose
network plugin lacks policy enforcement. Keep that environment trusted. A future
authenticated gateway deployment must explicitly allow its ingress traffic.

`make manifests-api` renders the default host and state YAML under `dist/`.
Future Helm packaging can consume the same component boundaries. A standalone
deployment must supply `public-api-state-client` in `management-engine-system`
with `kubeconfig`, `token` and `page-token-key` keys. The kubeconfig must select a
verified HTTPS datastore and reference `/etc/public-api/token`; the pagination
key must contain 32–64 raw bytes and remain stable across replicas and restarts.
Credential refresh must preserve that pagination key. Never commit actual
credentials or generated kubeconfigs. For local provisioning and refresh commands,
see [development credentials](../../DEVELOPMENT.md#credentials). The
[public contract](../../public/tenant/README.md#access-and-json) defines the fixed evaluation
scope and requirements for future caller-derived scope.

The storage identity cannot write status or delete resources.
Kubernetes RBAC is resource-level: the API code enforces its narrower field
policy. A compromised API credential could still patch NVKECluster spec/metadata;
field-level admission would be a separate production boundary. API handlers do
not receive CWE credentials or call CWE; provisioning belongs to the controller.

Datastore access is direct and ignores ambient proxy variables. Exec/auth-provider
plugins, basic authentication, impersonation, insecure TLS and kubeconfig proxy
URLs are rejected. Readiness performs a bounded namespace list; it demonstrates
datastore access, not controller or workload readiness. Liveness only checks the
serving process. Readiness fails when shutdown begins, while active API requests
have up to 30 seconds to drain within the Pod's 45-second termination grace period.

Each evaluation replica accepts up to 32 connections per API listener. The HTTP
gateway uses one connection to the local gRPC listener, sharing RPC controls with
native clients. A shared limit admits up to 64 decoded calls across both transports,
counting each call once. Each gRPC connection allows 16 concurrent streams.
Both listeners close connections after 30 seconds
without active requests, so idle clients eventually release the connection budget.
Reuse client connections; review these fixed evaluation limits before broader
multi-client use.
