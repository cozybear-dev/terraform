#!/usr/bin/env bash
# Stops applies part way and looks at the state left in the bucket: how many
# of the objects the provider had already created aren't in it.
#
#   killtest.sh SCENARIO BINDIR OUT LABEL...
#
# For each build (BINDIR/terraform-LABEL) it applies the scenario's plan
# "kill", which only creates objects, once to the end to see how long that
# takes (T), then KILLS times stops it with kill -9 at a moment between 0.1 T
# and 0.9 T, as an out-of-memory kill or a lost machine would. Then it stops
# it once with SIGINT to the process group, as Ctrl-C does, and once with
# SIGTERM to terraform, as "docker stop" does.
#
# After each stop it counts the Pub/Sub topics that the Google API mock holds
# and the ones the state in the bucket records, both less what there was
# before. The difference is the lag: objects that exist but that the state
# doesn't know about. The state is read with terraform-stock, so a state that
# upstream can't read is found.
#
# Environment: KILLS (default 10), SEED (default 1), PARALLELISM (default
# 10), DELAY_MS (default 50). The services are those of apply.sh.
#
# Writes OUT/kill.tsv (label, signal, attempt, seconds before the stop, lag,
# readable) and OUT/kill.md. Fails if a state can't be read, or if anything
# is missing after SIGINT or SIGTERM.
set -euo pipefail
[ $# -ge 4 ] || { echo "usage: killtest.sh SCENARIO BINDIR OUT LABEL..." >&2; exit 2; }
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
scenario=$(cd "$1" && pwd)
bindir=$(cd "$2" && pwd)
mkdir -p "$3/logs"
out=$(cd "$3" && pwd)
shift 3
KILLS=${KILLS:-10}
SEED=${SEED:-1}
PARALLELISM=${PARALLELISM:-10}
DELAY_MS=${DELAY_MS:-50}
stock=$bindir/terraform-stock
plan=$scenario/plans/kill.tfplan
export TF_IN_AUTOMATION=1 CHECKPOINT_DISABLE=1
say() { printf '[%s] %s\n' "$(date +%T)" "$*" >&2; }
cd "$scenario/work"

[ -d .terraform ] || "$stock" init -input=false -no-color > "$out/init.log"
curl -s -o /dev/null -X POST -H 'X-Vault-Token: tfperf-root' -d '{"type":"kv-v2"}' \
	http://127.0.0.1:8200/v1/sys/mounts/bench || true

mock_topics() {
	curl -sf 'http://127.0.0.1:8766/_count?prefix=projects/tfperf/topics/' |
		python3 -I -c 'import json, sys; print(json.load(sys.stdin)["count"])'
}
# state_topics prints the topics in the state in the bucket, or nothing if
# upstream can't read that state.
state_topics() {
	local list
	list=$("$stock" state list 2> "$out/state-list.err") || return 0
	grep -c 'google_pubsub_topic\.t\[' <<< "$list" || true
}

"$here/reset.sh" "$scenario"
mock0=$(mock_topics)
state0=$(state_topics)
[ -n "$state0" ] || { echo "killtest.sh: the scenario's state can't be read" >&2; exit 1; }
say "Before: $mock0 topics in the mock, $state0 in the state"

trap '"$here/netem.sh" off' EXIT
"$here/netem.sh" on "$DELAY_MS" 8200 8766

# stop LABEL SIGNAL AFTER: starts the apply in a session of its own, waits
# AFTER seconds and sends SIGNAL, then waits for it to end.
stop() {
	local label=$1 signal=$2 after=$3 pid target
	setsid "$bindir/terraform-$label" apply -input=false -no-color -parallelism="$PARALLELISM" "$plan" \
		> "$out/apply.log" 2>&1 &
	pid=$!
	sleep "$after"
	case $signal in
	KILL | INT) target=-$pid ;; # the whole process group
	TERM) target=$pid ;;
	esac
	kill -s "$signal" -- "$target" 2>/dev/null || true
	for _ in $(seq 3000); do
		kill -0 "$pid" 2>/dev/null || break
		sleep 0.1
	done
	if kill -0 "$pid" 2>/dev/null; then
		say "terraform-$label didn't stop within 5 minutes of SIG$signal"
		kill -s KILL -- "-$pid" 2>/dev/null || true
	fi
	wait "$pid" 2>/dev/null || true
	# Providers that outlived it.
	pkill -s "$pid" 2>/dev/null || true
}

: > "$out/kill.tsv"
for label in "$@"; do
	"$here/reset.sh" "$scenario"
	started=$(date +%s.%N)
	"$bindir/terraform-$label" apply -input=false -no-color -parallelism="$PARALLELISM" "$plan" > "$out/logs/$label-full.log" 2>&1
	full=$(python3 -I -c 'import sys, time; print(round(time.time() - float(sys.argv[1]), 1))' "$started")
	say "terraform-$label applies the plan in $full s"
	moments=$(python3 -I -c 'import random, sys
r = random.Random(int(sys.argv[1]))
t = float(sys.argv[2])
print(" ".join("%.1f" % r.uniform(0.1 * t, 0.9 * t) for _ in range(int(sys.argv[3]))))' "$SEED" "$full" "$KILLS")
	attempt=0
	for spec in $(for m in $moments; do echo "KILL:$m"; done) "INT:$(python3 -I -c 'import sys; print(round(float(sys.argv[1]) / 2, 1))' "$full")" \
		"TERM:$(python3 -I -c 'import sys; print(round(float(sys.argv[1]) / 2, 1))' "$full")"; do
		signal=${spec%%:*} after=${spec#*:}
		attempt=$((attempt + 1))
		"$here/reset.sh" "$scenario"
		stop "$label" "$signal" "$after"
		tail -n 20 "$out/apply.log" > "$out/logs/$label-$signal-$attempt.log"
		created=$(($(mock_topics) - mock0))
		recorded=$(state_topics)
		if [ -n "$recorded" ]; then
			lag=$((created - (recorded - state0)))
			readable=yes
		else
			lag=NA
			readable=no
			cp "$out/state-list.err" "$out/logs/$label-$signal-$attempt.unreadable"
		fi
		printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$label" "$signal" "$attempt" "$after" "$lag" "$readable" | tee -a "$out/kill.tsv" >&2
	done
done
rm -f "$out/apply.log"

python3 -I - "$out/kill.tsv" > "$out/kill.md" <<'PY'
import collections, statistics, sys
rows = [l.rstrip("\n").split("\t") for l in open(sys.argv[1]) if l.strip()]
groups = collections.OrderedDict()
for label, signal, attempt, after, lag, readable in rows:
    groups.setdefault((label, signal), []).append((lag, readable))
print("| build | stopped with | times | objects missing from the state: median | most | unreadable states |")
print("|---|---|---|---|---|---|")
for (label, signal), rs in groups.items():
    lags = [int(l) for l, r in rs if r == "yes"]
    print("| %s | SIG%s | %d | %s | %s | %d |" % (
        label, signal, len(rs), statistics.median(lags) if lags else "-", max(lags) if lags else "-",
        sum(1 for _, r in rs if r != "yes")))
print()
print("Objects missing: Pub/Sub topics the provider had created that the state in the bucket didn't record.")
PY
cat "$out/kill.md" >&2

if awk -F'\t' '$6 != "yes" || ($2 != "KILL" && $5 != 0) { bad = 1 } END { exit !bad }' "$out/kill.tsv"; then
	echo "killtest.sh: a state couldn't be read, or objects were missing after a graceful stop" >&2
	exit 1
fi
