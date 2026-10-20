# Bootstrap E2E

Run the normal build-and-test flow with `make -C cli test-e2e-bootstrap`.
To test an externally built executable instead, from the repository root:

```sh
sudo -E env "PATH=$PATH" make -C cli test-e2e-bootstrap-artifact \
  NVKECTL=/absolute/path/to/nvkectl \
  NVKECTL_EXPECTED_VERSION=<expected-build-version>
```

The artifact target requires both inputs and does not build or download the CLI.
A relative executable path is relative to `cli/`. The script checks the supplied
executable's short version before preparing fixtures, then uses that same path
for site-spec generation and bootstrap. It logs SHA-256 before preparation and
again after fixture cleanup. Changed or unavailable bytes fail an otherwise
successful run. A version check is not a successful bootstrap result.

Use an isolated checkout and a disposable Linux host: the suite uses fixed
resource names and modifies host forwarding, addresses and routes. The caller
supplies root access, Go, Make, GNU coreutils/timeout, Kind, kubectl, Helm,
clusterctl, jq for VIP checks, `/dev/vtpmx`, and nerdctl backed by standalone
containerd and BuildKit. The controller image named by `NVKECTL_NVKE_CONTROLLER_IMAGE`
must already be in that containerd image store. See the repository workflow
`.github/workflows/nvkectl-bootstrap-e2e.yml` and host installer
`cli/hack/bootstrap-e2e-host.sh` for the existing upstream preparation.

The suite retains its supporting builds: OVN/Helm packages, fake-node/agent and
provisioned-provider images. It needs network access for dependencies, images,
charts and the live cross-bridge fixture; it cannot run as an offline unit test.
`run.sh` owns the fake nodes, bootstrap cluster, BGP speaker and forwarding/VIP
changes. Host services, preloaded images and build caches remain caller-owned.

Version checking has a 30-second deadline and five-second kill grace. SIGINT or
SIGTERM stops and joins that process group. During fixture execution the script
stops the background bootstrap group before removing its cluster, allowing five
seconds for termination before SIGKILL. Existing bootstrap and readiness deadlines
remain in `run.sh`; there is no new end-to-end deadline. Signal the suite's
process group when cancelling synchronous supporting builds.

Fixture teardown and diagnostics remain best effort. Some removals suppress
errors, so an exit status of zero is not proof of exhaustive teardown. Failed
runs can retain `cli/cluster-diagnostics`; inspect the host before reuse. This
interface does not yet provide a machine-readable terminal completion/cleanup
report. All bootstrap assertions remain in `run.sh`.

`artifact_test.go` exercises input admission, supplied-process failures,
preparation failure, timeout, cancellation and process-group cleanup with tool
and preparation doubles. These contracts do not provision a real cluster.
