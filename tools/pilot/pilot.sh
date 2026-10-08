#!/usr/bin/env bash
# Archivis pilot: index a real folder of photos into a separate, throwaway
# catalogue, measure it, interrupt it the hard way and check that it
# recovers, and write a report to send back.
#
#   tools/pilot/pilot.sh PHOTOS_DIR [OLD_PHOTODEX_DATA_DIR]
#
# Your photos are only read. Everything is written under $PILOT
# (default ~/archivis-pilot); your real catalogue (~/.archivis) is not
# touched. Models are reused from ~/.archivis/models if they are there.
#
# Settings (environment variables):
#   PILOT=~/archivis-pilot   where the pilot catalogue and report go
#   CRASH_AFTER=600          seconds into indexing at which to kill it
#   WORKERS=                 parallel photos (default: all cores)
#   ARCHIVIS=archivis        the archivis binary to test
#
# The report (pilot-report.txt) contains file paths from PHOTOS_DIR, in the
# error list. Look through it before sending it on.
set -uo pipefail

photos=${1:?usage: pilot.sh PHOTOS_DIR [OLD_PHOTODEX_DATA_DIR]}
old=${2:-}
pilot=${PILOT:-$HOME/archivis-pilot}
crash_after=${CRASH_AFTER:-600}
bin=${ARCHIVIS:-archivis}
data=$pilot/data
report=$pilot/pilot-report.txt
workers=()
[ -n "${WORKERS:-}" ] && workers=(--workers "$WORKERS")

[ -d "$photos" ] || { echo "no such folder: $photos" >&2; exit 2; }
command -v "$bin" >/dev/null || { echo "archivis not found (set ARCHIVIS=/path/to/archivis)" >&2; exit 2; }
if [ -e "$data" ]; then
	echo "$data already exists; remove it or set PILOT to a new folder" >&2
	exit 2
fi
mkdir -p "$data"
: >"$report"
say() { printf '%s\n' "$*" | tee -a "$report"; }
now() { date +%s; }
section() { say ""; say "== $*"; }

# Peak memory of a command, where the system can tell us.
timed() { # timed LOGFILE CMD...
	local log=$1
	shift
	if [ "$(uname)" = Darwin ]; then
		/usr/bin/time -l "$@" 2>>"$log"
	elif /usr/bin/time --version >/dev/null 2>&1; then
		/usr/bin/time -v "$@" 2>>"$log"
	else
		"$@" 2>>"$log"
	fi
}
peak_mb() { # from a timed log
	local b
	b=$(grep -E 'maximum resident set size' "$1" | tail -1 | awk '{print $1}')
	if [ -n "$b" ]; then echo "$((b / 1048576)) MB"; return; fi
	b=$(grep -E 'Maximum resident set size' "$1" | tail -1 | awk '{print $NF}')
	[ -n "$b" ] && echo "$((b / 1024)) MB" || echo "unknown (on Linux: sudo apt install time)"
}

section "Machine"
say "date:     $(date)"
say "system:   $(uname -srm)"
if [ "$(uname)" = Darwin ]; then
	say "model:    $(sysctl -n hw.model) / $(sysctl -n machdep.cpu.brand_string 2>/dev/null)"
	say "cores:    $(sysctl -n hw.ncpu), memory $(($(sysctl -n hw.memsize) / 1073741824)) GB"
else
	say "cores:    $(nproc), memory $(awk '/MemTotal/ {print int($2/1048576)}' /proc/meminfo) GB"
fi
say "archivis: $("$bin" --help 2>/dev/null | head -1)"
say "photos:   $photos"
say "volume:   $(df -h "$photos" | tail -1)"

if [ -d "$HOME/.archivis/models" ]; then
	ln -s "$HOME/.archivis/models" "$data/models"
	[ -d "$HOME/.archivis/lib" ] && ln -s "$HOME/.archivis/lib" "$data/lib"
fi
"$bin" setup --data "$data" >>"$pilot/setup.log" 2>&1 || { say "setup failed; see $pilot/setup.log"; exit 1; }

section "Folder"
total=$(find "$photos" -type f \( -iname '*.jpg' -o -iname '*.jpeg' -o -iname '*.heic' -o -iname '*.png' -o -iname '*.tif' -o -iname '*.tiff' \
	-o -iname '*.cr2' -o -iname '*.cr3' -o -iname '*.nef' -o -iname '*.arw' -o -iname '*.dng' -o -iname '*.raf' -o -iname '*.orf' -o -iname '*.rw2' \) 2>/dev/null | wc -l | tr -d ' ')
say "image files found: $total"
find "$photos" -type f 2>/dev/null | sed -n 's/.*\.\([^./]*\)$/\1/p' | tr '[:upper:]' '[:lower:]' | sort | uniq -c | sort -rn | head -12 | sed 's/^/  /' | tee -a "$report"

section "Index, then a hard kill after ${crash_after}s (power loss)"
t0=$(now)
timed "$pilot/index1.time" "$bin" index --data "$data" "${workers[@]}" "$photos" >"$pilot/index1.log" 2>&1 &
pid=$!
for ((i = 0; i < crash_after; i++)); do
	kill -0 "$pid" 2>/dev/null || break
	sleep 1
done
if kill -0 "$pid" 2>/dev/null; then
	pkill -KILL -P "$pid" 2>/dev/null # archivis itself, under time
	kill -KILL "$pid" 2>/dev/null
	wait "$pid" 2>/dev/null
	sleep 1
	if pgrep -f "index --data $data" >/dev/null; then
		say "WARNING: archivis survived the kill; stopping it"
		pkill -KILL -f "index --data $data"
	fi
	say "killed after $(($(now) - t0))s with $("$bin" stats --data "$data" | awk '/^photos/ {print $2}') photos catalogued"
else
	wait "$pid"
	say "finished before the kill, in $(($(now) - t0))s (raise CRASH_AFTER to test recovery)"
fi
integrity=$(sqlite3 "$data/archivis.db" 'PRAGMA integrity_check' 2>&1 || true)
say "database integrity after the kill: ${integrity:-sqlite3 not installed}"

section "Resume and finish"
t1=$(now)
timed "$pilot/index2.time" "$bin" index --data "$data" "${workers[@]}" "$photos" >"$pilot/index2.log" 2>&1
rc=$?
secs=$(($(now) - t1))
say "exit status: $rc, $secs s, peak memory $(peak_mb "$pilot/index2.time")"
tail -c 2000 "$pilot/index2.log" | tr '\r' '\n' | grep -E 'analysed|catalogue|auto-labelled|error' | tail -4 | sed 's/^/  /' | tee -a "$report"
"$bin" stats --data "$data" | sed 's/^/  /' | tee -a "$report"
n=$("$bin" stats --data "$data" | awk '/^photos/ {print $2}')
say "overall: $n photos in $(($(now) - t0)) s ($(awk -v n="$n" -v s="$(($(now) - t0))" 'BEGIN {printf "%.2f", n / (s > 0 ? s : 1)}') photos/s, including the restart)"

section "Unchanged rerun (should take seconds and analyse nothing)"
t2=$(now)
"$bin" index --data "$data" "${workers[@]}" "$photos" >"$pilot/index3.log" 2>&1
say "$(($(now) - t2)) s: $(tr '\r' '\n' <"$pilot/index3.log" | grep -E 'analysed' | tail -1)"

section "Copies are recognised without re-analysis"
copies=$pilot/copies
mkdir -p "$copies"
find "$photos" -type f \( -iname '*.jpg' -o -iname '*.jpeg' \) 2>/dev/null | head -50 | while read -r f; do
	cp "$f" "$copies/$(basename "$(dirname "$f")")-$(basename "$f")"
done
"$bin" index --data "$data" "$copies" >"$pilot/copies.log" 2>&1
say "$(tr '\r' '\n' <"$pilot/copies.log" | grep -E 'copies' | tail -1)"

section "Errors (first 25)"
"$bin" errors --data "$data" --limit 25 2>&1 | sed 's/^/  /' | tee -a "$report"
say "by message:"
sqlite3 "$data/archivis.db" "SELECT COUNT(*), substr(error, 1, 90) FROM errors GROUP BY substr(error, 1, 40) ORDER BY 1 DESC LIMIT 10" 2>/dev/null | sed 's/^/  /' | tee -a "$report"

section "Web interface"
port=18088
t3=$(now)
"$bin" serve --data "$data" --addr "127.0.0.1:$port" >"$pilot/serve.log" 2>&1 &
spid=$!
for ((i = 0; i < 600; i++)); do
	curl -s -o /dev/null -m 2 "http://127.0.0.1:$port/guide" && break
	sleep 1
done
say "ready after $(($(now) - t3)) s"
for q in "/" "/?q=people+outdoors" "/?q=a+dog" "/?sort=worst" "/people" "/discover"; do
	say "  $(curl -s -o /dev/null -m 120 -w '%{http_code} %{time_total}s' "http://127.0.0.1:$port$q")  $q"
done
say "memory: $(ps -o rss= -p "$spid" | awk '{print int($1/1024)}') MB"
kill "$spid" 2>/dev/null
wait "$spid" 2>/dev/null

if [ -n "$old" ]; then
	section "Upgrade of an existing photodex catalogue (on a copy)"
	up=$pilot/upgrade
	mkdir -p "$up"
	for f in photodex.db photodex.db-wal photodex.db-shm archivis.db archivis.db-wal archivis.db-shm; do
		[ -f "$old/$f" ] && cp "$old/$f" "$up/"
	done
	[ -d "$HOME/.archivis/models" ] && ln -s "$HOME/.archivis/models" "$up/models"
	src=$(ls "$up"/*.db | head -1)
	before=$(sqlite3 "$src" "SELECT (SELECT COUNT(*) FROM photos)||' photos, '||(SELECT COUNT(*) FROM faces)||' faces, '||(SELECT COUNT(*) FROM people)||' people, '||(SELECT COUNT(*) FROM faces WHERE person_source='manual')||' confirmed faces'" 2>&1)
	say "before: $before"
	t4=$(now)
	"$bin" aesthetic models --data "$up" >"$pilot/upgrade.log" 2>&1
	say "opened in $(($(now) - t4)) s (exit $?)"
	after=$(sqlite3 "$up/archivis.db" "SELECT (SELECT COUNT(*) FROM photos)||' photos, '||(SELECT COUNT(*) FROM faces)||' faces, '||(SELECT COUNT(*) FROM people)||' people, '||(SELECT COUNT(*) FROM faces WHERE person_source='manual')||' confirmed faces'" 2>&1)
	say "after:  $after"
	[ "$before" = "$after" ] && say "counts unchanged: OK" || say "COUNTS CHANGED"
	sed 's/^/  /' "$pilot/upgrade.log" | tee -a "$report"
	"$bin" people list --data "$up" 2>&1 | head -10 | sed 's/^/  /' | tee -a "$report"
fi

section "Next, by hand"
say "  archivis serve --data $data   then open http://127.0.0.1:8088"
say "  see tools/pilot/CHECKLIST.md for what to try and note down"
echo
echo "Report: $report"
