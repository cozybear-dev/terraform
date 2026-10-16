#!/usr/bin/env bash
# Measures "terraform show -no-color PLANFILE", the human-readable form of a
# saved plan, with a build of the fork and a build of upstream, and checks
# that the two print exactly the same thing.
#
#   show.sh FORK UPSTREAM OUT
#
# FORK and UPSTREAM are terraform binaries. In the current directory, which
# should be empty, it generates a configuration with gen4.py: MODULES calls
# of a module with PER_MODULE instances of each of its resources, using the
# Vault and Google providers. It creates the objects, in a Vault dev server
# on 127.0.0.1:8200 and the Google API mock on 127.0.0.1:8766, which have to
# be running, and saves two plans: "one", which changes a single resource,
# and "many", which changes a label of every Pub/Sub topic. Then it shows
# each plan RUNS times with each binary, and once more with the fork
# recording CPU and wall-clock profiles.
#
# Everything it measures goes to OUT: times.tsv (label, wall seconds, user
# and system CPU seconds, peak memory in KB), the digests of what each show
# printed, the profiles, and summary.md.
set -euo pipefail

[ $# -eq 3 ] || { echo "usage: show.sh FORK UPSTREAM OUT" >&2; exit 2; }
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
fork=$(realpath "$1")
upstream=$(realpath "$2")
mkdir -p "$3"
out=$(cd "$3" && pwd)
MODULES=${MODULES:-100}
PER_MODULE=${PER_MODULE:-100}
RUNS=${RUNS:-3}
export TF_IN_AUTOMATION=1 CHECKPOINT_DISABLE=1

say() { printf '%s\n' "$*" >&2; }

# timed LABEL COMMAND... runs the command with its output in OUT/LABEL.out
# and OUT/LABEL.err, and adds a line for it to OUT/times.tsv.
timed() {
	local label=$1
	shift
	/usr/bin/time -f "%e	%U	%S	%M" -o "$out/time.tmp" "$@" > "$out/$label.out" 2> "$out/$label.err"
	printf '%s\t%s\n' "$label" "$(cat "$out/time.tmp")" >> "$out/times.tsv"
	rm -f "$out/time.tmp"
}

# keep_tail LABEL shortens a long output to its last lines.
keep_tail() {
	tail -n 40 "$out/$1.out" > "$out/$1.tmp" && mv "$out/$1.tmp" "$out/$1.out"
}

: > "$out/times.tsv"
python3 "$here/gen4.py" . "$MODULES" "$PER_MODULE" | tee "$out/scenario.txt" >&2

say "Installing the providers"
"$upstream" init -input=false -no-color > "$out/init.log"

say "Creating the objects"
timed apply "$fork" apply -auto-approve -input=false -no-color -parallelism=20
keep_tail apply

say "Saving the plans"
timed plan-one "$fork" plan -refresh=false -input=false -no-color -var bump=1 -out=one.tfplan
keep_tail plan-one
sed -i 's/tier   = "bench"/tier   = "bench2"/' modules/app/main.tf
timed plan-many "$fork" plan -refresh=false -input=false -no-color -out=many.tfplan
keep_tail plan-many
ls -l terraform.tfstate one.tfplan many.tfplan | awk '{ print $5, $9 }' > "$out/sizes.txt"

for plan in one many; do
	say "Showing the plan \"$plan\""
	for run in $(seq "$RUNS"); do
		# Taking turns, so that anything else going on in the machine
		# affects both alike.
		for who in upstream fork; do
			bin=$upstream
			[ "$who" = upstream ] || bin=$fork
			timed "show-$plan-$who-$run" "$bin" show -no-color "$plan.tfplan"
			sha256sum < "$out/show-$plan-$who-$run.out" | cut -d' ' -f1 > "$out/show-$plan-$who-$run.sha256"
			wc -l < "$out/show-$plan-$who-$run.out" > "$out/show-$plan-$who-$run.lines"
			if [ "$run" -gt 1 ] || [ "$who" = fork ]; then
				rm "$out/show-$plan-$who-$run.out"
			fi
		done
	done
	# The first upstream run's output, shortened, so that a person can see
	# what was being shown.
	head -n 60 "$out/show-$plan-upstream-1.out" > "$out/show-$plan-upstream.head"
	tail -n 20 "$out/show-$plan-upstream-1.out" > "$out/show-$plan-upstream.tail"
	rm "$out/show-$plan-upstream-1.out"

	mkdir -p "$out/profile-$plan"
	TF_PROFILE_DIR="$out/profile-$plan" TF_PROFILE_WALL=1 "$fork" show -no-color "$plan.tfplan" > /dev/null
done

python3 - "$out" > "$out/summary.md" <<'PY'
import collections, glob, os, statistics, sys
out = sys.argv[1]
rows = collections.defaultdict(list)
for line in open(os.path.join(out, "times.tsv")):
    label, wall, user, system, rss = line.rstrip("\n").split("\t")
    rows[label.rsplit("-", 1)[0] if label.startswith("show-") else label].append(
        (float(wall), float(user) + float(system), int(rss)))

def cell(label):
    runs = rows[label]
    wall = statistics.median(r[0] for r in runs)
    cpu = statistics.median(r[1] for r in runs)
    rss = max(r[2] for r in runs)
    return "%.1f s | %.1f s | %.0f MB" % (wall, cpu, rss / 1024)

def digests(plan, who):
    return {open(p).read().strip() for p in glob.glob(os.path.join(out, "show-%s-%s-*.sha256" % (plan, who)))}

def lines(plan):
    return open(os.path.join(out, "show-%s-upstream-1.lines" % plan)).read().strip()

print("| | wall | CPU | peak memory |")
print("|---|---|---|---|")
for label in ("apply", "plan-one", "plan-many"):
    print("| `%s` (fork) | %s |" % (label, cell(label)))
print()
print("| show -no-color | upstream: wall | CPU | peak memory | fork: wall | CPU | peak memory | output |")
print("|---|---|---|---|---|---|---|---|")
for plan in ("one", "many"):
    up, fk = digests(plan, "upstream"), digests(plan, "fork")
    same = "identical" if len(up) == 1 and up == fk else "**DIFFERENT**"
    print("| plan `%s`, %s lines | %s | %s | %s |" % (plan, lines(plan), cell("show-%s-upstream" % plan), cell("show-%s-fork" % plan), same))
print()
print("Medians of the runs; peak memory is the highest.")
print()
print("```")
print(open(os.path.join(out, "scenario.txt")).read().strip())
print(open(os.path.join(out, "sizes.txt")).read().strip())
print("```")
PY
cat "$out/summary.md" >&2

# The job fails if the fork printed anything different from upstream.
if grep -q DIFFERENT "$out/summary.md"; then
	exit 1
fi
