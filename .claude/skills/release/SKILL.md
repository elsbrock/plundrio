---
name: release
description: Draft a new GitHub release with version bump
allowed-tools: Bash, Read, Edit, Glob, Grep, Task, WebFetch
---

# Release Workflow

Create a new release for plundrio. The user may provide context about what changed (e.g. "two community contributions", "bug fix release").

**Follow these steps IN ORDER. Do not skip any step.**

## Step 1: Determine current version and changes

1. Run `git tag --sort=-v:refname | head -1` to find the latest tag
2. Run `git log <latest-tag>..HEAD --oneline` to see unreleased commits
3. Run `gh pr list --state merged --limit 20` and cross-reference with the commits to identify merged PRs since the last release
4. For each relevant PR, run `gh pr view <number> --json title,body,author,number` to get details

## Step 2: Bump version

1. Read `flake.nix` and find the current `version = "X.Y.Z";` line
2. Increment the patch version (e.g. 0.10.4 -> 0.10.5)
3. Edit flake.nix with the new version

## Step 3: Commit and push

1. Stage and commit: `git add flake.nix && git commit -m "chore: next version is vX.Y.Z"`
2. **IMPORTANT: Push to remote!** `git push origin main` (pull --rebase first if rejected)

## Step 4: Draft the GitHub release

Use `gh release create` with `--draft` flag. Format the release notes like this:

```
gh release create vX.Y.Z --draft --title "vX.Y.Z" --notes "$(cat <<'EOF'
## What's Changed

### Bug Fixes
* <description> by @<author> in #<number>

### Improvements
* <description> by @<author> in #<number>

## New Contributors
* @<user> made their first contribution in #<number>

**Full Changelog**: https://github.com/elsbrock/plundrio/compare/vOLD...vNEW
EOF
)"
```

Categorize changes into sections as appropriate:
- **Bug Fixes** for fixes
- **Improvements** for enhancements
- **Breaking Changes** if any

Only include "New Contributors" if there are first-time contributors. Check with `gh api repos/elsbrock/plundrio/contributors` if unsure.

## Step 5: Report back

Print the release URL and a summary of what's included.

## Argument: $ARGUMENTS

Use this as context for what changed in this release (may be empty).
