# A branch of the fork for applying saved plans

The most `terraform apply <plan file>` performance for the fewest changed
lines of Terraform's own code.

Written 2026-10-29, from a conversation with the fork's owner. What the owner
said is marked as theirs; everything else is a call made while writing this,
and is marked as such where it isn't obvious.

## Why

The owner plans with the full fork (`perf/v1.17.0-rc1`, 33 patches, about
10,000 changed lines of Terraform code in 99 files, not counting tests), and
applies the saved plan file. They want a separate branch for the apply that
carries only what makes the apply fast, so that the changes are few enough
to read and understand before trusting them with real state. In their words:
"it's just about the fewest LOC changes to get the most performance at
apply".

Their setup (theirs):

- About 400 MB of state, with complex dependencies, in a GCS backend.
- Mostly the `hashicorp/vault` and `hashicorp/google` providers.
- A typical apply changes a few hundred resources; the range is a few tens
  to about 1,500.
- Applies run on a MacBook Air M4 with 20 GB of memory, in a Docker
  container (so the `linux/arm64` image, in Docker Desktop's VM).
- `-parallelism` is set as high as memory allows. Memory therefore limits
  the speed of the walk directly: a build that needs less memory per
  in-flight resource can run with more of them at once.
- Upload speed to Google Cloud is fine.
- No real numbers from a stock apply are available yet.
- What hurts today: memory use so high that the machine pages, and the
  graph walk.
- Plans will keep coming from the full fork; only the apply switches to the
  new build.

Earlier the same day the owner said that unexpected apply changes are not
acceptable, and on 2026-10-13 that data integrity is critical.

### The cause, as far as the code shows

In upstream v1.17.0-rc1, after every resource instance is applied,
`updateStateHook` calls `StateHook.PostStateUpdate` with the walk's state
locked. With a remote backend that calls `remote.State.WriteState`, which
deep-copies the entire state. Every 20 seconds it also calls `PersistState`,
which serializes both the current and the last-read state in full to compare
them (`statefile.StatesMarshalEqual`), serializes once more, and uploads, all
still under that lock. A 300-change apply on a 400 MB state is therefore
about 300 full copies of the state, plus the periodic serialization and
upload, with every other part of the walk waiting. That fits both symptoms.
It's a reading of the code, not yet a measurement of the owner's apply.

## Goals

1. Apply time and peak memory, on the benchmark below, as close to the full
   fork's as the owner chooses to pay for in changed lines.
2. As few changed lines of Terraform's own code as possible. Lines are
   counted in non-test Go files of upstream's tree; the proof, workflows,
   Dockerfile, README and tests don't count (they're still on the branch).
3. Each change readable on its own: one commit per area, as in the full
   fork's stack, each saying what it does and why.
4. The same bar for integrity as the full fork: the proof's verdict is
   `identical`, unit and race tests pass, no upstream test file is changed,
   and nothing is published otherwise.
5. Plan files written by `perf/v1.17.0-rc1` are applied by the new build
   with the same result as stock v1.17.0-rc1 applying the same file.

Success is the owner picking a point on a measured table of "changed lines"
against "apply time and peak memory", and that point being published as the
branch.

## Non-goals

- Making plan, show, init, validate or rendering faster. Patches that only
  help those stay out (for example 16 rendering, 28 skipping validation,
  29 GCS modules, 2 profiling hooks, 3 the benchmark provider).
- New optimisations that the full fork doesn't already have. If the
  measurements show a large cost that no existing patch removes, that comes
  back to the owner rather than being written.
- Any change to `perf/v1.17.0-rc1`, which keeps being the plan build.
- Moving the apply branch to newer upstream versions automatically. It is
  laid out as a stack like the full fork, so the same method can be applied
  later, but the rebase workflow isn't extended now.

## Anti-goals

- A state, after applying a full-fork plan, that differs from what stock
  v1.17.0-rc1 writes when applying the same plan file, other than the
  differences already documented for the patches kept (for example
  difference 2, a lower `serial`).
- A patch kept for a gain that is small compared to how hard it is to read.
- Any upstream test changed to make a patch pass.

## Constraints

- Nothing is built, tested or benchmarked on the owner's machine. All of it
  runs in GitHub Actions on `cozybear-dev/terraform` (public, so minutes are
  free); only small results are downloaded.
- Published branches are never rewritten or deleted. Changes after
  publishing are `--fixup` commits on top.
- Anything that can change what is written to state, plan files or remote
  state needs a differential test against upstream and the proof.

## Decided

### What the branch is made of

- Upstream `v1.17.0-rc1`, then upstream `3c8cba7d99` cherry-picked with
  `-x` (the proof needs it, as on the full fork), then a subset of the full
  fork's patches in their stack order, then the patches for the README,
  version line, workflows and proof.
- Existing patches only. Where most of a patch doesn't help the apply, only
  the part that does is kept, as long as that part builds, passes `go vet`
  and its tests, and still reads as one change.
- Patch 1 (`parallel.For`, 126 lines) is in if anything that needs it is.
- Patch 30, the `terraform version` line (20 lines), is kept so the output
  says which build applied (my call).

### How the cut is found

Measured in Actions, by a new workflow beside the existing "Fork benchmark"
on the `fork` branch, reusing `bench/gen4.py`, `bench/gcpmock.py` and the
Vault dev server setup.

- State in a GCS emulator (`fsouza/fake-gcs-server`) through a `backend
  "gcs"` block, as the owner's is.
- State of about 400 MB with dependency lists between resources, Vault and
  Google resources only. `gen4.py` with 56,000 instances gives 93 MB, so it
  has to grow.
- The configuration has many module calls, each with a module directory of
  its own, as the owner's bucket modules have (`gen4.py` with `COPIES=1`, as
  in the `module-copies` scenario). Their real plan spent about 120 s
  building the graph, so the shape of the configuration matters, not only
  the size of the state (my call, from earlier notes).
- Three plans, all saved by a build of `perf/v1.17.0-rc1`, changing about
  50, 300 and 1,500 resources. 300 is the main case. Most changes are
  in-place updates, with some creates, destroys and replacements, so that
  each kind of apply node is in the graph (my call).
- For each build: wall time, user and system CPU, and peak resident memory
  of `terraform apply <plan file>`, several runs each, with stock
  v1.17.0-rc1 and the full fork as the two reference points.
- Every measured apply starts from the same state in the bucket and asks
  the providers to do the same work.
- The result is one table: candidate builds in order of patches added, each
  row with its total changed lines and its time and memory on the three
  plans. Patches are added in order of gain per changed line, a patch's
  prerequisites counted with it (patch 9 needs 4, 5 and 6).
- Memory comes first in that order, because paging is what hurts: patches
  are added by peak memory saved per line until the 300-change apply is
  under the memory target, then by time saved per line (my call). GitHub's
  runners have 16 GB and won't page on their own, so the applies also run
  under a memory limit like the owner's, which makes paging show up as time.
- The memory limit is 10 GB, my guess at what Docker Desktop's VM gets on a
  20 GB Mac (its default is a share of the host's memory). The owner can
  read the real figure in Docker Desktop under Settings, Resources, and the
  limit is one input of the workflow.
- Each build is measured at the default `-parallelism=10` and at a high
  setting, because the owner raises it as far as memory allows. The Google
  mock and Vault answer in about a millisecond, which would hide what
  parallelism buys, so the mock adds a delay to each request like a real
  API's (my call; the builder picks the figures).
- The runs are on GitHub's `linux/arm64` runners, the architecture of the
  image the owner runs (my call).
- `TF_STATE_PERSIST_INTERVAL` and `TF_GCS_STATE_GZIP` are left unset, as the
  owner didn't mention setting them (my reading). The upload is not slowed
  down, since the owner's upload speed is fine.

Candidates (my reading of what the apply touches, to be confirmed by
measurement; lines are non-test lines added + removed):

| Patch | What | Lines | Why it may matter for apply |
|---|---|---|---|
| 18 | Save snapshots of a large state in the background | 393 | Removes the per-instance write under the lock |
| 9 (+4, 5, 6) | Keep less for each object of a state in memory | 1,113 (+1,136) | Copies of the state share unchanged objects |
| 12 | Don't retrieve or serialize an unchanged remote state again | 90 | Removes the full serialize-to-compare |
| 13 | GCS: fewer, larger requests | 300 | Download at start, upload at end and every 20 s |
| 10 | Read and write state files on all processors | 2,439 | Decode and encode of 400 MB |
| 15 | Read the parts of a plan file concurrently | 982 | Also decodes no-op changes only if referenced |
| 14 | Index planned changes | 654 | Lookups of changes during the walk |
| 7, 17 | Graph algorithms, graph building | 639, 339 | Apply graph build |
| 8 | Goroutine per vertex only when ready | 383 | Memory and GC during the walk |
| 26 | Don't search the whole state per resource | 45 | Lock held during that search |
| 25, 24, 22, 19 | Walk-time evaluation | 108, 90, 196, 61 | Expression evaluation during the walk |
| 27 | Release memory between stages | 41 | Peak memory |
| 33 | Find files of the plan's configuration by directory | 58 | Loading configuration from the plan |

Checked with `git merge-tree` on top of upstream plus patch 1: every
candidate applies textually on its own except 18 and 27 (a conflict each in
`internal/backend/local/backend_apply.go`) and 22 (in
`internal/terraform/evaluate.go`). Textual application says nothing about
whether it compiles.

### Mixed builds

Goal 5 needs a check the proof doesn't make today: the proof plans and
applies with the same binary. The branch needs, in its proof or in a test
that runs on every push, plans saved by the full fork applied by the new
build and by stock v1.17.0-rc1 from the same starting state, with the
resulting states compared by the proof's `compare state` rules.

The full fork keeps getting fixups, so that check plans with the current
head of `perf/v1.17.0-rc1` each time it runs, and records which commit that
was. The branch's README names the full-fork commit and image digest it was
last checked against, and advises planning with the full fork's image by
its `-<sha>` tag rather than the moving branch tag (my call).

### Name and publishing

- Branch `perf-apply/v1.17.0-rc1` (my call). A name under `perf/v*` would
  be read by `rebase/newest.py` as a newer pre-release than `perf/v1.17.0-rc1`
  (`rc1-apply` sorts after `rc1`), and the rebase workflow would start from it.
- Its own `fork-image.yml` also runs on `perf-apply/v*`, and publishes
  `ghcr.io/cozybear-dev/terraform:perf-apply-v1.17.0-rc1`.
- The repository ruleset that refuses force-pushes and deletion (id
  25032998) covers `fork` and `perf/v*`. Adding `perf-apply/v*` is a change
  to the repository's settings and is asked of the owner before it is made.

### README

The branch's `README-FORK.md` describes this build only: what it is for
(applying plans made by `perf/v1.17.0-rc1`), the patches it keeps with their
changed lines, the measured table, and only the behaviour differences of the
patches it keeps. If patch 18 is kept, difference 1 (intermediate snapshots
during apply) is described for a GCS backend specifically. Upstream uploads
an intermediate snapshot only every 20 seconds, and the fork keeps that
schedule, so after a hard stop the state in the bucket should be about as
far behind as upstream's. That is checked by killing applies with `kill -9`
against the emulator, not assumed.

## Left to the builder

- How `gen4.py` reaches about 400 MB: more instances, bigger objects, more
  dependencies, or a mix, as long as it stays Vault and Google.
- How each apply is reset to the same starting point, including the
  objects in Vault and the mock.
- The number of runs per build, and how the jobs are split across runners.
- Which hunks of a large patch are dropped, and how the proof's
  `FORK_FILES` and components are cut down to this branch.
- What to do if upstream doesn't fit in a runner's memory on the 400 MB
  state (record that it didn't fit, rather than shrinking the state).
