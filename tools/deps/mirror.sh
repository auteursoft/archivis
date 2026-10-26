#!/bin/sh
# Keep the "deps" release of auteursoft/archivis holding a copy of every
# file Archivis downloads: tools/deps/assets.txt (what `archivis setup`
# fetches) and tools/samples/fetch.sh --list (the camera test files). A
# file missing from the release, or there with the wrong SHA-256, is
# downloaded from its original, checked against its pinned SHA-256 and
# uploaded. Run by .github/workflows/deps.yml (GH_TOKEN with contents:
# write); fails if any file could not be mirrored. The release is marked a
# pre-release so it never shows as the project's latest release.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
repo=${GITHUB_REPOSITORY:-auteursoft/archivis}
tag=deps
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
sha() {
	if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

{
	grep -v '^#' "$here/assets.txt" | awk 'NF == 3'
	"$here/../samples/fetch.sh" --list | while read -r sum url; do echo "$sum ${url##*/} $url"; done
} >"$work/entries"
dups=$(cut -d' ' -f2 "$work/entries" | sort | uniq -d)
[ -z "$dups" ] || { echo "::error::two files would share a mirror name: $dups"; exit 1; }

if gh release view "$tag" -R "$repo" >/dev/null 2>&1; then
	gh release edit "$tag" -R "$repo" --notes-file "$here/NOTICE.md" --prerelease >/dev/null
else
	gh release create "$tag" -R "$repo" --title "Dependency mirror" --notes-file "$here/NOTICE.md" --prerelease --latest=false
fi
# name and recorded digest ("sha256:...") of each file already there
gh api "repos/$repo/releases/tags/$tag" --jq '.assets[] | .name + " " + (.digest // "")' >"$work/have"

failed=0
# gh reads from /dev/null: the loop's input is the list of files
while read -r sum name url; do
	there=$(awk -v n="$name" '$1 == n { print "yes" }' "$work/have")
	got=$(awk -v n="$name" '$1 == n { print $2 }' "$work/have")
	if [ "$got" = "sha256:$sum" ]; then
		echo "ok        $name"
		continue
	fi
	if [ -n "$there" ] && [ -z "$got" ]; then
		# no digest recorded: check the mirrored copy itself
		if curl -fsSL --retry 3 -o "$work/$name" "https://github.com/$repo/releases/download/$tag/$name" &&
			[ "$(sha "$work/$name")" = "$sum" ]; then
			echo "ok        $name (checked: no digest recorded)"
			rm -f "$work/$name"
			continue
		fi
		got="unreadable or different"
	fi
	[ -n "$there" ] && echo "::warning::$name in the mirror is $got, not sha256:$sum"
	# the replacement is fetched and checked before anything in the mirror
	# changes: if the original is gone too, the copy there stays
	if ! curl -fsSL --retry 3 -o "$work/$name" "$url"; then
		if [ -n "$there" ]; then
			echo "::error::$name: the original ($url) cannot be downloaded to replace it; the mirror keeps the copy it has"
		else
			echo "::error::$name: the original ($url) cannot be downloaded, and the mirror has no copy"
		fi
		failed=1
		continue
	fi
	if [ "$(sha "$work/$name")" != "$sum" ]; then
		echo "::error::$name: the original ($url) no longer matches its pinned SHA-256"
		failed=1
	else
		gh release upload "$tag" "$work/$name" -R "$repo" --clobber </dev/null # replaces in one step
		echo "mirrored  $name"
	fi
	rm -f "$work/$name"
done <"$work/entries"
exit $failed
