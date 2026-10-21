> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl validate image

Check a node image against its release evidence

### Required options

`--checks`, `--image`, `--target-arch`, `--target-ubuntu`, `--target-variant`

At least one of `--bundle`, `--checksums` is required.

`--bundle`, `--checksums` may not be used together.

`--emit-verdict`, `--sign-key` must be used together.

### Synopsis

Check a built node image against its release evidence and the versioned
check set, entirely offline, before the image is registered.

The report prints to stdout (--format json for pipelines). The command exits 0
only when every blocking check passed; any failure — a failing check or a run
that could not evaluate — exits 1, like every other nvkectl failure. A
pipeline that needs the three-way exit contract (pass / blocking fail /
could-not-evaluate) runs the standalone nvke-image-validate binary instead;
both share the same engine and flags.

--emit-verdict with --sign-key additionally writes the result as a DSSE-signed
attestation over the image's digest, which downstream gates re-check with
nvke-verdict instead of ever re-reading the image.

```
nvkectl validate image [flags]
```

### Examples

```
  nvkectl validate image --image nvke-node-fde-ubuntu-26.04.1-amd64.qcow2 \
    --bundle release-evidence/ --checks checks.yaml \
    --target-variant fde --target-arch amd64 --target-ubuntu 26.04.1
  nvkectl validate image --image img.qcow2 --checksums checksums.txt --checks checks.yaml \
    --target-variant non-fde --target-arch arm64 --target-ubuntu 26.04.1 --format json
```

### Options

```
      --boot string                   boot tier (boots a throwaway copy under QEMU, minutes): on | off (default "off")
      --boot-timeout duration         Wait bound for the boot tier's probe channel (0 = default)
      --bundle string                 Release evidence bundle directory; verifies the evidence before any check runs
      --cert-issuer string            OIDC issuer the evidence signer's certificate must carry (bundle mode) (default "https://token.actions.githubusercontent.com")
      --cert-san-regex string         Regex the evidence signer's SubjectAlternativeName must match (bundle mode) (default "^https://github\\.com/dsx-ai-factory/nvke/\\.github/workflows/os-image-artifact\\.yml@.+$")
      --checks string                 Path to checks.yaml (required)
      --checksums string              Bare checksums.txt: digest-only mode, the unverified evidence reports as a skip
      --emit-verdict string           Write the result as a DSSE-signed attestation to this path
      --format string                 Report format: text | json (default "text")
  -h, --help                          help for image
      --image string                  Path to the qcow2 under validation (required)
      --inspect string                guestfs inspection tier: auto (on when guestfish is on PATH) | on | off (default "auto")
      --luks-passphrase-file string   File holding the LUKS passphrase for FDE images
      --name string                   Artifact release name; defaults to the image file's basename
      --sign-key string               PEM private key (Ed25519 or ECDSA P-256) that signs the verdict
      --target-arch string            Target architecture: amd64 | arm64 (required)
      --target-bom string             Target BOM YAML (components: {name: version}); omitted, the BOM-driven COMPAT checks report a skip
      --target-ubuntu string          Target Ubuntu release, e.g. 26.04.1 (required)
      --target-variant string         Target variant: fde | non-fde (required)
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl validate](nvkectl_validate.md)	 - Validate a file before acting on it

