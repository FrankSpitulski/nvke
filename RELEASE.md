# Release process

This document describes how nke releases are created and published.

## Versioning

nke uses [Semantic Versioning](https://semver.org/):

```
v<MAJOR>.<MINOR>.<PATCH>
```

| Increment | When |
|---|---|
| `PATCH` | Backwards-compatible bug fixes |
| `MINOR` | New backwards-compatible features |
| `MAJOR` | Breaking API or behavioral changes |

## Release cadence

There is no fixed release schedule. Releases are made when a meaningful
set of features or fixes has accumulated on `main`, or when a security
patch requires an immediate out-of-band release.

## Creating a release

### 1. Update CHANGELOG.md

Move entries from `[Unreleased]` to a new versioned section:

```markdown
## [v0.2.0] — 2026-08-07

### Added
...
### Fixed
...
```

Commit the changelog update:

```bash
git commit -s -m "chore: update CHANGELOG.md for v0.2.0"
```

### 2. Tag the release

```bash
git tag -s v0.2.0 -m "Release v0.2.0"
git push upstream v0.2.0
```

### 3. Create a GitHub release

```bash
gh release create v0.2.0 \
  --title "v0.2.0" \
  --notes-file <(sed -n '/^## \[v0.2.0\]/,/^## \[/{ /^## \[v0.2.0\]/d; /^## \[/d; p }' CHANGELOG.md)
```

Or use the GitHub UI: **Releases → Draft a new release → Choose tag → Generate release notes**.

### 4. Verify artifacts

After the tag push, confirm that any CI release pipelines complete
successfully (image build, chart publish, etc.).

## Hotfix releases

For security patches that cannot wait for the next regular release:

1. Branch from the tag that needs patching: `git checkout -b fix/v0.1.1 v0.1.0`
2. Cherry-pick or apply the fix.
3. Follow steps 1–4 above with the patch version incremented.
4. Backport to `main` if the fix applies there too.

## Pre-releases

For release candidates, append `-rc.N` to the version: `v0.2.0-rc.1`.
Tag and publish the same way; GitHub marks pre-release tags automatically.
