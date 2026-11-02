#!/bin/sh
# block.sh NAME: prints the shell block that DEPLOY.md marks with
# <!-- ci: NAME ... -->, unindented, so CI runs the commands exactly as the
# guide gives them. Fails if there is no such block.
set -e
here=$(cd "$(dirname "$0")" && pwd)
out=$(awk -v m="<!-- ci: $1 " '
  index($0, m) { found = 1; next }
  found == 1 && /^[ \t]*```sh[ \t]*$/ { match($0, /^[ \t]*/); ind = RLENGTH; found = 2; next }
  found == 2 && /^[ \t]*```[ \t]*$/ { exit }
  found == 2 { print substr($0, ind + 1) }
' "$here/../../DEPLOY.md")
[ -n "$out" ] || { echo "block.sh: no block marked $1 in DEPLOY.md" >&2; exit 1; }
printf '%s\n' "$out"
