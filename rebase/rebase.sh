#!/usr/bin/env bash
# Moves the fork from the version of upstream it's on to another one.
#
# The fork is a version of upstream with the fork's commits on top. Moving it
# means replaying those commits onto the new version. This script does the
# parts of that which need no judgement, checks the result, and hands the
# parts that do need judgement to Claude, if it's allowed to.
#
#   rebase.sh start TARGET   Replay the fork's commits onto TARGET, which is
#                            an upstream tag such as v1.17.0, or a commit.
#   rebase.sh continue       Finish what start began: let Claude resolve what
#                            stopped the replay, record the new upstream in
#                            the proof, check the result, and let Claude
#                            repair what the checks reject. Writes a summary
#                            and, if everything passed, a bundle of the result.
#   rebase.sh run TARGET     Both of those.
#   rebase.sh check          Only the checks.
#
# It works on the checkout in $WORK (default: the current directory), which
# has to be on the fork's branch with nothing uncommitted, and leaves the
# result on a local branch called "candidate". It pushes nothing.
#
# Settings, from the environment:
#   OUT          Where the logs, the summary and the bundle go (default ./.fork-rebase)
#   AGENT        auto (default): use Claude if the "claude" command and a
#                credential for it are there. on: use it or fail. off: never.
#   MODEL        Default claude-opus-5-5
#   EFFORT       Default high
#   BUDGET_USD   The most one round of Claude's work may cost (default 15)
#   MAX_ROUNDS   How many rounds Claude gets (default 3)
#   CHECKS       Which checks to run (default "build vet proof-self unit proof")
#   JOBS         Passed to the proof as -jobs (default 4)
set -euo pipefail

UPSTREAM_URL=${UPSTREAM_URL:-https://github.com/hashicorp/terraform.git}
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
WORK=$(cd "${WORK:-.}" && pwd)
OUT=${OUT:-$WORK/.fork-rebase}
AGENT=${AGENT:-auto}
MODEL=${MODEL:-claude-opus-5-5}
EFFORT=${EFFORT:-high}
BUDGET_USD=${BUDGET_USD:-15}
MAX_ROUNDS=${MAX_ROUNDS:-3}
CHECKS=${CHECKS:-build vet proof-self unit proof}
JOBS=${JOBS:-4}

mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
STATE=$OUT/state
NOTES=$OUT/notes.md

say() { printf '%s\n' "$*" >&2; }
die() { say "rebase.sh: $*"; exit 1; }

# Values that later steps need are kept in a file, so that "continue" can be
# a separate command from "start".
remember() { printf '%s=%q\n' "$1" "$2" >> "$STATE"; }
recall() { [ -f "$STATE" ] || die "nothing has been started here: run \"rebase.sh start TARGET\" first"; . "$STATE"; }

in_progress() {
	[ -d "$(git -C "$WORK" rev-parse --git-path rebase-merge)" ] ||
		[ -d "$(git -C "$WORK" rev-parse --git-path rebase-apply)" ] ||
		[ -f "$(git -C "$WORK" rev-parse --git-path CHERRY_PICK_HEAD)" ]
}

# The commits listed in UPSTREAM_BACKPORTS at the given revision, in order.
backports_at() {
	git -C "$WORK" show "$1:fork-proof/UPSTREAM_BACKPORTS" 2>/dev/null |
		grep -v '^[[:space:]]*#' | awk 'NF { print $1 }' || true
}

# json_field prints one top-level field of a JSON file, or "unknown". An
# amount of money is printed as one.
json_field() {
	python3 -c 'import json, sys
try:
    value = json.load(open(sys.argv[1])).get(sys.argv[2], "unknown")
    print("%.2f" % value if isinstance(value, float) else value)
except Exception:
    print("unknown")' "$1" "$2"
}

# agent_worked says whether a round of Claude's got as far as doing anything.
# It didn't if its credential was refused or the service couldn't be reached,
# and then another round would go the same way.
agent_worked() {
	python3 -c 'import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(1)
sys.exit(1 if d.get("is_error") and not d.get("total_cost_usd") else 0)' "$OUT/agent-$1.json"
}

# agent_said prints how a round ended, in Claude's words, or the error if it
# didn't get as far as answering.
agent_said() {
	local file=$OUT/agent-$1.json said
	if [ -s "$file" ]; then
		said=$(json_field "$file" result)
		# Stopped by the spending limit, for one, it says nothing.
		[ "$said" != unknown ] || said="It stopped without a last message ($(json_field "$file" subtype))."
		echo "$said"
	else
		echo "Claude didn't get as far as answering:"
		echo
		echo '```'
		tail -n 20 "${file%.json}.err" 2>/dev/null
		echo '```'
	fi
}

have_agent() {
	[ "$AGENT" != off ] || return 1
	command -v claude >/dev/null 2>&1 || return 1
	[ "$AGENT" = on ] || [ -n "${ANTHROPIC_API_KEY:-}${CLAUDE_CODE_OAUTH_TOKEN:-}" ]
}

start() {
	local target=$1
	cd "$WORK"
	[ -z "$(git status --porcelain --untracked-files=no)" ] || die "$WORK has uncommitted changes"
	in_progress && die "a rebase or cherry-pick is already in progress in $WORK"
	[ -f fork-proof/UPSTREAM_REF ] || die "$WORK isn't a checkout of the fork: it has no fork-proof/UPSTREAM_REF"

	local old new from_head from_name
	old=$(tr -d '[:space:]' < fork-proof/UPSTREAM_REF)
	from_head=$(git rev-parse HEAD)
	from_name=$(git rev-parse --abbrev-ref HEAD)
	git merge-base --is-ancestor "$old" HEAD || die "the commit in fork-proof/UPSTREAM_REF isn't an ancestor of this branch"

	say "Fetching $target from $UPSTREAM_URL"
	if [[ $target =~ ^[0-9a-f]{40}$ ]]; then
		git fetch --quiet --no-tags "$UPSTREAM_URL" "$target"
		new=$target
	else
		git fetch --quiet --no-tags "$UPSTREAM_URL" "+refs/tags/$target:refs/tags/$target"
		new=$(git rev-parse "refs/tags/$target^{commit}")
	fi
	[ "$new" != "$old" ] || die "the fork is already on $target"

	# The fixes of upstream's that the branch carries have to be there to
	# compare with afterwards.
	local b
	for b in $(backports_at HEAD); do
		git cat-file -e "$b^{commit}" 2>/dev/null || git fetch --quiet --no-tags "$UPSTREAM_URL" "$b"
	done

	# Commits made here have to be by someone. A person's own checkout says
	# who already.
	git config user.name >/dev/null || git config user.name "github-actions[bot]"
	git config user.email >/dev/null || git config user.email "41898282+github-actions[bot]@users.noreply.github.com"

	: > "$STATE"
	: > "$NOTES"
	remember TARGET "$target"
	remember OLD "$old"
	remember NEW "$new"
	remember FROM_HEAD "$from_head"
	remember FROM_NAME "$from_name"
	remember ROUNDS 0

	git branch --force fork-rebase-from "$from_head" >/dev/null
	git switch --quiet --force-create candidate "$from_head"

	# Every commit after the old upstream is replayed, in order, onto the new
	# one. Commits marked as amendments of earlier ones are folded into them
	# first. A commit that the new upstream makes pointless, which is what
	# happens to a fix of upstream's that the fork carried once upstream has
	# released it, comes out empty and is dropped.
	say "Replaying $(git rev-list --count "$old..$from_head") commits from $(git rev-parse --short=12 "$old") onto $target ($(git rev-parse --short=12 "$new"))"
	if GIT_SEQUENCE_EDITOR=true GIT_EDITOR=true git rebase --interactive --autosquash --empty=drop --onto "$new" "$old" > "$OUT/replay.log" 2>&1; then
		say "Every commit replayed without a conflict."
	else
		in_progress || { cat "$OUT/replay.log" >&2; die "the replay failed without stopping on a commit"; }
		say "The replay stopped on: $(git log -1 --format='%h %s' REBASE_HEAD 2>/dev/null || echo '(unknown)')"
		say "Conflicting files:"
		git diff --name-only --diff-filter=U | sed 's/^/  /' >&2
	fi
}

# finish records the new upstream in the proof and folds in any amendments
# that were made after the replay.
finish() {
	recall
	cd "$WORK"
	in_progress && die "the replay isn't finished"
	[ "$(git rev-parse --abbrev-ref HEAD)" = candidate ] || die "expected to be on the branch \"candidate\""
	[ -z "$(git status --porcelain --untracked-files=no)" ] || die "there are uncommitted changes"
	git merge-base --is-ancestor "$NEW" HEAD || die "the branch \"candidate\" isn't based on $TARGET any more"

	# A fix of upstream's is still carried if its cherry-pick survived the
	# replay. The others are in the new upstream.
	local messages kept=() b
	messages=$(git log --format=%B "$NEW..HEAD")
	for b in $(backports_at "$FROM_HEAD"); do
		if grep -qF "(cherry picked from commit $b)" <<< "$messages"; then
			kept+=("$b")
		fi
	done

	printf '%s\n' "$NEW" > fork-proof/UPSTREAM_REF
	if [ -f fork-proof/UPSTREAM_BACKPORTS ]; then
		# A commit that's no longer carried goes from the list, along with
		# the comment directly above it, which is about it. The comment
		# that opens the file is about the file, and stays.
		KEPT="${kept[*]:-}" python3 - fork-proof/UPSTREAM_BACKPORTS <<'PY'
import os, sys
path = sys.argv[1]
kept = set(os.environ["KEPT"].split())
out, about = [], []
heading = True  # nothing but the comment that opens the file so far
for line in open(path).read().splitlines():
    first = line.split()[0] if line.split() else ""
    if first.startswith("#"):
        about.append(line)
        continue
    if not first:
        out += about + [line]
    elif first in kept:
        out += about + [line]
    elif heading:
        # The file's heading stays, even with a commit directly under it.
        out += about
    about = []
    heading = False
out += about
# Taking a block out can leave two blank lines where there was one.
tidy = [line for i, line in enumerate(out) if line.strip() or (i > 0 and out[i - 1].strip())]
open(path, "w").write("\n".join(tidy).rstrip("\n") + "\n")
PY
	fi
	if ! git diff --quiet -- fork-proof/UPSTREAM_REF fork-proof/UPSTREAM_BACKPORTS; then
		# These belong to the patch that adds the proof, so that's where
		# they go.
		local owner
		owner=$(git log -1 --format=%H "$NEW..HEAD" -- fork-proof/UPSTREAM_REF)
		[ -n "$owner" ] || die "no commit of the fork touches fork-proof/UPSTREAM_REF"
		git add fork-proof/UPSTREAM_REF fork-proof/UPSTREAM_BACKPORTS
		git commit --quiet --fixup="$owner"
	fi
	local subjects
	subjects=$(git log --format=%s "$NEW..HEAD")
	if grep -q '^fixup! ' <<< "$subjects"; then
		GIT_SEQUENCE_EDITOR=true GIT_EDITOR=true git rebase --interactive --autosquash --empty=drop "$NEW" > "$OUT/fold.log" 2>&1 ||
			{ in_progress && git rebase --abort; cat "$OUT/fold.log" >&2; die "folding the amendments into their patches failed"; }
	fi
}

# check runs the checks in order and stops at the first that fails, because
# each of the later ones takes the earlier ones for granted. It prints the
# names of what failed, and returns 1, if anything did.
check() {
	recall
	cd "$WORK"
	local name failed=""
	: > "$OUT/checks.txt"
	for name in $CHECKS; do
		say "Checking: $name"
		if check_"${name//-/_}" > "$OUT/check-$name.log" 2>&1; then
			echo "passed $name" >> "$OUT/checks.txt"
		else
			echo "FAILED $name" >> "$OUT/checks.txt"
			failed=$name
			say "  failed; the output is in $OUT/check-$name.log"
			break
		fi
	done
	[ -z "$failed" ] || { echo "$failed"; return 1; }
}

check_build() {
	go build ./... && (cd internal/backend/remote-state/gcs && go build ./...)
}

check_vet() {
	go vet ./... && (cd internal/backend/remote-state/gcs && go vet .)
}

check_proof_self() {
	(cd fork-proof && go vet . && go test .)
}

# Upstream's own tests, for every Go module in the repository. A package
# that fails is run again, and if it fails again it's run on upstream itself,
# so that a test of upstream's that fails without the fork isn't counted
# against the fork. Both are said in the output.
check_unit() {
	local dir out failed=() pkg rc=0
	out=$(mktemp)
	for dir in $(go list -m -f '{{.Dir}}' github.com/hashicorp/terraform/...); do
		(cd "$dir" && go test ./... 2>&1) | tee -a "$out" | grep -v -e '^ok ' -e 'no test files' || true
	done
	mapfile -t failed < <(awk '$1 == "FAIL" && $2 ~ /^github.com\// { print $2 }' "$out" | sort -u)
	rm -f "$out"
	[ ${#failed[@]} -ne 0 ] || return 0

	local upstream_tree=$OUT/upstream-tree
	for pkg in "${failed[@]}"; do
		echo "=== $pkg failed; running it again"
		if run_package "$WORK" "$pkg"; then
			echo "=== $pkg passed the second time: it has a test that doesn't always pass"
			continue
		fi
		if [ ! -d "$upstream_tree" ]; then
			git worktree add --quiet --detach "$upstream_tree" "$NEW"
		fi
		if run_package "$upstream_tree" "$pkg"; then
			echo "=== $pkg FAILS on the fork and passes on upstream at $TARGET"
			rc=1
		else
			echo "=== $pkg fails on upstream at $TARGET too, so this isn't the fork's doing"
		fi
	done
	[ ! -d "$upstream_tree" ] || git worktree remove --force "$upstream_tree"
	return $rc
}

# run_package runs the tests of one package in the given tree, which can be
# in any of the repository's modules.
run_package() {
	local tree=$1 pkg=$2 rel
	rel=${pkg#github.com/hashicorp/terraform}
	rel=${rel#/}
	(cd "$tree/${rel:-.}" && go test -count=1 .)
}

check_proof() {
	(cd fork-proof && go run . -work "$OUT/proof" -report "$OUT/proof-report.json" -jobs "$JOBS")
}

# agent gives Claude the state of things and what has to be true before it
# stops. Whether it got there is for the caller to find out.
agent() {
	local reason=$1 round=$2
	recall
	cd "$WORK"
	local prompt
	prompt=$(
		TARGET=$TARGET OLD=$OLD NEW=$NEW FROM_NAME=$FROM_NAME NOTES=$NOTES OUT=$OUT REASON=$reason \
			ROUND=$round MAX_ROUNDS=$MAX_ROUNDS CHECKS=$CHECKS \
			envsubst '$TARGET $OLD $NEW $FROM_NAME $NOTES $OUT $REASON $ROUND $MAX_ROUNDS $CHECKS' < "$HERE/agent-prompt.md"
	)
	# An empty credential isn't the same as none: it's taken as a
	# credential, and refused.
	[ -n "${ANTHROPIC_API_KEY:-}" ] || unset ANTHROPIC_API_KEY
	[ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ] || unset CLAUDE_CODE_OAUTH_TOKEN
	say "Round $round of $MAX_ROUNDS for Claude ($MODEL, effort $EFFORT, at most \$$BUDGET_USD): $reason"
	# CLAUDECODE is set inside a Claude Code session, which refuses to start
	# another one from within itself. This one is on its own, and it doesn't
	# take on the settings, plugins or hooks of whoever is running this: it
	# has to behave the same on a person's machine as in the workflow.
	env -u CLAUDECODE claude --print "$prompt" \
		--model "$MODEL" --effort "$EFFORT" \
		--dangerously-skip-permissions \
		--setting-sources local \
		--disallowedTools "WebFetch,WebSearch" \
		--max-budget-usd "$BUDGET_USD" \
		--no-session-persistence \
		--output-format json \
		< /dev/null > "$OUT/agent-$round.json" 2> "$OUT/agent-$round.err" || say "  Claude stopped with an error; see $OUT/agent-$round.err"
	say "  cost: \$$(json_field "$OUT/agent-$round.json" total_cost_usd), turns: $(json_field "$OUT/agent-$round.json" num_turns)"
}

# continue_ carries on from wherever things are until the replay is done and
# the checks pass, or until there's nothing more this script is allowed to try.
continue_() {
	recall
	cd "$WORK"
	local round=$ROUNDS reason failed problem said outcome=""
	while :; do
		if in_progress; then
			reason="the replay stopped on a commit that doesn't apply to $TARGET as it is"
		elif ! problem=$(finish 2>&1); then
			reason="the result can't be checked yet: ${problem#rebase.sh: }"
		elif failed=$(check); then
			outcome=passed
			break
		else
			reason="the check \"$failed\" fails"
		fi
		if ! have_agent; then
			outcome="stopped: $reason, and Claude isn't available to deal with it"
			break
		fi
		if [ "$round" -ge "$MAX_ROUNDS" ]; then
			outcome="stopped: $reason, after $round rounds of Claude's work"
			break
		fi
		round=$((round + 1))
		remember ROUNDS "$round"
		agent "$reason" "$round"
		if ! agent_worked "$round"; then
			said=$(agent_said "$round")
			outcome="stopped: $reason, and Claude couldn't start: $(awk 'NF { printf "%s%s", sep, $0; sep = " "; if (++n == 3) exit }' <<< "$said")"
			break
		fi
	done

	summary "$outcome"
	if [ "$outcome" = passed ]; then
		git bundle create --quiet "$OUT/candidate.bundle" "$NEW..candidate"
		say "Done. The result is the branch \"candidate\", and $OUT/candidate.bundle."
	else
		say "$outcome"
		return 1
	fi
}

# summary writes what a person needs to know about what happened.
summary() {
	local outcome=$1
	recall
	cd "$WORK"
	{
		echo "## Moving the fork to upstream $TARGET"
		echo
		echo "| | |"
		echo "|---|---|"
		echo "| From | \`$FROM_NAME\` at \`${FROM_HEAD:0:12}\`, on upstream \`${OLD:0:12}\` |"
		echo "| To | upstream \`$TARGET\` (\`${NEW:0:12}\`) |"
		echo "| Outcome | **$outcome** |"
		local cost=0 file round
		for file in "$OUT"/agent-*.json; do
			[ -f "$file" ] || continue
			cost=$(python3 -c 'import sys; print("%.2f" % (float(sys.argv[1]) + float(sys.argv[2])))' "$cost" "$(json_field "$file" total_cost_usd | sed 's/unknown/0/')")
		done
		echo "| Rounds of Claude's work | $ROUNDS$([ "$ROUNDS" = 0 ] || echo ", costing about \$$cost") |"
		if ! in_progress && git merge-base --is-ancestor "$NEW" candidate 2>/dev/null; then
			echo "| Commits on top of upstream | $(git rev-list --count "$NEW..candidate"), from $(git rev-list --count "$OLD..$FROM_HEAD") |"
		fi
		echo

		if [ -s "$OUT/checks.txt" ]; then
			echo "### Checks"
			echo
			sed 's/^/- /' "$OUT/checks.txt"
			if [ -f "$OUT/proof-report.json" ]; then
				echo "- the proof's verdict: \`$(json_field "$OUT/proof-report.json" verdict)\`"
			fi
			if [ -f "$OUT/check-unit.log" ] && grep -q '^=== ' "$OUT/check-unit.log"; then
				echo
				echo "Tests that failed at first:"
				echo
				grep '^=== ' "$OUT/check-unit.log" | sed 's/^=== /- /'
			fi
			echo
		fi

		if ! in_progress && git merge-base --is-ancestor "$NEW" candidate 2>/dev/null; then
			# A patch is gone if nothing with its subject survived.
			local dropped
			dropped=$(comm -23 \
				<(git log --format=%s "$OLD..$FROM_HEAD" | grep -v '^fixup! ' | sort -u) \
				<(git log --format=%s "$NEW..candidate" | sort -u))
			if [ -n "$dropped" ]; then
				echo "### Commits that didn't come along"
				echo
				echo "Either upstream has them now, or they were left out. The notes say which."
				echo
				sed 's/^/- /' <<< "$dropped"
				echo
			fi

			# What upstream changed underneath the fork: the files that
			# both upstream, between the two versions, and the fork touch.
			local under
			under=$(comm -12 \
				<(git diff --name-only --no-renames "$OLD" "$NEW" | sort) \
				<(git diff --name-only --no-renames "$NEW" candidate | sort))
			echo "### Files of the fork's that upstream changed between the two versions"
			echo
			if [ -n "$under" ]; then
				echo "A patch that still applies can be carrying what upstream used to do."
				echo "The proof is what tells, and these are where to look if it objects."
				echo
				sed 's/^/- `/; s/$/`/' <<< "$under"
			else
				echo "None."
			fi
			echo
		fi

		if [ -s "$NOTES" ]; then
			echo "### Claude's notes"
			echo
			cat "$NOTES"
			echo
		fi

		# How each round ended, in Claude's words, which is also where it
		# says so if it couldn't start at all.
		for file in "$OUT"/agent-*.json; do
			[ -f "$file" ] || continue
			round=${file##*/agent-}
			round=${round%.json}
			echo "### How round $round ended"
			echo
			agent_said "$round"
			echo
		done
	} > "$OUT/summary.md"
	say "The summary is in $OUT/summary.md"
}

case ${1:-} in
start)
	[ $# -eq 2 ] || die "usage: rebase.sh start TARGET"
	start "$2"
	;;
continue)
	continue_
	;;
run)
	[ $# -eq 2 ] || die "usage: rebase.sh run TARGET"
	start "$2"
	continue_
	;;
check)
	check
	;;
*)
	sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d; s/^# \{0,1\}//' >&2
	exit 2
	;;
esac
