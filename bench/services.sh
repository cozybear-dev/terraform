#!/usr/bin/env bash
# Prepares a GitHub runner for the apply benchmark.
#
#   services.sh start        Vault on 127.0.0.1:8200 and the GCS emulator on
#                            127.0.0.1:4443 with an empty bucket tfperf-state
#   services.sh start-mock   the same, and the Google API mock on 8766 with
#                            nothing in it (reset.sh starts it otherwise)
#   services.sh swap GB      adds GB of swap, for applies that go over their
#                            memory limit
#
# The mock's process id and log go in BENCH_RUN (default: ./run), where
# reset.sh looks for them.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
run=${BENCH_RUN:-$PWD/run}
mkdir -p "$run"

wait_for() {
	local what=$1 url=$2
	for _ in $(seq 120); do
		curl -s -o /dev/null "$url" && return 0
		sleep 1
	done
	echo "::error::$what didn't start"
	return 1
}

start() {
	# For the time and memory each apply takes.
	[ -x /usr/bin/time ] || { sudo apt-get update -q && sudo apt-get install -y -q time; } > /dev/null
	docker run --detach --name vault -p 127.0.0.1:8200:8200 \
		-e VAULT_DEV_ROOT_TOKEN_ID=tfperf-root -e VAULT_DEV_LISTEN_ADDRESS=0.0.0.0:8200 \
		--cap-add IPC_LOCK hashicorp/vault:latest > /dev/null
	docker run --detach --name gcs -p 127.0.0.1:4443:4443 fsouza/fake-gcs-server:latest \
		-scheme http -port 4443 -public-host 127.0.0.1:4443 -backend memory > /dev/null
	wait_for Vault http://127.0.0.1:8200/v1/sys/health || { docker logs vault; exit 1; }
	wait_for "The GCS emulator" http://127.0.0.1:4443/storage/v1/b || { docker logs gcs; exit 1; }
	curl -sf -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"name":"tfperf-state"}' \
		'http://127.0.0.1:4443/storage/v1/b?project=tfperf'
}

case "${1:-}" in
start)
	start
	;;
start-mock)
	start
	nohup python3 -I "$here/gcpmock.py" 8766 >> "$run/gcpmock.log" 2>&1 &
	echo $! > "$run/gcpmock.pid"
	wait_for "The Google API mock" http://127.0.0.1:8766/_count
	;;
swap)
	gb=${2:?usage: services.sh swap GB}
	sudo fallocate -l "${gb}G" /bench-swapfile
	sudo chmod 600 /bench-swapfile
	sudo mkswap /bench-swapfile > /dev/null
	sudo swapon /bench-swapfile
	swapon --show
	free -m
	;;
*)
	echo "usage: services.sh start | start-mock | swap GB" >&2
	exit 2
	;;
esac
