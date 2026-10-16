# Terraform, large-state performance fork

This repository is an unofficial fork of
[hashicorp/terraform](https://github.com/hashicorp/terraform) that makes
`terraform plan` and `terraform apply` much faster, and much less
memory-hungry, when the state and the configuration are very large. It does
exactly what upstream does, and can show it.

This branch isn't Terraform. It holds the one thing that isn't tied to a
version of upstream: the workflow that moves the fork to a new one.

| What | Where |
|---|---|
| The fork itself | The branches named `perf/v<version>`, one for each version of upstream that the fork has been carried to. The newest is the one to use. |
| What it changes, why, and every way it differs from upstream | `README-FORK.md` on those branches |
| Images | `ghcr.io/cozybear-dev/terraform:perf-v<version>`, built for `linux/amd64` and `linux/arm64` and published only for a commit that passes upstream's tests and the proof |
| The proof that it does what upstream does | `fork-proof/` on those branches |
| `main` | An unmodified copy of upstream's `main` |

## Moving the fork to a new version of upstream

When upstream releases a version that you want the fork on:

1. Open the **Actions** tab and choose **Fork rebase** on the left.
2. Press **Run workflow**, type upstream's tag, for example `v1.17.0`, and
   press the green button.

That's all there is to starting it. About half an hour later, if it worked,
there is a new branch `perf/v1.17.0` and, about half an hour after that, an
image `ghcr.io/cozybear-dev/terraform:perf-v1.17.0`. It takes longer, an
hour or two, when something needs judgement. The run's summary page says
what happened in between.

Tick **Trial run** to have it do everything except push, which is the way to
find out how a move would go without anything changing.

### What it does

The fork is a version of upstream with the fork's own commits on top, about
thirty of them, one for each area it changes. Those commits are the
performance work, and they are on the `perf/v<version>` branches, not on
this one. Moving the fork means taking the new version of upstream and
putting those same commits on top of that instead:

```
what it starts from:  upstream v1.17.0-rc1 ─ the fork's commits            = perf/v1.17.0-rc1
what it makes:        upstream v1.17.0     ─ the same commits, replayed    = perf/v1.17.0
```

What it starts from is the fork's newest `perf/v<version>` branch, which it
finds by itself. The file `fork-proof/UPSTREAM_REF` on that branch says
which commit of upstream the branch is on, and every commit after that one
is the fork's. The branch it starts from is left as it was, and the branch
it makes is what the next move starts from. To see the fork's commits on
their own, compare a branch with the tag it's named for:
`https://github.com/cozybear-dev/terraform/compare/v1.17.0-rc1...perf/v1.17.0-rc1`.

1. **Replay.** A script replays the fork's commits, in order, onto the new
   tag. Most of the time each one applies as it is.
2. **Check.** It builds the result, runs upstream's own tests on it, and
   runs the proof, which builds upstream at the new tag and compares what
   the two do.
3. **Judgement, where it's needed.** If a commit doesn't apply, because
   upstream changed the same code, or if a check fails, Claude (Opus 5.5)
   works out what upstream changed and what the fork's commit should now
   be, and the checks run again. It gets three rounds and a spending limit.
   If nothing needs judgement, Claude isn't called at all.
4. **Guard.** A second job, with no model in it, compares the result with
   the branch it came from and refuses it if anything that checks the fork
   was touched: the proof, the workflows, upstream's tests.
5. **Push.** The result is pushed as the branch `perf/<tag>`.
6. **Publish.** The push starts the **Fork image** workflow on the new
   branch, which runs upstream's tests, the race detector and the proof
   once more and publishes the image only if all of them pass.

### Why the result can be trusted

Claude decides nothing about whether the result is right. Two things do, and
both give the same answer whoever runs them:

- **The checks**: upstream's own test suite, unchanged, and the proof, which
  compares the fork with upstream itself over a fixed set of scenarios and
  generated inputs. A fork commit that was carried over wrongly makes the
  fork behave differently from the new upstream, and that's what these
  detect.
- **The guard**, which makes sure those are still the checks they were. The
  job Claude works in can't push, and holds no secret but Claude's own. What
  it produces is only accepted if the proof, its list of files the fork may
  change, the two workflows and every test of upstream's are byte for byte
  what they were or what upstream has. Changing any of those is for a
  person to do, on the fork's branch, where it can be seen.

Where Claude isn't sure that a commit can be carried over correctly, it's
told to leave that commit out and say so. The fork without one of its
optimisations is slower and still correct.

### When it stops

The summary page of the run says which of these it was.

| What it says | What to do |
|---|---|
| Claude isn't available | Something needed judgement and neither secret for Claude is set. Set one (see below) and run it again. |
| Claude couldn't start | Its credential was refused, or the service couldn't be reached, and the summary quotes the error. Nothing was spent. Put the secret right (see below) and run it again. |
| Stopped after three rounds of Claude's work | It couldn't get the checks to pass. The summary has its notes and the artifact has the logs. Run it again with a higher limit, or finish it by hand (see below). |
| The guard rejected it | The move needs a change that only a person makes, such as adding a file to `fork-proof/FORK_FILES`. The log says which. Make the change on the fork's current branch and run it again. |
| The branch already exists | There has already been a move to that tag. A fix to it goes on top of it, as a new commit. To start again, when nothing was published from it, the branch has to go first: see below. |
| The branch has changed since this run started | Something was pushed to the fork's branch while the move was running, and the result doesn't have it. Run it again. |
| Pushed, and then **Fork image** failed | The branch is there and no image was published. Push a fix to that branch, which runs the checks again. |

A rule of this repository refuses a force-push to this branch or to any
`perf/v<version>` branch, and refuses deleting one, because other people may
be building from them. So a branch from a failed attempt can't simply be
deleted. An owner of the repository can turn the rule off for as long as it
takes, under **Settings → Rules → Rulesets**, delete the branch, and turn
the rule on again.

### What it needs

Two secrets, under **Settings → Secrets and variables → Actions**:

| Secret | What for |
|---|---|
| `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` | Claude. The first is an API key from [console.anthropic.com](https://console.anthropic.com), billed for what's used. The second comes from running `claude setup-token` and uses a Claude subscription. Set one and not both: each kind is refused under the other's name, and the API key is the one that's used if both are there. Without either, the workflow still works whenever nothing needs judgement. |
| `FORK_DEPLOY_KEY` | The private half of a deploy key of this repository that's allowed to write, which is what pushes the new branch. The workflow's own token can't push a branch that has workflow files in it. |

Give the API key a spending limit. The job it's used in runs code from
upstream's repository, as any build of it does.

### Doing it by hand

The same script runs anywhere that has git, Go and a checkout of the fork:

```sh
git clone --branch fork https://github.com/cozybear-dev/terraform fork-control
git clone --branch perf/v1.17.0-rc1 https://github.com/cozybear-dev/terraform work
cd work
../fork-control/rebase/rebase.sh run v1.17.0
```

It leaves the result on a local branch called `candidate` and a summary in
`.fork-rebase/summary.md`, and pushes nothing. With the `claude` command
installed and `AGENT=on`, it calls Claude as the workflow does. Without it,
it stops where judgement is needed: resolve the commit it stopped on,
`git rebase --continue`, and run `../fork-control/rebase/rebase.sh continue`.
`../fork-control/rebase/guard.sh perf/v1.17.0-rc1 candidate v1.17.0` is the
check the workflow makes before it pushes.

## What's on this branch

| File | What it is |
|---|---|
| `.github/workflows/fork-rebase.yml` | The workflow |
| `rebase/rebase.sh` | Replays, checks, and calls Claude |
| `rebase/agent-prompt.md` | What Claude is told |
| `rebase/guard.sh` | Decides whether a result may be pushed |
| `rebase/newest.py` | Finds the fork's newest branch |
| `.github/workflows/fork-bench.yml` | **Fork benchmark**: times `terraform show` on a branch of the fork and on its upstream, on generated Vault and Google configurations, and checks they print the same |
| `bench/show.sh` | What it runs |
| `bench/gen4.py`, `bench/gcpmock.py` | The configurations it generates, and the stand-in for Google's APIs that their objects are created in |
