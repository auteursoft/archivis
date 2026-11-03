#!/bin/bash
# provider.sh FLAG VALUE: after DEPLOY.md's switching block (run by CI) on
# the agents, each plist holds FLAG (provider, clip-provider, face-provider)
# exactly once, as --FLAG VALUE after its command, or not at all for an
# empty VALUE; and the running serve was started that way.
set -u
flag=$1 want=$2
fail() { echo "::error::--$flag '$want': $*"; exit 1; }
for j in serve index; do
  p=~/Library/LaunchAgents/com.archivis.$j.plist
  args=$(plutil -extract ProgramArguments json -o - "$p") || fail "cannot read $p"
  at=$([ $j = serve ] && echo 2 || echo 4)
  got=$(python3 -c '
import json, sys
a, flag, at = json.loads(sys.argv[1]), sys.argv[2], int(sys.argv[3])
idx = [i for i, x in enumerate(a) if x.lstrip("-").split("=")[0] == flag]
print(len(idx), *(a[i] + " " + (a[i + 1] if i + 1 < len(a) else "") for i in idx if i >= at))
' "$args" "$flag" "$at")
  if [ -n "$want" ]; then
    [ "$got" = "1 --$flag $want" ] || fail "$j: $(echo "$args" | tr -d '\n'), want one --$flag $want after the command"
  else
    [ "$got" = 0 ] || fail "$j: $(echo "$args" | tr -d '\n'), want no --$flag"
  fi
done
# the restarted serve runs with it (or without it)
pid=
for _ in $(seq 90); do
  pid=$(pgrep -f '/usr/local/bin/archivis serve' | head -1)
  if [ -n "$pid" ]; then
    a="$(ps -o args= -p "$pid") "
    if [ -n "$want" ]; then case "$a" in *" --$flag $want "*) break ;; esac
    else case "$a" in *" --$flag "*) ;; *) break ;; esac; fi
  fi
  sleep 2
done
a="$(ps -o args= -p "$pid") "
if [ -n "$want" ]; then
  case "$a" in *" --$flag $want "*) ;; *) fail "serve runs as: $a" ;; esac
else
  case "$a" in *" --$flag "*) fail "serve runs as: $a" ;; esac
fi
echo "--$flag '$want': ok ($a)"
