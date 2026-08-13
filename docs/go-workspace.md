# The Go workspace

This repository holds seven Go modules. `go.work` at the root ties together the
five that build from public dependencies, so one `make build` covers all of
them and one Go toolchain version applies across the tree.

`go.work` and `go.work.sum` are **committed**, and CI fails when they go stale.

## What is in the workspace

| Module | In `go.work` | Why |
|---|---|---|
| `agent` | yes | builds from public dependencies |
| `controller/api` | yes | builds from public dependencies |
| `controller/hack/capd-reboot-controller` | yes | builds from public dependencies |
| `controller/hack/nico-mock` | yes | builds from public dependencies |
| `controller/test/e2e` | yes | builds from public dependencies |
| `controller` | **no** | requires `gitlab-master.nvidia.com` |
| `controller/forge-vms` | **no** | requires `gitlab-master.nvidia.com` |

### Why the last two are excluded

A workspace resolves **one** build list across every member. Adding a module
that needs private code therefore makes *every* module in the workspace
unbuildable without credentials — including `agent`, which anyone can build
today. With `controller` added, building `agent` fails like this:

```
gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go@v0.0.14: unrecognized import
path "gitlab-master.nvidia.com/nvmetal/carbide-rest-ngc-sdk-go": 401 Unauthorized
```

The two also disagree with each other. `controller` replaces the carbide SDK at
`v0.0.14` and `controller/forge-vms` at `v0.0.9`, and a workspace holds one
replacement per module path:

```
go: conflicting replacements for gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go
	gitlab-master.nvidia.com/nvmetal/carbide-rest-ngc-sdk-go@v0.0.14
	gitlab-master.nvidia.com/nvmetal/carbide-rest-ngc-sdk-go@v0.0.9
```

Both modules keep their own `Makefile` and are built as before, from inside
their own directory. They join the workspace when they build from public
dependencies.

## Everyday use

```bash
make            # list the targets
make build      # build the released commands into bin/
make clean      # remove bin/
```

`make build` produces `bin/agent`, `bin/capd-reboot-controller` and
`bin/nico-mock`.

## Keeping the workspace up to date

`go.work.sum` records the checksums the workspace needs. It goes stale whenever
a module's requirements change — a dependency bump, a new import, a `go get`.
**Nothing updates it for you**, including Dependabot.

After any change to a `go.mod` or `go.sum`:

```bash
make work-sync
```

That does three things:

1. `go work sync` writes the workspace's selected version of each dependency
   back into every member's `go.mod`.
2. `go mod tidy` runs in each module with the workspace **off**, completing its
   `go.sum`. `go work sync` updates `go.mod` but not `go.sum`, which leaves a
   module unbuildable on its own — and these modules are built on their own, by
   their own Makefiles, by the image builds, and by every per-module CI job.
3. `go mod download all` rewrites `go.work.sum`.

**Commit `go.work`, `go.work.sum` and every `go.mod` and `go.sum` it touched, in
the same commit as the change that moved them.**

### Why `go.mod` changes too

A workspace builds every member against one unified version of each dependency,
whether or not that version is written down. `go work sync` writes it down.
Skipping it does not avoid the upgrade — it only hides it from `go.mod`, from
Dependabot, and from `THIRD_PARTY_NOTICES.md`, so the file says one thing and
the binary contains another.

To check before pushing exactly what CI will check:

```bash
make work-check
```

⚠️ `work-check` is not read-only: it performs the same refresh and then fails if
anything moved, so a failure leaves your tree edited. That edit *is* the fix —
review it and commit it.

## What CI enforces

The **Go Workspace Check** workflow runs `make work-check`, which asserts five
things. Each maps to a failure this repository can produce:

| Assertion | Failure it catches | Fix |
|---|---|---|
| `go.work` and `go.work.sum` are tracked by git | Either file left untracked. `git diff` reports nothing for an untracked path, so the rest of the check would pass in silence. | `make work-sync` and commit both |
| `go.work` lists exactly the expected modules | A module dropped from or added to the workspace by hand | `make work` and commit `go.work` |
| `go.work` carries no `replace` | `make work-full` output committed by accident. It repoints a dependency for every module while the module list still looks correct. | `make work`, or `git checkout -- go.work go.work.sum '*/go.mod' '*/go.sum'` |
| Every `go.mod` in the tree is classified in the `Makefile` | A **new module** added to neither `WORK_MODULES` nor `PRIVATE_MODULES`. It would never be built or synced, and nothing else in CI would mention it. | add it to the right list in the `Makefile`, then `make work-sync` |
| A full refresh changes nothing | The ordinary stale case: a bump landed in one module, so the others build against a version none of their `go.mod` files admits to | `make work-sync` and commit everything it changes |

The **Go Build** workflow runs `make build`. Both run on `ubuntu-latest` and
need no NVIDIA credentials, so they also run on fork and Dependabot pull
requests.

## Adding a module

1. Create it with `go mod init`.
2. Add its path to `WORK_MODULES` in the root `Makefile` — or to
   `PRIVATE_MODULES` if it needs `gitlab-master.nvidia.com`.
3. If it has commands to build, add it to `CMD_MODULES` too.
4. Run `make work-sync` and commit `go.work`, `go.work.sum` and the `Makefile`.

Skipping step 2 fails `make work-check` by design, with the module named.

## Working on `controller` inside NVIDIA

To put all seven modules in the workspace:

```bash
make work-full
```

This adds `controller` and `controller/forge-vms` and writes the workspace-level
`replace` the version conflict requires, pinning the carbide SDK to `v0.0.14`
for both. It needs `gitlab-master.nvidia.com` access, and it forces
`controller/forge-vms` off its own `v0.0.9`.

The result is **local only**. Undo it before committing — and restore all of it,
not just `go.work`. If you ran `make work-sync` while the private modules were
listed, it also rewrote `go.work.sum` and every `go.mod` and `go.sum`:

```bash
git checkout -- go.work go.work.sum '*/go.mod' '*/go.sum'
```

`make work` also resets `go.work` on its own: it drops any module not in
`WORK_MODULES` and any `replace`.

## Troubleshooting

**`error obtaining VCS status: exit status 128`** — you are building inside a
linked `git worktree`, where `.git` is a file rather than a directory, and Go
cannot stamp VCS metadata. It does not happen in a normal clone or in CI. Build
with stamping off:

```bash
GOFLAGS=-buildvcs=false make build
```

**`directory prefix . does not contain modules listed in go.work`**, or
**`directory ... is contained in a module that is not one of the workspace
modules`** — two different causes.

At the repository root, the root is not itself a module: build through the
`Makefile`, or name a module path (`go build ./agent/...`).

Inside `controller/` or `controller/forge-vms`, the cause is that those modules
are not workspace members, and `GOWORK` is on by default for every `go` command
under the root. Their Makefiles set `GOWORK=off`, so `make build`, `make test`
and `make lint` work as before. A `go` command typed directly needs it too:

```bash
cd controller
GOWORK=off go list ./...
```
