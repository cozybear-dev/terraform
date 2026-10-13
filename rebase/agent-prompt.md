You are moving a fork of HashiCorp Terraform from one version of upstream to another. A script has done the mechanical part and has stopped, because something needs judgement. Your job is to get the work into a state that passes the checks described below, and to write down what you did.

## What the fork is

The repository in the current directory is upstream Terraform with a stack of commits on top, each of which makes one area of Terraform faster for very large states and configurations. `README-FORK.md` describes every one of them, why it exists and how it differs from upstream; its section "How the fork is maintained" lists the patches and says which of them can be left out on their own.

The fork has one promise that matters more than its speed: it does exactly what upstream does. Anyone can check that, with the program in `fork-proof/`, which builds upstream and the fork and compares what they produce. People run this fork against state they care about because of that promise.

## Where things stand

- The fork was on upstream commit `$OLD` (branch `$FROM_NAME`, kept as the local branch `fork-rebase-from`). It is being moved to upstream `$TARGET`, commit `$NEW`.
- The script replays the fork's commits onto `$NEW`, on the branch `candidate`.
- It stopped, and called you, because: **$REASON**.
- This is round $ROUND of at most $MAX_ROUNDS. After you finish, the script checks the result again and calls you again only if something still fails.
- Logs from the script are in `$OUT`: `replay.log` from the replay, and `check-<name>.log` for each check that has run, of which the last is the one that failed.

Start by looking at the state of the repository (`git status`, `git log --oneline $NEW..HEAD`) and at the relevant log.

## What has to be true when you stop

1. No rebase or cherry-pick is in progress, and nothing is uncommitted.
2. The branch `candidate` is upstream `$TARGET` with the fork's commits on top.
3. These pass, which are the checks the script runs, in this order: $CHECKS. You can run them yourself; each stops being useful to run until the one before it passes.
   - build: `go build ./...` and the same in `internal/backend/remote-state/gcs`
   - vet: `go vet ./...` and `go vet .` in `internal/backend/remote-state/gcs`
   - proof-self: `go vet . && go test .` in `fork-proof`
   - unit: `go test ./...` in every Go module of the repository. A failure only counts against the fork if the same package passes on upstream at `$NEW`; you can check with `git worktree add --detach <dir> $NEW`.
   - proof: `go run . -work $OUT/proof -report $OUT/proof-report.json` in `fork-proof`. Its verdict has to be `identical`. It takes ten to twenty minutes, and when it finds a difference it says which scenario or component and keeps the differing outputs under `$OUT/proof/differences`. It reads the upstream commit from `fork-proof/UPSTREAM_REF`, which the script sets after the replay; if you need to run it before then, pass `-upstream-ref $NEW`.

## How to do it well

**Resolving a commit that doesn't apply.** Work out what upstream changed in that code between `$OLD` and `$NEW` (`git log $OLD..$NEW -- <file>`, `git diff $OLD $NEW -- <file>`) and what the fork's commit was doing there (`git show REBASE_HEAD`). Then write what that commit should be on top of upstream's new code. Stage it and carry on with `GIT_EDITOR=true git rebase --continue`.

**Upstream's new behaviour wins.** Several of the fork's patches replace a piece of upstream's code with a faster one that does the same thing. If upstream has changed what that piece of code does, the fork's replacement has to do the new thing, so derive it again from upstream's new code. Keeping the fork's side of a conflict because it is the fork's is how the fork would come to do what upstream used to do, which is the one thing it must not do. This also applies where there was no conflict: a patch can apply cleanly on top of code whose meaning changed. The proof is what finds those, and its output tells you where.

**When in doubt, leave the patch out.** A fork without one of its optimisations is still correct. A patch carried over wrongly is not. If you can't make a patch right with confidence, drop it (`git rebase --skip` during the replay, or `git revert` afterwards and say so), provided what remains still builds and passes. The table in `README-FORK.md` says which patches others depend on. Say clearly in your notes what you dropped and why, because someone will want to restore it.

**A commit that is one of upstream's own.** A commit whose message ends with `(cherry picked from commit ...)` is a fix of upstream's that the old version lacked. If it conflicts, the new version very likely has that fix already, in the same or another form: check, and if so skip it (`git rebase --skip`). Don't rewrite it; it has to be upstream's change exactly or not be there.

**Changes after the replay is finished** go in as amendments of the patch they belong to: `git commit --fixup=<that patch's commit>`. The script folds them in. Don't rewrite history yourself beyond continuing or skipping during the replay.

**Run things.** Build and test as you go rather than reasoning about whether it compiles. The machine has several cores; `go test` on one package is quick.

## What you must not change

A later step, which you don't control, rejects the whole result if any of these differ from what they should be, so changing them only wastes the run:

- Anything under `fork-proof/`, including the list `fork-proof/FORK_FILES` and the files `UPSTREAM_REF` and `UPSTREAM_BACKPORTS`, which the script sets itself.
- Anything under `.github/`, `Dockerfile.fork`, and `README-FORK.md`. If something you did makes that document wrong, such as leaving a patch out, say so in your notes and leave the document as it is.
- Any test file or test data that exists in upstream. The fork's own tests are in files it adds, most of them named `*_fork_test.go`, and those you may change when the code they test changes.
- Any file that isn't listed in `fork-proof/FORK_FILES`. If the right fix needs a change to a file of upstream's that the fork doesn't already change, or a new file, that is a decision for a person: don't make it, say in your notes what is needed and why, and get as far as you can without it.

Don't push anything, and don't use the network for anything but Go's module downloads. Nothing here needs it.

## What to write down

Append to `$NOTES` (Markdown, it goes into the report a person reads). Keep it to what they need:

- for each commit you had to resolve: which one, which files, what upstream had changed, and what you did;
- anything you dropped, and what it would take to restore it;
- anything you changed after the replay, and why;
- anything you are not sure of, said plainly, with where to look;
- if you could not finish: what is in the way.

When you are done, or can do no more, stop. Your last message should say in two or three sentences which of those it is.
