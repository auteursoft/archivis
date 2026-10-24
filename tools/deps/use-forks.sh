#!/bin/sh
# Build from auteursoft's forks instead of the original repositories, for
# when one disappears. The Go module proxy (proxy.golang.org) keeps every
# published version even then, so this is the second line: it adds a
# replace directive per module, at the version go.mod already pins.
#
#   tools/deps/use-forks.sh                 every module in repos.txt
#   tools/deps/use-forks.sh golang.org/x/image github.com/mattn/go-sqlite3
#   tools/deps/use-forks.sh --undo          drop them again
#
# The forks keep their original module path, as replace requires.
set -eu
org=${ARCHIVIS_FORK_ORG:-auteursoft}
here=$(cd "$(dirname "$0")" && pwd) # absolute: the cd below must not move it
cd "$here/../.."
undo=false
[ "${1:-}" = --undo ] && { undo=true; shift; }
while read -r repo module _; do
	case $repo in '' | '#'*) continue ;; esac
	[ "$module" = - ] && continue
	if [ $# -gt 0 ]; then
		want=false
		for m in "$@"; do [ "$m" = "$module" ] && want=true; done
		$want || continue
	fi
	if $undo; then
		go mod edit -dropreplace="$module"
		echo "original  $module"
		continue
	fi
	v=$(go list -m -f '{{.Version}}' "$module")
	go mod edit -replace="$module=github.com/$org/${repo#*/}@$v"
	echo "fork      $module -> github.com/$org/${repo#*/}@$v"
done <"$here/repos.txt"
echo "go.mod updated; run: go mod download && go build ./..."
