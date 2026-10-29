#!/usr/bin/env bash
# Puts the services back as they were when a scenario's plans were saved, so
# that every apply starts from the same place:
#
#   reset.sh SCENARIO
#
# - restarts the Google API mock on 127.0.0.1:8766 with SCENARIO/mock.json,
# - uploads SCENARIO/state.tfstate to bench/default.tfstate in the bucket
#   tfperf-state of the GCS emulator on 127.0.0.1:4443,
# - removes the state lock, if an earlier apply left one.
#
# The mock's process id and log are kept in BENCH_RUN (default: a directory
# "run" beside SCENARIO). Vault isn't reset: the applies only write to it,
# and every write is a whole object.
set -euo pipefail
[ $# -eq 1 ] || { echo "usage: reset.sh SCENARIO" >&2; exit 2; }
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
scenario=$(cd "$1" && pwd)
run=${BENCH_RUN:-$(dirname "$scenario")/run}
mkdir -p "$run"
gcs=http://127.0.0.1:4443

if [ -s "$run/gcpmock.pid" ]; then
	kill "$(cat "$run/gcpmock.pid")" 2>/dev/null || true
	for _ in $(seq 100); do
		curl -s -o /dev/null http://127.0.0.1:8766/_count || break
		sleep 0.1
	done
fi
nohup python3 -I "$here/gcpmock.py" 8766 --load "$scenario/mock.json" >> "$run/gcpmock.log" 2>&1 &
echo $! > "$run/gcpmock.pid"
for _ in $(seq 600); do
	curl -s -o /dev/null http://127.0.0.1:8766/_count && break
	sleep 0.1
done
curl -sf -o /dev/null http://127.0.0.1:8766/_count || { echo "reset.sh: the mock didn't start" >&2; exit 1; }

curl -sf -o /dev/null -X POST -H 'Content-Type: application/json' --data-binary "@$scenario/state.tfstate" \
	"$gcs/upload/storage/v1/b/tfperf-state/o?uploadType=media&name=bench%2Fdefault.tfstate"
curl -s -o /dev/null -X DELETE "$gcs/storage/v1/b/tfperf-state/o/bench%2Fdefault.tflock" || true
