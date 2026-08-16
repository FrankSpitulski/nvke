# Third-Party Notices

nke is a monorepo of independently built and independently released components.
Each one links a different set of third-party Go modules, so the notices are
generated **per component** rather than merged into a single file. Merging them
would attach every module's attribution to every artifact, which misstates what
each artifact actually contains.

This file is the index. It records which components have generated notices,
where those notices are, and which do not yet — and why.

## Covered components

| Component | Notices | Generated from | Enforced by |
|---|---|---|---|
| `agent/` — the node agent | [agent/THIRD_PARTY_NOTICES.md](agent/THIRD_PARTY_NOTICES.md) | the import closure of `./cmd/...` for `linux/amd64` and `linux/arm64` | the `Third-Party Notices` workflow, on every push |

Regenerate with:

```bash
cd agent && make notices
```

`make notices-check` regenerates the file and fails if the result differs from
what is committed. CI runs it on every push, so the file cannot drift away from
the dependency tree without the build going red.

## Not yet covered

| Component | Why | Tracked by |
|---|---|---|
| `controller/` — the Cloud Workflow Engine | ⚠️ **A `controller/THIRD_PARTY_NOTICES.md` now exists and is NOT verified.** It was imported wholesale rather than generated from this tree, and `notices-check.yml` still runs `working-directory: agent`, so nothing regenerates or compares it. It attributes at least two modules that appear in no `go.sum` here and are not vendored. `controller/Dockerfile` copies it into `/licenses`, so it ships. | Regenerating it here and widening `notices-check.yml` |
| `controller/nkectl/` — the `nkectl` CLI | No notices file at all. It is a shipped binary, so its dependencies — cobra, viper, mapstructure, yaml.v3 — are attributed nowhere. Every dependency is public, so nothing blocks generating it. | Generating notices for `controller/nkectl/` |

⚠️ **The `controller/` row changed meaning on 2026-08-16 and this file has not
caught up in spirit.** It used to mean *"no notices file exists"*. It now means
*"one exists, ships inside the published image, and nobody has checked it"* —
which is the worse of the two, because a wrong attribution list looks compliant.

**A partial file would be worse than none here.** A notices file that silently
omits dependencies, or claims ones the artifact does not contain, asserts a
completeness it does not have, and anyone relying on it for license compliance
would be relying on the wrong list. The rows above stay open until each closure
can be walked end to end **and a check enforces it**.

When `controller/` builds from public dependencies, cover it by copying
`agent/hack/generate-notices.sh`, `agent/Makefile`'s `notices` and
`notices-check` targets, and adding the module to
`.github/workflows/notices-check.yml`. The generator is repository-agnostic and
is configured entirely through the `PACKAGES`, `PLATFORMS`, `OUTPUT` and
`GO_LICENSES` environment variables.

## Scope of a generated file

Each generated file lists every Go module reachable from that component's
released commands, on every platform it ships for, with the full text of each
license. It excludes:

- The Go standard library.
- Packages belonging to the module itself.
- Test-only and build-time dependencies that the released commands do not
  reach, because they are not distributed.

## See also

- [NOTICE](NOTICE) — the attribution notice for this repository, as required by
  the Apache License, Version 2.0
- [LICENSE](LICENSE) — the license this repository is distributed under
