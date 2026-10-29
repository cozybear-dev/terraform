#!/usr/bin/env bash
# Makes a branch with some of the fork's patches, to measure with the apply
# benchmark:
#
#   subset.sh LABEL N...
#
# The branch dev/apply-LABEL starts at upstream v1.17.0-rc1 with upstream
# 3c8cba7d99 cherry-picked (BASE), and gets patches N... of
# origin/perf/v1.17.0-rc1 cherry-picked in the order of the stack. Patch N is
# the Nth commit after BASE that isn't a fixup, as in the table in
# README-FORK.md.
#
# It runs in the git worktree WT (default ~/tfperf/stack/wt), with rerere on,
# so that a conflict resolved by hand once is resolved the same way the next
# time. If a conflict can't be resolved that way it stops in the middle of
# the cherry-pick: resolve it, "git cherry-pick --continue", and run it
# again. Then it prints the changed lines and pushes the branch, unless
# NOPUSH=1.
set -euo pipefail
[ $# -ge 2 ] || { echo "usage: subset.sh LABEL N..." >&2; exit 2; }
label=$1
shift
BASE=38bf979f34498a36dbfb2db2786c6ade988182fd
WT=${WT:-$HOME/tfperf/stack/wt}
nocred=(-c credential.helper= -c 'credential.helper=!gh auth git-credential')
ident=(-c user.name=cozybear-dev -c user.email=mjvogel99@gmail.com)
cd "$WT"
git config rerere.enabled true
git config rerere.autoupdate true
git "${nocred[@]}" fetch --quiet origin perf/v1.17.0-rc1
mapfile -t patches < <(git log --reverse --format=%H --invert-grep --grep='^fixup!' "$BASE..origin/perf/v1.17.0-rc1")
[ "${#patches[@]}" -ge 33 ] || { echo "expected at least 33 patches, found ${#patches[@]}" >&2; exit 1; }

git switch --quiet --discard-changes -C "dev/apply-$label" "$BASE"
for n in $(printf '%s\n' "$@" | sort -n -u); do
	commit=${patches[$((n - 1))]}
	echo "patch $n: $(git log -1 --format=%s "$commit")"
	if ! git "${ident[@]}" cherry-pick "$commit" > /dev/null 2>&1; then
		if [ -z "$(git diff --name-only --diff-filter=U)" ]; then
			# rerere resolved it as before.
			git "${ident[@]}" -c core.editor=true cherry-pick --continue > /dev/null
		else
			echo "patch $n conflicts in:" >&2
			git diff --name-only --diff-filter=U >&2
			exit 3
		fi
	fi
done
lines=$(git diff --numstat "$BASE" HEAD -- . ':!*_test.go' ':!fork-proof' ':!*.md' ':!.github' \
	':!internal/provider-bench' ':!**/testdata/**' ':!Dockerfile*' | awk '{ n += $1 + $2 } END { print n + 0 }')
echo "dev/apply-$label: $lines changed lines, at $(git rev-parse --short HEAD)"
if [ "${NOPUSH:-}" != 1 ]; then
	git "${nocred[@]}" push --quiet --force origin "dev/apply-$label"
fi
