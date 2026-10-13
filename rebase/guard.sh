#!/usr/bin/env bash
# Decides whether a rebased fork may be pushed.
#
#   guard.sh FROM CANDIDATE TARGET
#
# FROM is the fork's branch that was moved, CANDIDATE the result, and TARGET
# the commit of upstream's that the result is meant to be based on. All three
# are revisions in the repository this is run in, and TARGET has to have been
# fetched from upstream by whoever runs this, not taken from whatever made
# the candidate.
#
# The candidate was made partly by a language model, with a shell. This
# doesn't judge whether it's right. Upstream's tests and the proof do that,
# once it's pushed. This makes sure that they are still the tests and the
# proof that were there before, and that nothing was added which runs on its
# own: the candidate is rejected if it differs from FROM in anything that
# defines the checks, or from upstream in anything the fork hasn't declared.
#
# It prints each reason for rejecting, and exits with status 1 if there is one.
set -euo pipefail

[ $# -eq 3 ] || { echo "usage: guard.sh FROM CANDIDATE TARGET" >&2; exit 2; }
from=$(git rev-parse --verify "$1^{commit}")
cand=$(git rev-parse --verify "$2^{commit}")
target=$(git rev-parse --verify "$3^{commit}")

rejected=0
reject() {
	echo "REJECTED: $*"
	rejected=1
}

# The files that say what the fork is compared with. The script that makes
# the candidate sets these, and what they may say is checked below.
is_reference_file() {
	[ "$1" = fork-proof/UPSTREAM_REF ] || [ "$1" = fork-proof/UPSTREAM_BACKPORTS ]
}

backports_at() {
	git show "$1:fork-proof/UPSTREAM_BACKPORTS" 2>/dev/null | grep -v '^[[:space:]]*#' | awk 'NF { print $1 }' || true
}

# 1. It's built on the upstream it claims.
if ! git merge-base --is-ancestor "$target" "$cand"; then
	reject "the candidate isn't based on the target commit $target"
fi
ref=$(git show "$cand:fork-proof/UPSTREAM_REF" 2>/dev/null | tr -d '[:space:]')
if [ "$ref" != "$target" ]; then
	reject "fork-proof/UPSTREAM_REF says ${ref:-nothing}, and the target is $target"
fi

# 2. The proof, the workflows that run it and publish, the image's build
# file, and the document that says how the fork differs from upstream are
# exactly what they were.
protected=(fork-proof .github/workflows/fork-image.yml .github/workflows/fork-proof.yml Dockerfile.fork Dockerfile.fork.dockerignore README-FORK.md)
while IFS= read -r file; do
	[ -n "$file" ] || continue
	is_reference_file "$file" && continue
	reject "$file differs from the branch that was moved. It's part of what checks the fork, and only a person changes that."
done < <(git diff --name-only --no-renames "$from" "$cand" -- "${protected[@]}")

# 3. Nothing else is in the directory that GitHub runs things from.
while IFS= read -r file; do
	[ -n "$file" ] || continue
	case $file in
	.github/workflows/fork-image.yml | .github/workflows/fork-proof.yml) ;;
	*) reject "$file differs from upstream's. Nothing under .github is the fork's but its two workflows." ;;
	esac
done < <(git diff --name-only --no-renames "$target" "$cand" -- .github)

# 4. The fixes of upstream's that it carries are ones the branch already
# carried, they come first, and each is upstream's change exactly.
mapfile -t from_backports < <(backports_at "$from")
mapfile -t cand_backports < <(backports_at "$cand")
mapfile -t commits < <(git rev-list --reverse "$target..$cand")
base=$target
for i in "${!cand_backports[@]}"; do
	b=${cand_backports[$i]}
	if [[ " ${from_backports[*]:-} " != *" $b "* ]]; then
		reject "fork-proof/UPSTREAM_BACKPORTS lists $b, which the branch that was moved didn't. Adding one is for a person to do."
		continue
	fi
	c=${commits[$i]:-}
	message=$([ -z "$c" ] || git log -1 --format=%B "$c")
	if ! grep -qF "(cherry picked from commit $b)" <<< "$message"; then
		reject "commit number $((i + 1)) after upstream should be the cherry-pick of $b, and isn't"
		continue
	fi
	if ! git cat-file -e "$b^{commit}" 2>/dev/null; then
		reject "the upstream commit $b isn't in this repository, so the cherry-pick of it can't be checked"
		continue
	fi
	# Applying upstream's change to what the cherry-pick was made on has to
	# give exactly what the cherry-pick gave.
	index=$(mktemp)
	rm -f "$index"
	if tree=$(
		export GIT_INDEX_FILE=$index
		git read-tree "$c^" &&
			git diff-tree -p --binary --full-index --no-renames "$b^" "$b" | git apply --cached --whitespace=nowarn - &&
			git write-tree
	) 2>/dev/null && [ "$tree" = "$(git rev-parse "$c^{tree}")" ]; then
		base=$c
	else
		reject "commit $c says it's upstream's $b and isn't the same change"
	fi
	rm -f "$index"
done

# 5. Everything else it changes is in a file the fork has declared, and
# isn't a test of upstream's. The proof checks both of these again when it
# runs. Here they're checked against the list as it was before the move.
declared=$(git show "$from:fork-proof/FORK_FILES" | grep -v '^[[:space:]]*#' | awk 'NF { print $1 }')
is_declared() {
	local file=$1 dir
	grep -qxF "$file" <<< "$declared" && return 0
	dir=$(dirname "$file")
	while [ "$dir" != . ]; do
		grep -qxF "$dir/" <<< "$declared" && return 0
		dir=$(dirname "$dir")
	done
	return 1
}
while IFS=$'\t' read -r status file; do
	[ -n "$file" ] || continue
	if ! is_declared "$file"; then
		reject "$file differs from upstream and isn't in fork-proof/FORK_FILES"
	fi
	if [ "$status" != A ] && { [[ $file == *_test.go ]] || [[ /$file == */testdata/* ]]; }; then
		reject "$file is a test of upstream's, or data for one, and the candidate changes it"
	fi
done < <(git diff --name-status --no-renames "$base" "$cand")

if [ "$rejected" -ne 0 ]; then
	exit 1
fi
echo "The candidate differs from upstream only in what the fork declares, and from"
echo "the branch that was moved in nothing that checks it."
