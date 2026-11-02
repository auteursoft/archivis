#!/bin/bash
# check.sh agents|services: after DEPLOY.md's macOS steps (run by CI),
# checks that Archivis runs as that kind of launchd job and no other: serve
# and index loaded, one serve running as this user, refusing requests
# without the password and taking the one in its plist.
set -u
kind=$1
uid=$(id -u) user=$(id -un)
fail() { echo "::error::$kind: $*"; exit 1; }

if [ "$kind" = services ]; then
  plist=/Library/LaunchDaemons/com.archivis.serve.plist
  get() { sudo /usr/libexec/PlistBuddy -c "Print :$1" "$plist" 2>/dev/null; }
  for j in serve index; do
    sudo launchctl print system/com.archivis.$j >/dev/null 2>&1 || fail "system/com.archivis.$j is not loaded"
    launchctl print gui/$uid/com.archivis.$j >/dev/null 2>&1 && fail "the agent com.archivis.$j is still loaded"
    [ -e ~/Library/LaunchAgents/com.archivis.$j.plist ] && fail "the agent's plist is still in ~/Library/LaunchAgents"
    d=/Library/LaunchDaemons/com.archivis.$j.plist
    [ "$(stat -f '%Su:%Sg %Lp' "$d")" = "root:wheel 600" ] || fail "$d is $(stat -f '%Su:%Sg %Lp' "$d"), want root:wheel 600"
  done
  [ "$(get UserName)" = "$user" ] || fail "UserName is '$(get UserName)', want $user"
  [ "$(get EnvironmentVariables:HOME)" = "$HOME" ] || fail "HOME is '$(get EnvironmentVariables:HOME)', want $HOME"
  running() { sudo launchctl print system/com.archivis.serve | grep -q 'state = running'; }
else
  plist=~/Library/LaunchAgents/com.archivis.serve.plist
  get() { /usr/libexec/PlistBuddy -c "Print :$1" "$plist" 2>/dev/null; }
  for j in serve index; do
    launchctl print gui/$uid/com.archivis.$j >/dev/null 2>&1 || fail "the agent com.archivis.$j is not loaded"
    sudo launchctl print system/com.archivis.$j >/dev/null 2>&1 && fail "system/com.archivis.$j is still loaded"
    [ -e /Library/LaunchDaemons/com.archivis.$j.plist ] && fail "the system service's plist is still there"
  done
  get UserName >/dev/null && fail "the agent has a UserName"
  running() { launchctl print gui/$uid/com.archivis.serve | grep -q 'state = running'; }
fi
# just loaded, or relaunched by KeepAlive after an exit: give it a while
for _ in $(seq 90); do running && break; sleep 2; done
running || fail "serve is not running"

auth=$(get EnvironmentVariables:ARCHIVIS_AUTH) || fail "no ARCHIVIS_AUTH in $plist"
# serve loads the search indexes before it listens; a changed password
# takes effect once the restarted serve answers with it
for _ in $(seq 90); do
  without=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8088/)
  with=$(curl -s -o /dev/null -w '%{http_code}' -u "$auth" http://127.0.0.1:8088/)
  [ "$without" = 401 ] && [ "$with" = 200 ] && break
  sleep 2
done
[ "$without" = 401 ] || fail "without the password: $without, want 401"
[ "$with" = 200 ] || fail "with the plist's password: $with, want 200"

pids=$(pgrep -f '/usr/local/bin/archivis serve')
[ "$(echo "$pids" | wc -l | tr -d ' ')" = 1 ] || fail "serve processes: $(echo $pids)"
[ "$(ps -o user= -p "$pids" | tr -d ' ')" = "$user" ] || fail "serve runs as $(ps -o user= -p "$pids"), want $user"
echo "$kind: ok (serve pid $pids as $user, 401 without the password, 200 with it)"
