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
| Restrict who can push to matching branches | ✅ enabled (nke-developers team) | Prevents accidental force-push |
| Allow force pushes | ❌ disabled | Preserve linear history |
| Allow deletions | ❌ disabled | `main` cannot be deleted |

## Recommended status checks

Add these required checks once the corresponding workflows are merged:

- `controller / build` (from `.github/workflows/ci.yml`)
- `node-agent / build` (from `.github/workflows/ci.yml`)
- `license / check` (from `.github/workflows/license.yml`)

## Setting up via GitHub CLI

```bash
gh api repos/NVIDIA/nke/branches/main/protection \
  --method PUT \
  --field required_status_checks='{"strict":true,"contexts":["controller / build","node-agent / build"]}' \
  --field enforce_admins=true \
  --field required_pull_request_reviews='{"dismiss_stale_reviews":true,"require_code_owner_reviews":true,"required_approving_review_count":1}' \
  --field restrictions=null
```

> **Note:** The `restrictions` field controls push access. Set it to a
> specific team or users object if you want to lock down who can push
> even with admin privileges.
