#!/usr/bin/env bash
# Makes what the apply benchmark measures: a configuration, the objects and
# state for it, and saved plans that change some of it.
#
#   scenario.sh PLANNER OUT
#
# PLANNER is the terraform binary that creates the objects and saves the
# plans (a build of the full fork). Vault on 127.0.0.1:8200, the Google API
# mock on 127.0.0.1:8766 and the GCS emulator on 127.0.0.1:4443, with a
# bucket tfperf-state, have to be running, without any delay. Environment:
#
#   MODULES, PER_MODULE, BLOB_KB   passed to gen4.py, with COPIES=1
#   PLANS   space-separated name:target:mix, where mix is that of
#           mkchanges.py (default: p50:50:mixed p300:300:mixed
#           p1500:1500:mixed kill:1500:create)
#
# It writes to OUT:
#
#   work/                 the configuration, with the state in the bucket
#   state.tfstate         the state once everything is created
#   mock.json             what the mock held then
#   plans/NAME.tfplan     the saved plans, each against that state
#   plans/NAME.counts.json  {"add", "change", "destroy"} from the plan, and
#                         what mkchanges.py expected
#   sizes.txt             sizes in bytes of the state and the plans
#
# It fails if a plan's counts are more than a quarter off what mkchanges.py
# expected.
set -euo pipefail
[ $# -eq 2 ] || { echo "usage: scenario.sh PLANNER OUT" >&2; exit 2; }
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
planner=$(realpath "$1")
mkdir -p "$2/plans"
out=$(cd "$2" && pwd)
work=$out/work
MODULES=${MODULES:-1000}
PER_MODULE=${PER_MODULE:-20}
BLOB_KB=${BLOB_KB:-0}
PLANS=${PLANS:-p50:50:mixed p300:300:mixed p1500:1500:mixed kill:1500:create}
export TF_IN_AUTOMATION=1 CHECKPOINT_DISABLE=1
say() { printf '[%s] %s\n' "$(date +%T)" "$*" >&2; }

COPIES=1 BLOB_KB=$BLOB_KB python3 -I "$here/gen4.py" "$work" "$MODULES" "$PER_MODULE" | tee "$out/scenario.txt" >&2
cat > "$work/backend.tf" <<'EOF'
terraform {
  backend "gcs" {
    bucket                  = "tfperf-state"
    prefix                  = "bench"
    access_token            = "fake-token"
    storage_custom_endpoint = "http://127.0.0.1:4443/storage/v1/"
  }
}
EOF
cd "$work"
say "Installing the providers"
"$planner" init -input=false -no-color > "$out/init.log"

say "Creating the objects"
"$planner" apply -auto-approve -input=false -no-color -parallelism=50 > "$out/create.log" 2>&1 || {
	tail -n 40 "$out/create.log" >&2
	exit 1
}
tail -n 5 "$out/create.log" >&2
curl -sf -o "$out/state.tfstate" "http://127.0.0.1:4443/storage/v1/b/tfperf-state/o/bench%2Fdefault.tfstate?alt=media"
curl -sf -o "$out/mock.json" http://127.0.0.1:8766/_dump

# The configuration as it was, to go back to after each plan's edits.
tar -cf "$out/config.tar" main.tf modules
for spec in $PLANS; do
	IFS=: read -r name target mix <<< "$spec"
	say "Saving the plan \"$name\": $target changes, $mix"
	expected=$(python3 -I "$here/mkchanges.py" "$work" "$target" "$mix" 1)
	"$planner" plan -input=false -no-color -parallelism=50 -out="$out/plans/$name.tfplan" > "$out/plans/$name.log" 2>&1 || {
		tail -n 40 "$out/plans/$name.log" >&2
		exit 1
	}
	rm -rf modules main.tf
	tar -xf "$out/config.tar"
	# "Plan: 31 to add, 200 to change, 32 to destroy." A replacement is
	# one add and one destroy.
	python3 -I - "$out/plans/$name.log" "$expected" > "$out/plans/$name.counts.json" <<'PY'
import json, re, sys
text = open(sys.argv[1]).read()
m = re.search(r"Plan: (\d+) to add, (\d+) to change, (\d+) to destroy", text)
if not m:
    sys.exit("the plan has no summary line")
add, change, destroy = map(int, m.groups())
e = json.loads(sys.argv[2])
want = {"add": e["create"] + e["replace"], "change": e["update"], "destroy": e["delete"] + e["replace"]}
got = {"add": add, "change": change, "destroy": destroy}
print(json.dumps({**got, "expected": want, "mkchanges": e}))
bad = [k for k in got if abs(got[k] - want[k]) > max(2, 0.25 * want[k])]
if bad:
    sys.exit("the plan's %s %s, expected %s" % (", ".join(bad), got, want))
PY
	cat "$out/plans/$name.counts.json" >&2
done
rm "$out/config.tar"

{
	echo "state $(stat -c %s "$out/state.tfstate")"
	for f in "$out"/plans/*.tfplan; do
		echo "$(basename "$f" .tfplan) $(stat -c %s "$f")"
	done
} > "$out/sizes.txt"
cat "$out/sizes.txt" >&2
