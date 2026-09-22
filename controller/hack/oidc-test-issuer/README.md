# Controlled OIDC test issuer

This program runs the controlled issuer used by the OIDC reference tests.

The issuer creates its CA, TLS material, and signing keys at runtime. It makes
valid and invalid tokens for a directly configured Kubernetes API server. Its
TLS and signing keys stay in memory. The runtime volume holds the CA, rendered
reference configuration, and generated tokens.

Run the standard checks from `controller/`:

```bash
make test-oidc-fixtures
make test-oidc-reference
```

Keep generated keys and bearer tokens in the fixture runtime. Do not put them
in checked-in files, command arguments, environment variables, or logs.

## Run it with Tilt

The normal controller Tilt stack leaves this fixture off. Add
`"oidc-test-issuer"` to `providers` in `controller/tilt_config.json`, then run
`tilt up`. Tilt builds and loads `docker.local/oidc-test-issuer:dev`, deploys
the issuer in `oidc-test`, and exposes the manual `test-oidc-reference`
resource. Tilt reads `config.json` when it builds the issuer ConfigMap. Trigger
the test resource in the Tilt UI or with:

```bash
tilt trigger test-oidc-reference
```

To run only the issuer, use a Kind context and this directory's Tiltfile:

```bash
export KUBECONFIG=/path/to/kind.kubeconfig
tilt up -f hack/oidc-test-issuer/Tiltfile
```

The issuer URL stays `https://oidc-test-issuer.oidc-test.svc:8443`. Pods reach
it through cluster DNS. A host can inspect discovery through Tilt's localhost
port forward while preserving that hostname with curl `--connect-to` and the
runtime CA at `/run/nke-oidc-test-issuer/ca.crt` in the issuer pod. For example:
Keep `KUBECONFIG` exported in the terminal that runs these `kubectl` commands.

```bash
pod="$(kubectl -n oidc-test get pod -l app.kubernetes.io/name=oidc-test-issuer -o jsonpath='{.items[0].metadata.name}')"
kubectl -n oidc-test cp "$pod:/run/nke-oidc-test-issuer/ca.crt" /tmp/oidc-test-issuer-ca.crt
curl --fail --noproxy '*' --cacert /tmp/oidc-test-issuer-ca.crt \
  --connect-to oidc-test-issuer.oidc-test.svc:8443:127.0.0.1:8443 \
  https://oidc-test-issuer.oidc-test.svc:8443/.well-known/openid-configuration
```
