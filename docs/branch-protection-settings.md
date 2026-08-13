# Branch protection settings

This document describes the branch protection rules that a repository admin
must configure in **Settings → Branches → Branch protection rules** (or
**Settings → Rules → Rulesets** for the newer ruleset UI).

These settings are not enforced automatically by files in the repository.
An admin must apply them once after cloning or during initial repo setup.

## Rule: protect `main`

| Setting | Value | Reason |
|---|---|---|
| Require a pull request before merging | ✅ enabled | No direct pushes to `main` |
| Required approvals | 1 | At least one non-author review |
| Dismiss stale pull request approvals when new commits are pushed | ✅ enabled | Re-review after force-push |
| Require review from Code Owners | ✅ enabled | Enforces `.github/CODEOWNERS` |
| Require status checks to pass before merging | ✅ enabled | CI must be green |
| Require branches to be up to date before merging | ✅ enabled | No hidden conflicts |
| Restrict who can push to matching branches | ✅ enabled (nke-maintainer team) | Prevents accidental force-push |
| Allow force pushes | ❌ disabled | Preserve linear history |
| Allow deletions | ❌ disabled | `main` cannot be deleted |

## Recommended status checks

A required check is named after the **job**, not the workflow. These are the
checks the current workflows report:

| Check | Workflow | Safe to require? |
|---|---|---|
| `golangci-lint` | `.github/workflows/lint.yml` | yes |
| `golangci-lint (controller)` | `.github/workflows/lint.yml` | yes |
| `golangci-lint (forge-vms)` | `.github/workflows/lint.yml` | yes |
| `Check THIRD_PARTY_NOTICES.md is up to date` | `.github/workflows/notices-check.yml` | yes |
| `build` | `.github/workflows/image.yml` | yes |
| `DCO` | the DCO GitHub App, not a workflow | yes |
| `Build binaries` | `.github/workflows/go-build.yml` | **no — path-filtered** |
| `Check go.work and go.work.sum are current` | `.github/workflows/go-work-check.yml` | **no — path-filtered** |

⚠️ **Do not mark the last two required.** Both filter on `paths:`, so a
documentation-only pull request never reports them — which is exactly the trap
described next. Require them only if the filters go, or a skip shim reports the
same check name.

Confirm the exact names before you require one. A required check that names a
job which never reports leaves every pull request waiting forever:

```bash
gh pr view <number> --repo NVIDIA/nke \
  --json statusCheckRollup --jq '.statusCheckRollup[] | .name // .context'
```

The command below requires the first two and `DCO`. It leaves `build` out on
purpose: an image build is the slowest job here, and blocking every
documentation change on it buys little. Add it if you want that guarantee.

## Setting up via GitHub CLI

```bash
gh api repos/NVIDIA/nke/branches/main/protection \
  --method PUT \
  --field required_status_checks='{"strict":true,"contexts":["golangci-lint","Check THIRD_PARTY_NOTICES.md is up to date","DCO"]}' \
  --field enforce_admins=true \
  --field required_pull_request_reviews='{"dismiss_stale_reviews":true,"require_code_owner_reviews":true,"required_approving_review_count":1}' \
  --field restrictions=null
```

> **Note:** The `restrictions` field controls push access. Set it to a
> specific team or users object if you want to lock down who can push
> even with admin privileges.
