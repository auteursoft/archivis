#!/bin/sh
# Check that everything Archivis depends on is still where it was: every
# original download (tools/deps/assets.txt, tools/samples/fetch.sh), every
# repository in tools/deps/repos.txt, and every Go module straight from
# its origin (not the module proxy); and, once the forks exist, that the
# module fallback builds (tools/deps/use-forks.sh). A loss fails, naming what
# went, while the mirror and the forks still hold it. A fork not yet made in
# auteursoft is a warning (run tools/deps/fork.sh). Run by deps.yml.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
org=${ARCHIVIS_FORK_ORG:-auteursoft}
failed=0
urls=$(
	grep -v '^#' "$here/assets.txt" | awk 'NF == 3 { print $3 }'
	"$here/../samples/fetch.sh" --list | cut -d' ' -f2
)
for u in $urls; do
	# one byte, following redirects: works where HEAD is refused
	if curl -fsSL --retry 2 -r 0-0 -o /dev/null "$u"; then
		echo "ok        $u"
	else
		echo "::error::gone or failing: $u"
		failed=1
	fi
done
while read -r repo _; do
	case $repo in '' | '#'*) continue ;; esac
	if gh api "repos/$repo" --jq .full_name </dev/null >/dev/null 2>&1; then
		echo "ok        $repo"
	else
		echo "::error::repository gone or private: $repo (fork: $org/${repo#*/})"
		failed=1
	fi
	gh api "repos/$org/${repo#*/}" --jq .full_name </dev/null >/dev/null 2>&1 ||
		echo "::warning::no fork $org/${repo#*/} yet (run tools/deps/fork.sh)"
done <"$here/repos.txt"
cd "$here/../.."
if GOPROXY=direct GOFLAGS=-mod=mod GOMODCACHE="$(mktemp -d)" go mod download all; then
	echo "ok        Go modules, from their origins"
else
	echo "::error::a Go module cannot be fetched from its origin (proxy.golang.org may still serve it; tools/deps/use-forks.sh switches to the forks)"
	failed=1
fi
# the fallback itself: build from the forks, straight from them (no proxy),
# once every Go-module fork exists
forks=yes
while read -r repo module _; do
	case $repo in '' | '#'*) continue ;; esac
	[ "$module" = - ] && continue
	gh api "repos/$org/${repo#*/}" --jq .full_name </dev/null >/dev/null 2>&1 || forks=no
done <"$here/repos.txt"
if [ $forks = yes ]; then
	saved=$(mktemp -d) && cp go.mod go.sum "$saved"
	if "$here/use-forks.sh" >/dev/null && GOPROXY=direct GONOSUMDB="github.com/$org/*" GOFLAGS=-mod=mod GOMODCACHE="$(mktemp -d)" go build ./...; then
		echo "ok        builds from the $org forks (tools/deps/use-forks.sh)"
	else
		echo "::error::building from the $org forks failed: the Go module fallback does not work"
		failed=1
	fi
	cp "$saved/go.mod" "$saved/go.sum" .
else
	echo "::warning::not every Go module has a fork in $org yet, so building from the forks was not tried (run tools/deps/fork.sh)"
fi
exit $failed
