#!/usr/bin/env bash
# Measure server start-up time, memory and per-page latency on a catalogue.
#   latency.sh ARCHIVIS_BINARY DATA_DIR PORT
# Exits non-zero if any sampled request is not HTTP 2xx: a fast error page
# is not a latency measurement.
set -u
BIN=$1 DATA=$2 PORT=$3
BASE=http://127.0.0.1:$PORT
LOG=$(mktemp)
# portable sub-second clock (GNU date's %N doesn't exist on macOS)
now() { python3 -c 'import time; print(f"{time.time():.3f}")'; }
start=$(now)
"$BIN" serve --data "$DATA" --addr 127.0.0.1:$PORT >"$LOG" 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null' EXIT
# Every curl is bounded: a server that accepts connections but never
# answers must reach a failure path, not hang the script.
PROBE="--connect-timeout 5 --max-time 10"
# ready = /guide answers 2xx (-f: an error page is not "ready")
deadline=$(( $(date +%s) + 300 ))
until curl -sf $PROBE -o /dev/null "$BASE/guide"; do
  kill -0 $PID 2>/dev/null || { echo "server died:"; cat "$LOG"; exit 1; }
  if [ "$(date +%s)" -ge $deadline ]; then
    echo "server not ready after 300 s (last status: $(curl -s $PROBE -o /dev/null -w '%{http_code}' "$BASE/guide"))"; cat "$LOG"; exit 1
  fi
  sleep 0.5
done
ready=$(now)
printf "start-up (models + loading vectors): %.1fs\n" "$(python3 -c "print($ready - $start)")"
printf "resident memory after start-up: %d MB\n" $(( $(ps -o rss= -p $PID) / 1024 ))
# an id from the catalogue, or empty if it has none (that page is skipped)
pick() { python3 -c "import sqlite3,sys; r=sqlite3.connect(sys.argv[1]).execute(sys.argv[2]).fetchone(); print(r[0] if r else '')" "$DATA/archivis.db" "$1"; }
pid1=$(pick "select id from photos order by id limit 1 offset (select min(500, count(*)/2) from photos)")
face1=$(pick "select id from faces order by id limit 1 offset (select min(500, count(*)/2) from faces)")
person1=$(pick "select id from people limit 1")
urls=(
  "/" "/?sort=newest" "/?sort=worst&page=50" "/?min_focus=0.7&faces=group&max_cast=6"
  "/?tag=protest" "/?q=protest+at+night+in+the+rain" "/?q=children+playing&min_focus=0.7"
  "/people" "/discover" "/bursts" "/duplicates"
)
[ -n "$pid1" ] && urls+=("/photo/$pid1") || echo "  (no photos: skipping /photo)"
[ -n "$face1" ] && urls+=("/face/$face1") || echo "  (no faces: skipping /face)"
[ -n "$person1" ] && urls+=("/person/$person1") || echo "  (no named people: skipping /person)"
failed=0
for u in "${urls[@]}"; do
  ts=(); bad=""
  for i in 1 2 3 4 5; do
    # a page slower than 120 s counts as failed (curl reports status 000)
    t=$(curl -s --connect-timeout 5 --max-time 120 -o /dev/null -w "%{time_total} %{http_code}" "$BASE$u")
    ts+=("${t% *}"); code=${t#* }
    case $code in 2??) ;; *) bad="$bad $code" ;; esac
  done
  med=$(printf "%s\n" "${ts[@]}" | sort -n | sed -n 3p)
  if [ -n "$bad" ]; then
    printf "  %-48s FAILED (HTTP%s)\n" "$u" "$bad"
    failed=$((failed + 1))
  else
    printf "  %-48s median %6.0f ms\n" "$u" "$(python3 -c "print($med * 1000)")"
  fi
done
printf "resident memory after queries: %d MB\n" $(( $(ps -o rss= -p $PID) / 1024 ))
if [ $failed -gt 0 ]; then
  echo "$failed page(s) returned errors; see $LOG" >&2
  exit 1
fi
