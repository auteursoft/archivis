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
while read -r sum name url; do
	got=$(awk -v n="$name" '$1 == n { print $2 }' "$work/have")
	if [ "$got" = "sha256:$sum" ]; then
		echo "ok        $name"
		continue
	fi
	if [ -n "$got" ] && [ "${got#sha256:}" != "$got" ]; then
		echo "::warning::$name in the mirror has SHA-256 ${got#sha256:}, not $sum: replacing it"
		gh release delete-asset "$tag" "$name" -R "$repo" --yes
	elif [ -n "$got" ] || grep -q "^$name \$" "$work/have"; then
		echo "ok        $name (no digest recorded)"
		continue
	fi
	if ! curl -fsSL --retry 3 -o "$work/$name" "$url"; then
		echo "::error::$name: the original ($url) cannot be downloaded, and the mirror has no copy"
		failed=1
		continue
	fi
	if [ "$(sha "$work/$name")" != "$sum" ]; then
		echo "::error::$name: the original ($url) no longer matches its pinned SHA-256"
		failed=1
	else
		gh release upload "$tag" "$work/$name" -R "$repo" --clobber
		echo "mirrored  $name"
	fi
	rm -f "$work/$name"
done <"$work/entries"
exit $failed
