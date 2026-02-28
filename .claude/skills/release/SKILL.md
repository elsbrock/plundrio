---
name: release
description: Draft a new GitHub release with version bump
allowed-tools: Bash, Read, Edit, Glob, Grep, Task, WebFetch
---

# Release Workflow

Create a new release for plundrio. The user may provide context about what changed (e.g. "two community contributions", "bug fix release").

**Follow these steps IN ORDER. Do not skip any step.**

## Step 1: Determine baseline and scope

1. Run `git tag --sort=-v:refname | head -1` to find the latest **git tag** (this is the last released version)
2. Run `gh release list --limit 5` to check for any existing draft or pre-release entries — if a draft/pre-release already exists for the next version, update it instead of creating a new one
3. Read `flake.nix` to find the current `version = "X.Y.Z";` — if it's already bumped past the latest tag, use that version (don't bump again)
4. Determine the **commit range**: from the latest git tag to HEAD

**CRITICAL: Only include changes in the commit range.** If a previous release already covers some commits (e.g. v0.10.5 covers v0.10.4..v0.10.5), the new release notes must only cover commits AFTER that release. Check `gh release view <prev-version> --json body` to see what's already documented. The `Full Changelog` link must use the correct base version (the immediately preceding release, not an older one).

## Step 2: Gather changes

1. Run `git log <baseline>..HEAD --oneline` to see commits in scope
2. Run `gh pr list --state merged --limit 20` and cross-reference with the commits to identify merged PRs
3. For each relevant PR, run `gh pr view <number> --json title,body,author,number` to get details
4. Ignore version bump commits (`chore: next version`) and CI-only changes unless significant

## Step 3: Bump version (if needed)

Skip this step if `flake.nix` already has a version newer than the latest git tag.

1. Read `flake.nix` and find the current `version = "X.Y.Z";` line
2. Increment the patch version (e.g. 0.10.4 -> 0.10.5)
3. Edit flake.nix with the new version
4. Stage and commit: `git add flake.nix && git commit -m "chore: next version is vX.Y.Z"`

## Step 4: Push

1. **IMPORTANT: Push to remote!** `git push origin main` (pull --rebase first if rejected)

## Step 5: Create or update draft release

If a draft/pre-release already exists for this version (from step 1), use `gh release edit`. Otherwise use `gh release create --draft`.

Format the release notes like this:

```
gh release create vX.Y.Z --draft --title "vX.Y.Z" --notes "$(cat <<'EOF'
## What's Changed

### Features
* <description> (closes #<number>)

### Bug Fixes
* <description> by @<author> in #<number>

### Improvements
* <description> by @<author> in #<number>

## New Contributors
* @<user> made their first contribution in #<number>

**Full Changelog**: https://github.com/elsbrock/plundrio/compare/vPREV...vNEW
EOF
)"
```

Categorize changes into sections as appropriate:
- **Features** for new functionality
- **Bug Fixes** for fixes
- **Improvements** for enhancements and refactoring
- **Breaking Changes** if any

Only include sections that have entries. Only include "New Contributors" if there are first-time contributors. Check with `gh api repos/elsbrock/plundrio/contributors` if unsure.

## Step 6: Report back

Print the release URL and a summary of what's included.

## Argument: $ARGUMENTS

Use this as context for what changed in this release (may be empty).
