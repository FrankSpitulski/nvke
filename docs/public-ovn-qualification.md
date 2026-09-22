# Public OVN qualification runbook

This runbook qualifies fresh, temporary NKE test clusters using the public OVN-Kubernetes 1.4.0 package stream on amd64 and arm64. It preserves the existing Calico scenarios and support; it is not a migration plan.

## What is implemented and what remains evidence

The qualification work adds an `ovn-qualification` Ginkgo label grouping OVN smoke, multi-pod-CIDR, and lifecycle coverage. Both default architecture lanes select the OVN smoke and multi-pod-CIDR scenarios. Each selected required assertion must pass. A runtime skip, missing result, or failure does not qualify that architecture. The shared multi-CIDR assertion intentionally skips in scenarios that do not request multiple CIDRs; the dedicated multi-CIDR scenario must run it.

Those code and selection changes make the intended tests runnable. They are not live qualification evidence. A candidate is qualified only when the package, cluster, networking, and lifecycle results in this document are recorded as passed for **each** architecture. Record pending work as pending; do not infer a pass from a green build-only job.

## Candidate and package checks

Run package reproduction on a Linux host for each architecture. The package recipe intentionally rejects non-Linux hosts.

```sh
make -C os-image/packages/cni-ovn repro-check ARCH=amd64
make -C os-image/packages/cni-ovn repro-check ARCH=arm64
```

The public locks are `os-image/packages/cni-ovn/lock/{amd64,arm64}/1.4.0.lock`. Record the lock file hash and its per-architecture image manifest digest. Do not regenerate locks as part of qualification.

On each temporary node image, confirm that normal cluster provisioning installed the stream-suffixed packages from the local apt repository without an equality pin. Debian package revision is independent of the OVN stream; `=1.4.0` is not an appropriate apt pin.

```sh
apt-cache policy nke-cni-ovn-charts-1.4.0 nke-cni-ovn-images-1.4.0
dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\t${Status}\n' \
  nke-cni-ovn-charts-1.4.0 nke-cni-ovn-images-1.4.0
dpkg-query -L nke-cni-ovn-charts-1.4.0
ovs-vsctl --version
uname -r
```

Capture the exact tuple: node-image tag and digest, architecture, kernel, Open vSwitch version, installed package versions, OVN stream, Kubernetes version, and the lock manifest digest.

## Fresh-cluster run

Use the existing manual workflow inputs. Supply one image tag that exists for both architectures; use a placeholder until a concrete candidate is selected.

```text
runtime-profile: qualification
os-image-tag: <candidate-os-image-tag>
```

The qualification runtime profile selects exactly `nico-vms && ovn-qualification`, ignoring any custom label filter. It allows 480 minutes for the suite and 540 minutes for the job, with one Kubernetes upgrade hop. Default CI retains its existing budgets. The default profile accepts custom filters for debugging, but partial selections do not establish full qualification. These are execution limits; live timing still needs to be measured.

Run the selected suite on amd64 and arm64. It creates fresh temporary OVN clusters. The default architecture lanes retain Calico coverage separately. The required OVN results are: smoke readiness, multi-pod-CIDR primary and secondary subnet allocation, same-node and bidirectional cross-node traffic, service DNS, pod-to-node and node-to-pod reachability, then scale, reboot, and supported Kubernetes upgrade with traffic rechecked after every transition.

Host OVS is a prerequisite: the agent requires a working `ovs-vsctl` connection before it deploys OVN. Capture its version and readiness result as part of the tuple above.

For a local run, the same runner can use a qcow2 already built on the host:

```sh
CAPNICO_SOURCE_DIR=/absolute/path/to/provider-checkout \
NKE_E2E_OS_IMAGE_PATH=/absolute/path/to/nke-node.qcow2 \
NKE_E2E_LABEL_FILTER='nico-vms && ovn-qualification' \
controller/hack/ci/nico-vms-e2e.sh
```

The local image must be a readable regular file with an absolute path and a
qcow2 format reported by `qemu-img`. Paths containing whitespace are rejected
because the current iPXE parser tokenizes `image_url` on whitespace. Do not set
`NKE_E2E_OS_IMAGE_TAG` at the same time. The runner records the selected
file's SHA256 in `os-image.sha256` under its state directory and repoints the
same shared profiles used by the registry path.

Use the non-FDE LVM image target that the existing CI registry path selects.
The fixture profile names include `fde`, but that does not change the image
target for this lane. A filename or local SHA256 establishes file identity; it
does not prove the guest architecture. Qualification still requires a native
run on each architecture and the running-image evidence described below.

## Running-image provenance

For every OVN node, preserve the CRI image listing and the OVN workload image IDs, for example with `crictl images` and Kubernetes pod status. Match the image reference and the architecture-specific manifest digest to that architecture's committed lock.

Do not compare arbitrary image IDs blindly to the lock digest. A Kubernetes `imageID` or runtime-reported ID may identify an image configuration or a locally normalized runtime object, while the lock records the platform-specific **manifest** digest. First establish which digest form each tool reports; use a manifest-aware runtime or registry inspection when needed, and retain both raw outputs and the comparison method in the evidence.

## Required evidence record

Native qualification completed on 2026-09-22 using the local non-FDE Ubuntu
26.04.1 images built from source revision
`6de9506a698cfeaf12497f0628610922bb535bb7`.

| Field | amd64 | arm64 |
|---|---|---|
| Native package reproducibility | Passed, two byte-identical builds | Passed, two byte-identical builds |
| Guest kernel | `7.0.0-1018-nvidia` | `7.0.0-1018-nvidia-64k` |
| Open vSwitch / database schema | `3.7.1` / `8.8.0` | `3.7.1` / `8.8.0` |
| Kubernetes version / OVN stream | `1.34.2`, control plane upgraded to `1.35.3` / `1.4.0` | `1.34.2`, control plane upgraded to `1.35.3` / `1.4.0` |
| Installed charts and images packages | Stream `1.4.0`, Debian revision `1`, native architecture, local apt origin | Stream `1.4.0`, Debian revision `1`, native architecture, local apt origin |
| Initial readiness, OIDC and OVS RPC | Passed | Passed |
| Multi-CIDR allocation and connectivity | Passed in corrected scenario | Passed in corrected scenario |
| NetworkPolicy ingress/egress denial and restoration | Passed | Passed |
| Runner HTTP/UDP egress and MTU boundary | Passed | Passed |
| Scale-up/down, reboot and upgrade traffic checks | Passed | Passed |
| Cluster deletion and credential cleanup | Passed | Passed |
| Required assertion coverage | 25/25 distinct required assertions | 25/25 distinct required assertions |
| Required runtime skips in accepted evidence | None | None |
| Running-image provenance | Configuration and 11 ordered layer payloads match the locked manifest | Configuration and 11 ordered layer payloads match the locked manifest |

The original full runs selected 27 assertions on each architecture. Eighteen
required assertions outside the multi-CIDR scenario passed. The two shared
multi-CIDR-only assertions intentionally skipped in the ordinary single-CIDR
scenarios. In the dedicated multi-CIDR scenario, allocation from both ranges
passed, but probe creation failed with HTTP 403 because the test used the
restricted controller identity. That failure prevented its dependent cleanup
assertions from running.

The correction changes only that test's client selection to the existing
`GetAdminClient` fixture; production RBAC is unchanged. All seven assertions of
the dedicated multi-CIDR scenario were then rerun on fresh native guests and
passed on each architecture, including cleanup. The accepted evidence is
**18 unchanged assertions plus 7 corrected scenario assertions per architecture**,
not a claim that either original full run succeeded. Exact spec identities were
compared to exclude missing, duplicated or skipped required results. The original
failure reports and corrected result reports are retained in the qualification
evidence bundle.

The frozen qualification source changed only in
`controller/test/e2e/pod_cidr_test.go`: original snapshot SHA256
`85ceaefa9418665c26df89e9cc1d7dd31433412d74952975ffa8d54899ad66b1`,
corrected snapshot SHA256
`3499c185b2dee8124cf5ebba17fca875d7c6c0bc6f9a1d6569a5b12e61e74e53`.
The deployed patch, resulting source file, and running test executable were
hashed and recorded for each corrected run.

Image identities:

- amd64 node qcow2 SHA256: `9f0385ef82fd82f8ec3a36f24c36ece101183937db7570af9ed0798762b9cf9b`.
- arm64 node qcow2 SHA256: `73c6edd22faf4c06bc57ccbd9134dfa8ecab508a4cf0aae751e815298587731a`.
- amd64 locked OVN platform manifest: `sha256:8a7376d05735223734948b5b4f57af9a55bce76145ae8ac9b8f164b1b4fd87f2`.
- arm64 locked OVN platform manifest: `sha256:11b8b4dc5124a29053a7618a848050ce2ad7abb0d167454dfe2f193a8b38e124`.

Containerd normalizes the imported OCI manifest into Docker schema-2 media types,
so its target manifest digest differs from the lock. The manifest configuration,
ordered layer digests and sizes match; every installed archive layer payload was
independently hashed. Running OVN containers were tied to those runtime images.


## Networking assertions

The explicit OVN smoke scenario validates cross-node NetworkPolicy ingress and egress. It first proves reachability, then requires the same source pod to reach a control endpoint, time out against the denied target, and reach control again. A separate allowed source verifies target health. Removing the policy must restore connectivity.

The smoke scenario also requests a nonce from an HTTP endpoint on the Linux runner's bridge gateway. UDP probes from two distinct tenant nodes reach that gateway and the opposite node. The pinned OVN chart's MTU is 1400: each IPv4 probe forces path MTU discovery, requires a route MTU of 1400, echoes a 1372-byte UDP payload, and requires `EMSGSIZE` for 1373 bytes. This checks the effective IPv4 packet-size boundary; it does not claim IPv6, arbitrary internet reachability, or a specific SNAT source identity.

For local execution, set `NICO_VMS_BRIDGE_CIDR` to the actual runner bridge subnet. The CI entrypoint verifies that its network-plus-one gateway matches the bridge's sole global IPv4 address. When the egress test is selected, TCP 18082 and UDP 18081 must be unused. The entrypoint installs temporary INPUT rules restricted to that bridge, gateway, and those ports, and removes its own rules on exit even when `NKE_E2E_KEEP` preserves the lab. Running the Ginkgo binary directly requires arranging the same bridge access first. The endpoints never bind the physical host address.

Lifecycle traffic probes run after scale-up, scale-down, worker reboot, and each control-plane upgrade hop. Every current desired node participates in pod-IP, Service DNS, and ClusterIP checks. The newly added nodes, rebooted worker, and replacement control-plane node are explicitly required to participate in their respective checks.

Future candidates require passing live results on **both** architectures. Local unit tests and dry-run selection establish test behavior and coverage only. The dated evidence above applies to the recorded source, packages, images and fresh-cluster scope; it does not establish migration, downgrade or hardware-offload compatibility.

