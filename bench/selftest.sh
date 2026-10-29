#!/usr/bin/env bash
# Checks, on the machine that runs the apply benchmark, that limit.sh limits
# and measures memory and that netem.sh delays the ports it's given. Exits 0
# only if all of it works. Needs sudo, swap, and nothing listening on 8766.
set -uo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"; "$here/netem.sh" off 2>/dev/null; kill $server 2>/dev/null' EXIT
server=
failed=0
check() {
	if [ "$1" = ok ]; then
		echo "ok:   $2"
	else
		echo "FAIL: $2"
		failed=1
	fi
}
value() { sed -n "s/^$1=//p" "$2"; }

# Allocates MB megabytes and touches every page, so that they're resident.
alloc() {
	python3 -I -c "import sys, time
n = int(sys.argv[1]) << 20
b = bytearray(n)
b[::4096] = b'\x01' * len(range(0, n, 4096))
time.sleep(0.5)" "$1"
}

"$here/limit.sh" 1 "$tmp/small" bash -c "$(declare -f alloc); alloc 300"
rc=$?
peak=$(value mem_peak_mb "$tmp/small")
swap=$(value swap_peak_mb "$tmp/small")
[ "$rc" = 0 ] && [ "${peak:-0}" -ge 300 ] && [ "${swap:-1}" = 0 ] && r=ok || r=bad
check $r "300 MB under a 1 GB limit: exit $rc, peak ${peak:-?} MB, swap ${swap:-?} MB"

"$here/limit.sh" 1 "$tmp/big" bash -c "$(declare -f alloc); alloc 1500"
rc=$?
swap=$(value swap_peak_mb "$tmp/big")
ooms=$(value oom_kills "$tmp/big")
[ "${swap:-0}" -gt 0 ] || [ "${ooms:-0}" -gt 0 ] && r=ok || r=bad
check $r "1500 MB under a 1 GB limit: exit $rc, swap ${swap:-?} MB, OOM kills ${ooms:-?}"

python3 -I -m http.server 8766 --bind 127.0.0.1 --directory "$tmp" > /dev/null 2>&1 &
server=$!
for _ in $(seq 50); do
	curl -s -o /dev/null http://127.0.0.1:8766/ && break
	sleep 0.1
done
"$here/netem.sh" on 50 8766
slow=$(curl -s -o /dev/null -w '%{time_total}' http://127.0.0.1:8766/)
"$here/netem.sh" off
fast=$(curl -s -o /dev/null -w '%{time_total}' http://127.0.0.1:8766/)
python3 -I -c "import sys; sys.exit(0 if float(sys.argv[1]) >= 0.1 else 1)" "$slow" && r=ok || r=bad
check $r "a request to a delayed port takes ${slow} s (at least 0.1 s)"
python3 -I -c "import sys; sys.exit(0 if float(sys.argv[1]) < 0.02 else 1)" "$fast" && r=ok || r=bad
check $r "after the delay is removed it takes ${fast} s (under 0.02 s)"

exit $failed
