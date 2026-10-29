#!/usr/bin/env bash
# Delays the traffic to and from some local ports, so that a service on this
# machine answers about as slowly as one across a network.
#
#   netem.sh on DELAY_MS PORT...   each packet to or from PORT on lo waits
#                                  DELAY_MS, so a round trip takes twice that
#   netem.sh off                   no delay
#
# Other traffic on lo, such as Terraform talking to its providers or to the
# GCS emulator, isn't delayed. Needs sudo.
set -euo pipefail
case "${1:-}" in
off)
	sudo tc qdisc del dev lo root 2>/dev/null || true
	;;
on)
	[ $# -ge 3 ] || { echo "usage: netem.sh on DELAY_MS PORT..." >&2; exit 2; }
	delay=$2
	shift 2
	sudo tc qdisc del dev lo root 2>/dev/null || true
	# Bands 1 to 3 take traffic as usual; only the filters below send
	# anything to band 4, which has the delay.
	sudo tc qdisc add dev lo root handle 1: prio bands 4
	if ! sudo tc qdisc add dev lo parent 1:4 handle 40: netem delay "${delay}ms" 2>/dev/null; then
		# Some kernels keep netem in a package of extra modules.
		sudo apt-get install -y -q "linux-modules-extra-$(uname -r)" > /dev/null
		sudo modprobe sch_netem
		sudo tc qdisc add dev lo parent 1:4 handle 40: netem delay "${delay}ms"
	fi
	for port in "$@"; do
		sudo tc filter add dev lo parent 1:0 protocol ip prio 1 u32 match ip dport "$port" 0xffff flowid 1:4
		sudo tc filter add dev lo parent 1:0 protocol ip prio 1 u32 match ip sport "$port" 0xffff flowid 1:4
	done
	;;
*)
	echo "usage: netem.sh on DELAY_MS PORT... | netem.sh off" >&2
	exit 2
	;;
esac
