# Dex OIDC smoke test

This directory creates short-lived Dex credentials, a self-signed TLS certificate, and a password-grant client for one Docker smoke test. It never writes those values outside Go's temporary test directory.

Run this from `controller/`:

```bash
make test-oidc-dex
```

The test needs Docker with access to its daemon, Python 3 with the `bcrypt` module, and a local copy of the pinned Dex image. Pull it once if it is absent:

```bash
docker pull ghcr.io/dexidp/dex@sha256:8499afd690c437f52301efd2b05b2455da5bd2dfc20332cd697dc9937f808462
```

The test runs Dex directly through Docker. It does not need Kind. Docker publishes the test port only on `127.0.0.1`. The test verifies a TLS ID token against discovery and JWKS, rejects a bad password, and removes its named container before it exits.
