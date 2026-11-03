#!/bin/bash
# provider.sh VALUE: after DEPLOY.md's switching block (run by CI) on the
# agents, each plist holds exactly one provider flag, --provider VALUE, right
# after its command, and the running serve was started with it.
set -u
want=$1
fail() { echo "::error::provider $want: $*"; exit 1; }
for j in serve index; do
  p=~/Library/LaunchAgents/com.archivis.$j.plist
  args=$(plutil -extract ProgramArguments json -o - "$p") || fail "cannot read $p"
  at=$([ $j = serve ] && echo 2 || echo 4)
  got=$(python3 -c '
import json, sys
a = json.loads(sys.argv[1]); at = int(sys.argv[2])
flags = [x for x in a if x.lstrip("-").split("=")[0] == "provider"]
print(len(flags), a[at] if len(a) > at else "", a[at + 1] if len(a) > at + 1 else "")
' "$args" "$at")
  [ "$got" = "1 --provider $want" ] || fail "$j: $(echo "$args" | tr -d '\n'), want one --provider $want at $at"
done
# the restarted serve runs with it
for _ in $(seq 90); do
  pid=$(pgrep -f '/usr/local/bin/archivis serve' | head -1)
  [ -n "$pid" ] && ps -o args= -p "$pid" | grep -q -- "--provider $want " && break
  sleep 2
done
ps -o args= -p "$pid" | grep -q -- "--provider $want " || fail "serve runs as: $(ps -o args= -p "$pid")"
echo "provider $want: ok ($(ps -o args= -p "$pid"))"
