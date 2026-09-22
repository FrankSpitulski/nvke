# Testing OIDC

The controller has local checks for Kubernetes API server OIDC authentication.
Run them from `controller/`. Both targets need Go, GNU Make, and Python 3.

`test-oidc-fixtures` checks the controlled issuer, reference authentication
configuration, envtest setup, and the token-only Kubernetes client. It does not
start a Kubernetes API server.

```bash
make test-oidc-fixtures
```

`test-oidc-reference` starts Kubernetes 1.34.1 `kube-apiserver` and etcd
processes with envtest. The three cases check the shared and external provider
layout, protected username and group prefixes, and a narrow RBAC grant that
returns 200 for ConfigMaps, 403 for Secrets, and 401 for a rejected identity.

```bash
make test-oidc-reference
```

The first reference run installs the pinned `setup-envtest` release and fetches
the Kubernetes binaries. The target fails if any required binary is absent or
if the three reference cases do not all run.

## Keep an issuer running with Tilt

Add `"oidc-test-issuer"` to the `providers` array in `tilt_config.json`, then
start the usual controller Tilt stack. The `oidc-test-issuer` resource runs at
`https://oidc-test-issuer.oidc-test.svc:8443` inside the cluster and forwards
local port 8443. Its certificate covers the service DNS name.

The persistent issuer is available for cluster and host inspection. The
reference target still creates isolated issuers and an isolated API server for
each run.

Tilt leaves `test-oidc-reference` manual so API server processes do not restart
on every source edit. Run it from another terminal when needed:

```bash
tilt trigger test-oidc-reference
```

See the [controlled issuer guide](../hack/oidc-test-issuer/README.md) for the
standalone Tilt command and CA-verified discovery requests from a pod or host.
