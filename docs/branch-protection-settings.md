# Branch protection settings

`main` is protected by **repository rulesets**, not by the older branch
protection feature. That distinction is why this page was rewritten. The two are
separate systems with separate APIs, and the endpoint the previous version told
you to check cannot see a ruleset. It answered `"contexts": []` while `gate` and
`DCO` were both required, so the check confirmed the opposite of the truth and
this page concluded that nothing was enforced.

Read the live configuration this way:

```bash
gh api repos/NVIDIA/nke/rulesets --jq '.[] | {id, name, target, enforcement}'
gh api repos/NVIDIA/nke/rules/branches/main --jq '[.[].type]'
```

Do **not** use `gh api repos/NVIDIA/nke/branches/main/protection`. It reads the
legacy system, which this repository does not use, and it reports an unprotected
branch either way.

## What protects `main` today

Four rulesets apply, all with enforcement `active`:

| id | name | target |
|---|---|---|
| 20914269 | Protect main | branch |
| 21252202 | Conventional Commits | branch |
| 12413484 | Restrict visibility (no public, allow overrides) | repository |
| 12715293 | Restrict transfers | repository |

Together they place nine rules on `main`:

| rule | effect |
|---|---|
| `required_status_checks` | `gate` and `DCO` must pass. Strict mode is off, so a branch need not be current with `main` to merge. |
| `pull_request` | one approving review, stale approvals dismissed on push, and a review from a `CODEOWNERS` owner |
| `required_signatures` | every commit must be signed |
| `required_linear_history` | no merge commits on `main` |
| `non_fast_forward` | no force pushes |
| `deletion` | `main` cannot be deleted |
| `commit_message_pattern` | Conventional Commits, see below |
| `repository_visibility` | the repository cannot be made public without an override |
| `repository_transfer` | the repository cannot be transferred |

The "Protect main" ruleset has one bypass actor, a user with
`bypass_mode: always`.

## The two required checks

**`gate`** is the aggregate job in
[`merge-gate.yml`](../.github/workflows/merge-gate.yml). It is the only build
check branch protection knows about, by design: individual jobs come and go, and
a required context that stops reporting blocks every pull request until an admin
removes it. `gate` depends on thirteen member workflows and reports one result
for all of them.

Adding or removing a member is therefore an edit to `merge-gate.yml`, not to this
ruleset. [The workflow guide](../.github/workflows/README.md) covers how, and why
a member must also be added to `gate.needs`. Missing that second edit is
fail-open: the work runs, but its failure cannot affect the result.

**`DCO`** is a GitHub App that checks every commit carries a `Signed-off-by`
line. `CONTRIBUTING.md` explains the sign-off.

Everything else that runs on a pull request is advisory. An advisory check going
red does not block a merge, and its absence is not a pass. When reviewing,
confirm the checks you expected for that author and path set actually ran.

## Commit messages

Ruleset 21252202 enforces this pattern on every commit reaching `main`:

```
^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(\([a-z0-9/_.-]+\))?!?: .+
```

The type must be one of those eleven, an optional scope must be lowercase, and
`!` marks a breaking change. `feat(devx): add a thing` passes, while
`Feat: add a thing` and `feat(DevX): add a thing` do not.

This matters more than style, because the repository **squash-merges only**:
`allow_merge_commit` and `allow_rebase_merge` are both false. The pull request
title becomes the single commit subject on `main`, so the title is what the
pattern applies to and what `git-cliff` turns into the changelog entry.

## Changing the rules

Rulesets are edited in **Settings → Rules → Rulesets**, or through the API:

```bash
gh api repos/NVIDIA/nke/rulesets/20914269 > ruleset.json   # read
# edit ruleset.json, then
gh api -X PUT repos/NVIDIA/nke/rulesets/20914269 --input ruleset.json
```

Two cautions. Adding a context to `required_status_checks` blocks every pull
request until a job reports under exactly that name, so add the workflow first
and confirm it reports before requiring it. And a check that runs only for some
authors, such as one gated on a vetted mirror branch, can never be required: the
context never appears for everyone else, and their pull requests wait forever.

## Verifying

```bash
gh api repos/NVIDIA/nke/rules/branches/main --jq '[.[].type]'
gh api repos/NVIDIA/nke/rules/branches/main \
  --jq '.[] | select(.type=="required_status_checks")
        | [.parameters.required_status_checks[].context]'
gh pr checks <number> --repo NVIDIA/nke --json name,workflow,state,bucket
```

For a pull request, confirm the members you expect appear inside `Merge gate`,
that no standalone copy of a member runs alongside it, and that `gate` itself
reports. If this page and the API disagree, the API is right and this page needs
an edit.
