#!/bin/sh
# Keep the "deps" release of auteursoft/archivis holding a copy of every
# file Archivis downloads: tools/deps/assets.txt (what `archivis setup`
# fetches) and tools/samples/fetch.sh --list (the camera test files). A
# file missing from the release, or there with the wrong SHA-256, is
# downloaded from its original, checked against its pinned SHA-256 and
# uploaded, under a name GitHub keeps as it is (fetch.sh --mirror-name).
# Files no longer listed are removed after a clean run. Run by
# .github/workflows/deps.yml (GH_TOKEN with contents: write); fails if any
# file could not be mirrored. The release is marked a pre-release so it
# never shows as the project's latest release.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
repo=${GITHUB_REPOSITORY:-auteursoft/archivis}
tag=deps
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
sha() {
	if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

# The list is built one step at a time, with no pipelines (whose failures
# set -e cannot see): an incomplete list must stop the script, never lead
# to files being pruned from the mirror.
awk '!/^#/ && NF == 3' "$here/assets.txt" >"$work/assets"
"$here/../samples/fetch.sh" --list >"$work/samples"
[ -s "$work/assets" ] && [ -s "$work/samples" ] || { echo "::error::the list of files to mirror came out empty"; exit 1; }
cp "$work/assets" "$work/entries"
while read -r sum url; do
	name=$("$here/../samples/fetch.sh" --mirror-name "${url##*/}")
	echo "$sum $name $url" >>"$work/entries"
done <"$work/samples"
want=$(($(wc -l <"$work/assets") + $(wc -l <"$work/samples")))
[ "$(wc -l <"$work/entries")" -eq "$want" ] || { echo "::error::the list of files to mirror is incomplete"; exit 1; }
# GitHub rewrites other characters in release file names: none may be used
bad=$(cut -d' ' -f2 "$work/entries" | grep '[^A-Za-z0-9._-]' || true)
[ -z "$bad" ] || { echo "::error::mirror names GitHub would rewrite (use A-Z a-z 0-9 . _ -): $bad"; exit 1; }
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
# After a clean run, remove files no longer listed (renamed or dropped);
# but never a quarter or more of the mirror at once, which would mean a
# list gone wrong rather than a few renames.
if [ $failed = 0 ]; then
	cut -d' ' -f2 "$work/entries" | sort >"$work/listed"
	cut -d' ' -f1 "$work/have" | sort >"$work/held"
	comm -23 "$work/held" "$work/listed" >"$work/stale"
	stale=$(wc -l <"$work/stale")
	held=$(wc -l <"$work/held")
	if [ "$stale" -gt 0 ] && [ $((stale * 4)) -ge "$held" ]; then
		echo "::error::$stale of the $held files in the mirror are no longer listed; not removing so many at once (remove them by hand if meant)"
		exit 1
	fi
	while read -r name; do
		echo "::notice::$name is no longer listed: removing it from the mirror"
		gh release delete-asset "$tag" "$name" -R "$repo" --yes </dev/null
	done <"$work/stale"
fi
exit $failed
