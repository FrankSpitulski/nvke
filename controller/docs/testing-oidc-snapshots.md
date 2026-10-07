# Testing OIDC provider snapshots

The controller has passing tests for the authentication configuration that init
and join need to consume. These tests do not claim that either activity is wired
yet. Run every command on this page from `controller/`.

Run the in-process issuer, contract, runner, and token-only probe tests with:

```bash
make test-oidc-fixtures
```

Run the real API server matrix with:

```bash
make test-oidc-reference
```

The matrix asks `setup-envtest` for the repository's central
`ENVTEST_K8S_VERSION` pin, currently Kubernetes 1.34.1. It renders the input
with `BuildCombinedAuthenticationConfig` and
`GenerateAPIServerAuthenticationConfig`. The cases cover the shared
provider's `sub` username without a groups mapping, ordinary operator claims,
reserved trust-domain and exact-subject policy, and a protected value split
between a mapping prefix and a claim. All requests use the token-only probe.
The RBAC control records a 200 for ConfigMaps and a 403 for Secrets. Rejected
identities return 401.

## Snapshot contract

`PinClusterOIDCProviders` writes a cluster's operator providers only when the
`oidc_providers` column is SQL `NULL`. The first call wins, including a call
with no providers. It returns the stored value after the conditional update, so
concurrent callers converge on one snapshot.

`GetCluster` returns that value in `Cluster.OIDCProviders`. A nil slice means
the cluster has no snapshot. An empty, non-nil slice means the controller pinned
a snapshot with no operator providers. There is no separate
`GetClusterOIDCProviders` method.

The database snapshot does not contain the shared management provider, its
resolved CA bundle, or `OperatorJWTPolicy` values such as the management trust
domain and reserved subjects. Init and join still need those inputs before they
can call the production builder. The controller also has not wired
`controllerconfig.Load` into process startup, so activities cannot receive
the mounted provider list through an established startup path yet.

The operator-provider snapshot alone cannot guarantee byte-identical init and
later join authentication files if those unpinned inputs change. Byte identity
holds only while all unpinned inputs stay fixed. Disabling shared management
authentication and using an empty policy removes those unpinned inputs.

Existing clusters can have a nil snapshot. The product policy for those
clusters is unresolved. Join must not silently fall back to the current
controller file. The implementation needs an explicit legacy policy before a
test can fix the nil-snapshot behavior.

## Opt-in wiring regressions

The activity regressions use the `oidc_snapshot_wiring` build tag:

```bash
make test-oidc-snapshot-wiring
```

This target is expected to fail until init pins and uses one provider snapshot
and join reads the stored snapshot. It stays out of the passing fixture and
reference targets so the ready test helpers remain useful during that work.

On base `b0fead11`, two controls pass:

- `TestOIDCSnapshotWiringSharedProviderControl`
- `TestOIDCSnapshotWiringJoinWithPinnedEmptyProvidersKeepsSharedProvider`

Three regressions fail:

- `TestOIDCSnapshotWiringInitUsesPinnedProviders` gets only the shared
  provider because init does not use the snapshot returned by
  `PinClusterOIDCProviders`.
- `TestOIDCSnapshotWiringInitStopsOnPinError` gets a nil error because init
  does not call the pin operation.
- `TestOIDCSnapshotWiringJoinUsesPinnedProviders` ignores the stored operator
  provider and tries the current shared-provider config, which has no issuer in
  the fixture.

`TestOIDCSnapshotWiringJoinUsesPinnedProviders` checks that join selects the
stored operator provider. It does not prove byte identity with an earlier init.

A changed-mounted-file init case remains deferred. Activities have no
established input for the config loaded by `controllerconfig.Load`, and process
startup does not call that loader. Adding a test-only setter or guessed startup
API would lock in an interface before the production wiring exists.
