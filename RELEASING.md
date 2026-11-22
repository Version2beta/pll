# Releasing pll

The Go module is `github.com/version2beta/pll`; the repository is
[github.com/Version2beta/pll](https://github.com/Version2beta/pll). Its package
name is `pll`: Go does not require reserving a global bare registry name.
Python and Elixir publication are separate workflows, not part of this one.

## Prepare and review

1. Start from a clean worktree. Compare local `main` with `git ls-remote origin refs/heads/main`; reconcile differences deliberately, without discarding local work.
2. Check public tags and GitHub releases (`git ls-remote --tags origin` and `gh release list`). Choose an unused version based on all changes since the last public release. While the API may still change, use `v0.x.y`; use `v1.0.0` only when committing to a stable API. Never reuse or move a tag, including a retracted version.
3. On a preparation branch, update `CHANGELOG.md` with the complete scope and intended release date, and the README's version and install command. Review README, SPEC, exported API comments, examples, testing instructions and comparison sources against the code. Preserve existing retractions in `go.mod`.
4. Inspect `Makefile` and CI, then run `make verify && make fuzz` (each fuzz target runs for 30 seconds). Run `make sim`, the public examples and a separate consumer build. The module uses only the standard library; a local consumer can use a `replace` directive pointing at the candidate checkout with `GOPROXY=off GOTOOLCHAIN=local`.
5. Commit the narrow preparation changes. Record the full candidate SHA, release boundary, version, validation results and any blocked checks for accountable review. **Stop here until publication is approved.** Local testing is not evidence of CI passing on a later merge commit.

## Publish after approval

Push the preparation branch and use a reviewed pull request to merge it into
`main`. Then fetch without rewriting local work, ensure the release date and
scope still apply, and freeze the exact resulting commit. Repeat validation
if its content differs from the locally reviewed candidate.

Confirm CI passed on that exact commit, not merely the latest run on a branch:

```sh
v=v0.4.0
candidate=$(git rev-parse HEAD) # reviewed main commit, not an unmerged branch
remote_main=$(git ls-remote origin refs/heads/main | cut -f1)
test "$candidate" = "$remote_main" || exit 1
gh run list --commit "$candidate" --json headSha,status,conclusion,url
```

Inspect the results: all required CI jobs must have completed successfully.
Recheck that the tag is absent both locally and remotely; a network failure is
not proof of absence. Only after the exact-commit gate is satisfied:

```sh
git tag -a "$v" "$candidate" -m "pll $v"
git push origin "refs/tags/$v"
gh release create "$v" --verify-tag --title "pll $v" --notes-file <(awk -v v="$v" '$1 == "##" {f = ($2 == v); next} f' CHANGELOG.md)
```

The shell examples use bash. The tag triggers CI as well; check its run.
A successful tag push makes the version public, even if GitHub release
creation fails. Do not retag to repair it; rerun only the failed publication
step against the same tag.

## Check publication

```sh
GOPROXY=https://proxy.golang.org go list -m -json github.com/version2beta/pll@$v
tmp=$(mktemp -d)
(cd "$tmp" && go mod init example.com/check && go get github.com/version2beta/pll@$v && printf 'package main\nimport "github.com/version2beta/pll"\nfunc main() { _, _ = pll.UnitsPerSecond(48000, 1) }\n' > main.go && go build .)
```

Confirm the proxy's version and commit match the release. This published
consumer check cannot be completed before the tag is public. Then open
[pkg.go.dev/github.com/version2beta/pll](https://pkg.go.dev/github.com/version2beta/pll)
and confirm the version appears; request indexing from that page if needed.

Published tags are immutable release identities: if a release has a problem,
publish a new version and, if necessary, retract the affected version in
`go.mod`. Retraction does not erase cached copies or prevent explicit use.
Do not delete or move a tag.

For v2 and later, Go requires the major version in the module path
(`github.com/version2beta/pll/v2`) and in every import.
