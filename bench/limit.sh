#!/usr/bin/env bash
# Runs a command with a limit on its memory, as a container's limit would,
# and records how much memory it used.
#
#   limit.sh MEM_GB STATS_FILE COMMAND...
#
# The command runs as the caller, with the caller's environment and
# directory, in a cgroup (v2) of its own whose memory is limited to MEM_GB
# GiB. Beyond that the kernel moves its memory to swap, or kills it if there
# is no swap left. Afterwards STATS_FILE holds:
#
#   mem_peak_mb=   the most memory the command and its children held at once
#   swap_peak_mb=  the most of it that was in swap at once
#   oom_kills=     how many of its processes the kernel killed for memory
#
# It exits with the command's exit status. Needs sudo, and swap to be on.
set -euo pipefail
[ $# -ge 3 ] || { echo "usage: limit.sh MEM_GB STATS_FILE COMMAND..." >&2; exit 2; }
mem_gb=$1 stats=$2
shift 2

cg=/sys/fs/cgroup/tfbench-$$-$RANDOM
sudo mkdir "$cg"
cleanup() {
	# Anything the command left running goes with it.
	if [ -e "$cg/cgroup.procs" ]; then
		sudo xargs -r kill -9 < "$cg/cgroup.procs" 2>/dev/null || true
		for _ in $(seq 50); do
			[ -s "$cg/cgroup.procs" ] || break
			sleep 0.1
		done
		sudo rmdir "$cg" 2>/dev/null || true
	fi
}
trap cleanup EXIT
echo $((mem_gb * 1024 * 1024 * 1024)) | sudo tee "$cg/memory.max" > /dev/null
echo max | sudo tee "$cg/memory.swap.max" > /dev/null

# The shell moves itself into the group and then becomes the command.
set +e
bash -c 'echo $$ | sudo tee "$1/cgroup.procs" > /dev/null && shift && exec "$@"' limit "$cg" "$@"
rc=$?
set -e

mb() { [ -r "$1" ] && echo $(($(cat "$1") / 1024 / 1024)) || echo NA; }
{
	echo "mem_peak_mb=$(mb "$cg/memory.peak")"
	echo "swap_peak_mb=$(mb "$cg/memory.swap.peak")"
	echo "oom_kills=$(sed -n 's/^oom_kill //p' "$cg/memory.events")"
} > "$stats"
exit $rc
