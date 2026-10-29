#!/usr/bin/env bash
# Measures applying the saved plans of a scenario (from scenario.sh) with
# several builds of terraform, under a memory limit, against APIs slowed down
# to answer as real ones do.
#
#   apply.sh SCENARIO BINDIR OUT
#
# BINDIR holds terraform-LABEL for each build. Vault on 127.0.0.1:8200 and
# the GCS emulator on 127.0.0.1:4443 with a bucket tfperf-state have to be
# running; the Google API mock is started by reset.sh. Environment:
#
#   LABELS              the builds, in the order to report them (default:
#                       every terraform-* in BINDIR)
#   PLANS               default: p50 p300 p1500
#   PARALLELISM         default: 10 100
#   RUNS                default: 2
#   MEM_GB              memory limit of each apply (default: 10)
#   DELAY_MS            delay each way to and from Vault and the mock
#                       (default: 50)
#   APPLY_TIMEOUT_MIN   an apply still running after this is stopped and
#                       recorded as a timeout (default: 40)
#   COUNT_BIN           terraform used to count what's in the state after
#                       each apply (default: BINDIR/terraform-full)
#   LINES_TSV           changed lines of each build, for the summary
#
# The builds take turns, so that anything else going on in the machine
# affects them alike. Writes OUT/times.tsv (see summary.py), OUT/checks.tsv,
# the end of each apply's output in OUT/logs, and OUT/summary.md and
# OUT/summary.json.
set -euo pipefail
[ $# -eq 3 ] || { echo "usage: apply.sh SCENARIO BINDIR OUT" >&2; exit 2; }
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
scenario=$(cd "$1" && pwd)
bindir=$(cd "$2" && pwd)
mkdir -p "$3/logs"
out=$(cd "$3" && pwd)
read -r -a labels <<< "${LABELS:-$(cd "$bindir" && ls terraform-* | sed 's/^terraform-//' | tr '\n' ' ')}"
# A build that didn't compile has no binary, and is left out.
built=()
for label in "${labels[@]}"; do
	if [ -x "$bindir/terraform-$label" ]; then
		built+=("$label")
	else
		echo "There is no terraform-$label; it's left out." >&2
	fi
done
labels=("${built[@]}")
[ "${#labels[@]}" -gt 0 ] || { echo "apply.sh: no builds to measure" >&2; exit 1; }
PLANS=${PLANS:-p50 p300 p1500}
PARALLELISM=${PARALLELISM:-10 100}
RUNS=${RUNS:-2}
MEM_GB=${MEM_GB:-10}
DELAY_MS=${DELAY_MS:-50}
APPLY_TIMEOUT_MIN=${APPLY_TIMEOUT_MIN:-40}
COUNT_BIN=${COUNT_BIN:-$bindir/terraform-full}
export TF_IN_AUTOMATION=1 CHECKPOINT_DISABLE=1
say() { printf '[%s] %s\n' "$(date +%T)" "$*" >&2; }
work=$scenario/work
cd "$work"

say "Installing the providers"
"$bindir/terraform-${labels[0]}" init -input=false -no-color > "$out/init.log"
# The applies write Vault secrets into this mount, which the state says
# exists.
curl -s -o /dev/null -X POST -H 'X-Vault-Token: tfperf-root' -d '{"type":"kv-v2"}' \
	http://127.0.0.1:8200/v1/sys/mounts/bench || true

# managed_instances counts the resource instances in the state in the
# bucket, leaving out data sources.
managed_instances() {
	"$COUNT_BIN" state list | grep -Evc '(^|\.)data\.' || true
}
"$here/reset.sh" "$scenario"
original=$(managed_instances)
say "The state has $original resource instances"

trap '"$here/netem.sh" off' EXIT
"$here/netem.sh" on "$DELAY_MS" 8200 8766

: > "$out/times.tsv"
: > "$out/checks.tsv"
for run in $(seq "$RUNS"); do
	for plan in $PLANS; do
		expected=$(python3 -I -c 'import json, sys; c = json.load(open(sys.argv[1])); print(int(sys.argv[2]) + c["add"] - c["destroy"])' \
			"$scenario/plans/$plan.counts.json" "$original")
		for par in $PARALLELISM; do
			for label in "${labels[@]}"; do
				name=$label-$plan-$par-$run
				say "Applying $plan with $label, -parallelism=$par, run $run"
				"$here/reset.sh" "$scenario"
				rm -f "$out/time.tmp" "$out/stats.tmp"
				set +e
				"$here/limit.sh" "$MEM_GB" "$out/stats.tmp" \
					timeout -s KILL "${APPLY_TIMEOUT_MIN}m" \
					/usr/bin/time -f $'%e\t%U\t%S\t%M' -o "$out/time.tmp" \
					"$bindir/terraform-$label" apply -input=false -no-color -parallelism="$par" "$scenario/plans/$plan.tfplan" \
					> "$out/apply.log" 2>&1
				rc=$?
				set -e
				tail -n 30 "$out/apply.log" > "$out/logs/$name.log"
				if [ -s "$out/time.tmp" ]; then
					times=$(tail -n 1 "$out/time.tmp")
				else
					times=$(printf '%s\t0\t0\t0' $((APPLY_TIMEOUT_MIN * 60)))
				fi
				mem=$(sed -n 's/^mem_peak_mb=//p' "$out/stats.tmp" 2>/dev/null)
				swap=$(sed -n 's/^swap_peak_mb=//p' "$out/stats.tmp" 2>/dev/null)
				ooms=$(sed -n 's/^oom_kills=//p' "$out/stats.tmp" 2>/dev/null)
				result=$rc
				if [ "$rc" = 0 ]; then
					got=$(managed_instances)
					if [ "$got" != "$expected" ]; then
						say "The state has $got resource instances; the plan should have left $expected"
						result=wrong-count
					fi
					if [ "$run" = 1 ] && [ "$par" = "${PARALLELISM%% *}" ]; then
						# Applying the same plan again, to a state that has
						# moved on, has to be refused.
						set +e
						"$bindir/terraform-$label" apply -input=false -no-color "$scenario/plans/$plan.tfplan" > "$out/stale.log" 2>&1
						stale_rc=$?
						set -e
						if [ "$stale_rc" != 0 ] && grep -qi stale "$out/stale.log"; then
							printf '%s\t%s\tstale\tok\n' "$label" "$plan" >> "$out/checks.tsv"
						else
							printf '%s\t%s\tstale\tstale-accepted\n' "$label" "$plan" >> "$out/checks.tsv"
							tail -n 30 "$out/stale.log" > "$out/logs/$name-stale.log"
						fi
					fi
				fi
				printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$label" "$plan" "$par" "$run" "$times" \
					"${mem:-NA}" "${swap:-NA}" "${ooms:-0}" "$result" >> "$out/times.tsv"
				tail -n 1 "$out/times.tsv" >&2
			done
		done
	done
done
rm -f "$out/time.tmp" "$out/stats.tmp" "$out/apply.log" "$out/stale.log"
python3 -I "$here/summary.py" "$out" ${LINES_TSV:+"$LINES_TSV"}
cat "$out/summary.md" >&2
