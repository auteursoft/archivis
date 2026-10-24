#!/bin/sh
# Fork every repository Archivis depends on (tools/deps/repos.txt) into
# the auteursoft organization, or bring existing forks up to date. Run with
# a GitHub CLI signed in as someone who may create repositories there:
#
#   tools/deps/fork.sh            fork what is missing, sync the rest
#   tools/deps/fork.sh --check    only report which forks exist
#
# Forks carry source, branches and tags, but not release files: those are
# mirrored separately (tools/deps/assets.txt). A sync moves the default
# branch only, so for Go modules the upstream tags the fork lacks are
# copied too (forks share their parent's objects): `go mod edit -replace`
# onto a fork needs the pinned version's tag there.
set -eu
org=${ARCHIVIS_FORK_ORG:-auteursoft}
here=$(cd "$(dirname "$0")" && pwd)
check=false
[ "${1:-}" = --check ] && check=true
status=0
tags() { gh api --paginate "repos/$1/git/matching-refs/tags" --jq '.[] | .ref + " " + .object.sha'; }
while read -r repo module _; do
	case $repo in '' | '#'*) continue ;; esac
	name=${repo#*/}
	if gh repo view "$org/$name" --json name >/dev/null 2>&1; then
		if $check; then
			echo "ok       $org/$name"
			continue
		fi
		echo "sync     $org/$name"
		gh repo sync "$org/$name" --source "$repo" >/dev/null || echo "  sync failed; the fork stays as it was" >&2
	elif $check; then
		echo "MISSING  $org/$name (fork of $repo)"
		status=1
		continue
	else
		echo "fork     $repo -> $org/$name"
		gh repo fork "$repo" --org "$org" --clone=false
		sleep 5 # the fork is created asynchronously
	fi
	if [ "$module" != - ]; then
		have=$(tags "$org/$name" 2>/dev/null | cut -d' ' -f1)
		tags "$repo" | while read -r ref sha; do
			echo "$have" | grep -qxF "$ref" && continue
			gh api "repos/$org/$name/git/refs" -f ref="$ref" -f sha="$sha" >/dev/null &&
				echo "  tag    ${ref#refs/tags/}" || echo "  could not copy ${ref#refs/tags/}" >&2
		done
	fi
done <"$here/repos.txt"
exit $status
