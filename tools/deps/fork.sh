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
status=0 # any fork missing (--check), or failing to sync or take a tag: 1
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# gh reads from /dev/null: the loop's input is repos.txt
tags() { gh api --paginate "repos/$1/git/matching-refs/tags" --jq '.[] | .ref + " " + .object.sha' </dev/null; }
while read -r repo module _; do
	case $repo in '' | '#'*) continue ;; esac
	name=${repo#*/}
	if gh repo view "$org/$name" --json name </dev/null >/dev/null 2>&1; then
		if $check; then
			echo "ok       $org/$name"
			continue
		fi
		echo "sync     $org/$name"
		if ! gh repo sync "$org/$name" --source "$repo" </dev/null >/dev/null; then
			echo "  sync failed: $org/$name is behind $repo" >&2
			status=1
		fi
	elif $check; then
		echo "MISSING  $org/$name (fork of $repo)"
		status=1
		continue
	else
		echo "fork     $repo -> $org/$name"
		gh repo fork "$repo" --org "$org" --clone=false </dev/null
		sleep 5 # the fork is created asynchronously
	fi
	if [ "$module" != - ]; then
		if ! tags "$repo" >"$work/upstream" || ! tags "$org/$name" >"$work/fork"; then
			echo "  could not list the tags of $repo or $org/$name" >&2
			status=1
			continue
		fi
		cut -d' ' -f1 "$work/fork" >"$work/have"
		while read -r ref sha; do
			grep -qxF "$ref" "$work/have" && continue
			if gh api "repos/$org/$name/git/refs" -f ref="$ref" -f sha="$sha" </dev/null >/dev/null; then
				echo "  tag    ${ref#refs/tags/}"
			else
				echo "  could not copy tag ${ref#refs/tags/} to $org/$name" >&2
				status=1
			fi
		done <"$work/upstream"
	fi
done <"$here/repos.txt"
[ $status = 0 ] || echo "some forks are missing or behind (see above); run again once fixed" >&2
exit $status
