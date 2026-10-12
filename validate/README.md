# validate/

`nvke-image-validate` checks a built NVKE node image against its release
evidence, entirely offline: the image's digest against the release's (attested)
`checksums.txt`, and the artifact's declared identity against the validation
target. Deeper tiers — filesystem facts via libguestfs and behavioral checks
from a throwaway QEMU boot — land behind the same check registry.

```console
$ nvke-image-validate \
    --image nke-node-fde-ubuntu-24.04.5-amd64.qcow2 \
    --checksums checksums.txt \
    --checks checks.yaml \
    --target-variant fde --target-arch amd64 --target-ubuntu 24.04.5
nke-node-fde-ubuntu-24.04.5-amd64.qcow2 (sha256:49647c…)
check set 0.1.0
  PASS   INT-001      sha256 49647c… matches release checksums entry …
  PASS   COMPAT-001   name declares fde/amd64/ubuntu-24.04.5, matching the target
pass=2 fail=0 (blocking=0 warn=0) skip=0 error=0
```

Exit codes: `0` pass, `1` a blocking check failed, `2` the run could not
evaluate (error always outranks fail). `--format json` emits the schema-v1
report for pipelines.

See [AGENTS.md](AGENTS.md) for layout, constraints and verification targets.
