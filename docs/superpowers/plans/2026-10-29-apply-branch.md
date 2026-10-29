# Apply branch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish `perf-apply/v1.17.0-rc1`, the subset of the full fork's patches that gives the most `terraform apply <plan file>` speed and memory for the fewest changed lines, chosen by measurement.

**Architecture:** An Actions workflow on the `fork` branch measures `apply` of plans saved by the full fork, with any list of builds, against a ~400 MB state in a GCS emulator with Vault and a Google mock, under a 10 GB memory limit. Candidate builds are subsets of the stack, made locally with `git cherry-pick` and pushed as `dev/apply-*` branches. Three rounds of measurement give a table, the owner picks the cut, and that subset becomes the branch, with its own README, image workflow and a proof that also applies full-fork plans.

**Tech Stack:** Go (Terraform), bash, Python 3 (stdlib only), GitHub Actions on `ubuntu-24.04-arm`, Docker (`hashicorp/vault`, `fsouza/fake-gcs-server`), cgroup v2, `tc netem`.

**Spec:** `docs/superpowers/specs/2026-10-29-apply-branch-design.md` (on branch `fork`, worktree `~/tfperf/forkctl`).

## Global Constraints

- Nothing is built, tested or benchmarked with Go or Terraform on the local machine. Those run in GitHub Actions on `cozybear-dev/terraform`. Locally: git, reading, Python stdlib scripts on tiny inputs, and analysis of downloaded results.
- `perf/v1.17.0-rc1` is not changed. `fork` and `perf/v*` are never force-pushed or deleted.
- Commits use `git -c user.name=cozybear-dev -c user.email=mjvogel99@gmail.com commit`, plain-English subjects in the repository's style, ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Push with `git -c credential.helper= -c 'credential.helper=!gh auth git-credential' push origin <branch>`.
- The base of every subset is `38bf979f34` (upstream `v1.17.0-rc1` plus the cherry-pick of upstream `3c8cba7d99`). Patch *N* is the *N*th commit of `git log --reverse --format=%H --invert-grep --grep='^fixup!' 38bf979f34..origin/perf/v1.17.0-rc1` (1 = "Add parallel.For…", 33 = "Find files in the configuration saved in a plan…").
- Changed lines are `git diff --numstat 38bf979f34 <ref> -- . ':!*_test.go' ':!fork-proof' ':!*.md' ':!.github' ':!internal/provider-bench' ':!**/testdata/**' ':!Dockerfile*'`, added plus removed.
- Every benchmark build is `go build -trimpath -o …` with no `-ldflags`, so all builds report the same version and accept each other's plan files.
- Benchmark defaults: memory limit 10 GB with swap, `-parallelism` 10 and 100, request delay 50 ms each way on Vault (port 8200) and the Google mock (port 8766), plans `p50`, `p300`, `p1500` (mixed changes) and `kill` (1,500 creates), all saved by a build of `perf/v1.17.0-rc1`.
- The published apply image must report `terraform_version` equal to `version/VERSION` (`1.17.0-rc1`), the version the full fork's published image records in plan files.
- Changing repository ruleset 25032998 needs the owner's yes first.

## Review Focus

1. **The apply image has a different version string from the plan image.** If the image is built with `version.dev=yes`, a full-fork plan is refused with "plan file was created by Terraform 1.17.0-rc1, but this is 1.17.0-dev". Pinned by the version check in Task 9.
2. **A stale plan.** The state in the bucket changed after the plan was saved, so the apply must refuse it as stock does. Pinned by the stale-plan check in Task 3.
3. **A hard kill (`kill -9`, a Docker OOM kill) during the apply.** The state in the bucket must be readable, and no further behind than stock's. Pinned by `killtest.sh` in Task 4.
4. **A graceful stop (SIGINT, or SIGTERM from `docker stop`).** Afterwards nothing that was applied may be missing from the bucket. Pinned by the interrupt cases in `killtest.sh` in Task 4.
5. **A plan made by a newer commit of the full fork than the one last checked.** Pinned by the proof planning with the current head of `perf/v1.17.0-rc1` on every run (Task 8), and by the README's pinning advice (Task 10).

---

## Part A: the benchmark (branch `fork`, worktree `~/tfperf/forkctl`)

### Task 1: Mock and generator support for the apply benchmark

**Files:**
- Modify: `bench/gcpmock.py`
- Modify: `bench/gen4.py`
- Create: `bench/mkchanges.py`
- Test: `bench/test_bench.py`

**Interfaces:**
- Produces: `gcpmock.py PORT [--load FILE]`; `GET /_dump` returns the whole store as JSON; `GET /_count?prefix=P` returns `{"count": N}`, the number of stored names starting with `P`.
- Produces: `gen4.py` honours `BLOB_KB` (default `0`). When it is above 0, `main.tf` has one `locals { blob = "<BLOB_KB KiB of deterministic ASCII>" }`, every module call passes `blob = local.blob`, and the module has `variable "blob" { type = string, default = "" }` and puts `blob = var.blob` in `data_json`.
- Produces: `mkchanges.py DIR TARGET MIX [SEED]`, which edits a `COPIES=1` configuration in place and prints JSON `{"update": u, "create": c, "delete": d, "replace": r}` with the expected counts. `MIX` is `mixed` (70 % update, 10 % create, 10 % delete, 10 % replace of the target) or `create` (all creates). Updates change `tier = "bench"` to `tier = "bench2"` in a module's topics. Creates raise that module's `n` in `main.tf`, and deletes lower it. Replacements change `-secret-` to `-secret2-` in a module's `secret_id`. Modules are chosen in seeded order, and no module gets two kinds of edit.

- [ ] **Step 1: Write the failing tests** in `bench/test_bench.py` (stdlib `unittest`, run with `python3 -I`):
  - `test_mock_count_dump_load`: start the mock on a free port, `PUT /v1/projects/tfperf/topics/a` and `…/b`, then expect `GET /_count?prefix=projects/tfperf/topics/` to give `{"count": 2}`. Save `/_dump` to a file and restart with `--load` on it, then expect `GET /v1/projects/tfperf/topics/a` to give 200 with the same body.
  - `test_blob`: with `COPIES=1 BLOB_KB=2`, run `gen4.py` with 3 modules and 4 per module into a temp dir. Expect `main.tf` to contain exactly one 2048-character blob literal, three `blob = local.blob`, and each module's `data_json` to contain `blob = var.blob`.
  - `test_mkchanges_mixed`: generate 20 modules × 10 per module, then run `mkchanges.py DIR 100 mixed 1`. Expect the printed counts to be within ±25 % of 70/10/10/10 for update/create/delete/replace, and the count of `"bench2"` occurrences to equal the printed `update`. Running again with the same seed on a fresh copy must give identical files.
  - `test_mkchanges_create`: `mkchanges.py DIR 50 create 1` gives `update`, `delete` and `replace` all 0, and `create` within ±25 % of 50.
- [ ] **Step 2: Run the tests and see them fail.** Run `cd ~/tfperf/forkctl/bench && python3 -I test_bench.py -v`. Expected: 4 failures or errors (no `/_count`, no `BLOB_KB`, no `mkchanges.py`).
- [ ] **Step 3: Implement** the interfaces above. In the mock, `/_dump` and `/_count` are read under the existing `lock`.
- [ ] **Step 4: Run the tests and see them pass.** Run `python3 -I test_bench.py -v`. Expected: 4 passed.
- [ ] **Step 5: Commit** with the message "Let the benchmark's mock be saved and restored, and generate larger states and given changes".

### Task 2: Memory limit and network delay helpers

**Files:**
- Create: `bench/limit.sh`, `bench/netem.sh`, `bench/selftest.sh`

**Interfaces:**
- Produces: `limit.sh MEM_GB STATS_FILE CMD...` runs `CMD` with the caller's environment, working directory and user, in a cgroup v2 group with `memory.max` = `MEM_GB` GiB and unlimited swap. It exits with `CMD`'s status and writes `mem_peak_mb=`, `swap_peak_mb=` and `oom_kills=` lines to `STATS_FILE`, taken from that group's `memory.peak`, `memory.swap.peak` and `memory.events`. The mechanism is the implementer's choice, for example a transient group made with `sudo` that the command joins before it `exec`s.
- Produces: `netem.sh on DELAY_MS PORT...` delays TCP packets to and from each `PORT` on `lo` by `DELAY_MS`. `netem.sh off` removes the delay. One way to do it:
  ```
  tc qdisc add dev lo root handle 1: prio bands 4
  tc qdisc add dev lo parent 1:4 handle 40: netem delay ${DELAY}ms
  # for each port, both directions:
  tc filter add dev lo parent 1:0 protocol ip prio 1 u32 match ip dport $PORT 0xffff flowid 1:4
  tc filter add dev lo parent 1:0 protocol ip prio 1 u32 match ip sport $PORT 0xffff flowid 1:4
  ```
- Produces: `selftest.sh`, which exits 0 only if all of these hold:
  - A 300 MB allocation under `limit.sh 1` reports `mem_peak_mb` ≥ 300 and `swap_peak_mb` = 0.
  - A 1,500 MB allocation under `limit.sh 1` reports `swap_peak_mb` > 0 or `oom_kills` > 0.
  - With `netem.sh on 50 8766`, a request to a listener on 8766 takes at least 100 ms, and after `netem.sh off` it takes under 20 ms.

- [ ] **Step 1: Write `selftest.sh`** with the three checks above. It is the test of this task, and it runs only in Actions (Task 5, step 2).
- [ ] **Step 2: Implement `limit.sh` and `netem.sh`.**
- [ ] **Step 3: Commit** with the message "Run a command under a memory limit, and slow down the APIs, for the apply benchmark". This task is verified by the workflow's first run in Task 5.

### Task 3: Scenario and measurement scripts

**Files:**
- Create: `bench/scenario.sh`, `bench/apply.sh`, `bench/summary.py`

**Interfaces:**
- Consumes: Task 1 (`gen4.py`, `mkchanges.py`, the mock's `/_dump`) and Task 2 (`limit.sh`, `netem.sh`).
- Produces: `scenario.sh PLANNER_BIN OUT`, with environment `MODULES`, `PER_MODULE`, `BLOB_KB` and `PLANS` (space-separated `name:target:mix`, default `p50:50:mixed p300:300:mixed p1500:1500:mixed kill:1500:create`). It expects Vault on 8200, the mock on 8766 and the GCS emulator on 4443 with bucket `tfperf-state` to be running without delay. It writes:
  - `OUT/work/`: the configuration with `COPIES=1`, plus a `backend "gcs"` block with bucket `tfperf-state`, prefix `bench`, `access_token = "fake-token"` and `storage_custom_endpoint = "http://127.0.0.1:4443/storage/v1/"`. It has no `.terraform` directory.
  - `OUT/state.tfstate`, the state in the bucket after creating everything with `-parallelism=50`.
  - `OUT/mock.json`, the mock's `/_dump` at that point.
  - `OUT/plans/<name>.tfplan`, each saved with `PLANNER_BIN plan -input=false -out=…` after `mkchanges.py` on the configuration, which is then restored.
  - `OUT/plans/<name>.counts.json`, the counts of actions from `show -json` (`resource_changes[].change.actions`).
  - `OUT/sizes.txt`, the size in bytes of the state and of each plan.
  - It exits 1 if any plan's actual counts differ by more than 25 % from what `mkchanges.py` printed.
- Produces: `apply.sh SCENARIO BINDIR OUT`, with environment `PLANS`, `PARALLELISM`, `RUNS`, `MEM_GB`, `DELAY_MS` and `APPLY_TIMEOUT_MIN`.
  - `BINDIR` holds `terraform-<label>` files. The script copies `SCENARIO/work`, runs `init` once with the first binary, creates the Vault mount `bench` (kv-v2), and turns on `netem.sh` for 8200 and 8766.
  - For each run, plan, parallelism and label, labels taking turns:
    1. Restart the mock with `--load SCENARIO/mock.json`.
    2. Upload `SCENARIO/state.tfstate` to `bench/default.tfstate` in the bucket and delete `bench/default.tflock`.
    3. Run `timeout ${APPLY_TIMEOUT_MIN}m limit.sh $MEM_GB … /usr/bin/time -f '%e\t%U\t%S\t%M' terraform-<label> apply -input=false -no-color -parallelism=P SCENARIO/plans/<plan>.tfplan`.
  - Each apply appends a row to `OUT/times.tsv`: `label plan parallelism run wall user sys maxrss_kb mem_peak_mb swap_peak_mb oom_kills exit`.
  - After each successful apply, the number of resource instances in the bucket's state must equal the original count + `create` − `delete` from `counts.json`. If not, the row's `exit` is marked `wrong-count`.
  - **Stale-plan check:** once per label and plan, straight after a successful apply and without resetting, apply the same plan again. It must exit non-zero with output containing `stale`; otherwise mark `stale-accepted` in `OUT/checks.tsv`.
- Produces: `summary.py OUT [LINES_TSV]`, which writes `OUT/summary.md` and `OUT/summary.json`. There is one row per label, in input order, with its changed lines from `LINES_TSV` (`label\tlines`). For each plan and parallelism it gives the median wall time, the highest `mem_peak_mb + swap_peak_mb`, the highest `swap_peak_mb`, and any `exit` other than 0, plus wall and memory as a percentage of `stock`'s.

- [ ] **Step 1: Write the failing test** `test_summary` in `bench/test_bench.py`. Feed `summary.py` a temp `times.tsv`: two labels (`stock`, `x`), plan `p300`, parallelism 10, two runs each with walls 100/120 and 10/14, `x` with `swap_peak_mb` 0 and one row with `exit` = `wrong-count`. Feed it `lines.tsv` with `stock\t0`, `x\t45`. Expect `summary.json` to give `x` a median wall of 12.0 and lines 45, `stock` 110.0, `x`'s wall as 10.9 % of stock's, and `x` flagged `wrong-count`.
- [ ] **Step 2: Run** `python3 -I test_bench.py -v`. Expected: `test_summary` fails, the other 4 pass.
- [ ] **Step 3: Implement** `summary.py`, `scenario.sh` and `apply.sh`.
- [ ] **Step 4: Run** `python3 -I test_bench.py -v`. Expected: 5 passed. The shell scripts are verified by the workflow's smoke run in Task 5.
- [ ] **Step 5: Commit** with the message "Measure applying saved plans against a state in a GCS emulator".

### Task 4: Kill test

**Files:**
- Create: `bench/killtest.sh`

**Interfaces:**
- Consumes: Task 3's scenario layout and reset steps. Factor the reset (restart the mock, upload the state, remove the lock) into `bench/reset.sh SCENARIO`, used by both `apply.sh` and `killtest.sh`.
- Produces: `killtest.sh SCENARIO BINDIR OUT LABEL...`, with environment `KILLS` (default 10), `SEED`, `DELAY_MS`, `PARALLELISM` (default 10) and `MEM_GB`. For each label:
  1. Reset, then time one full apply of `plans/kill.tfplan`. Call its wall time *T*.
  2. `KILLS` times: reset, start the apply in its own process group, wait a seeded random time in [0.1 *T*, 0.9 *T*], then `kill -9` the group.
  3. Two more times: the same, but send `SIGINT` to the `terraform` process and wait for it to exit; then the same with `SIGTERM`.
  4. After each stop, download the state from the bucket and record `lag = mock /_count?prefix=projects/tfperf/topics/ − topics in the mock dump − (google_pubsub_topic instances in the bucket state − those in SCENARIO/state.tfstate)`.
  - It writes `OUT/kill.tsv` (`label signal attempt lag readable`) and `OUT/kill.md`, with the median and highest lag per label and signal.
  - It exits 1 if any downloaded state can't be read by `terraform-stock show -json`, or if any SIGINT or SIGTERM lag is above 0.

- [ ] **Step 1: Write `killtest.sh` and `reset.sh`,** and switch `apply.sh` to use `reset.sh`.
- [ ] **Step 2: Commit** with the message "Check how far behind the state in the bucket is after an apply is stopped". This task is verified by the workflow run in Task 5.

### Task 5: The workflow

**Files:**
- Create: `.github/workflows/fork-apply-bench.yml`
- Modify: `README.md` (one paragraph: what the workflow measures and how to start it)

**Interfaces:**
- Consumes: Tasks 1–4.
- Produces: the workflow "Fork apply benchmark" (`workflow_dispatch`). Its inputs and defaults:
  - `builds`: space-separated `label=ref`. A `ref` starting with `upstream:` is fetched from `https://github.com/hashicorp/terraform.git`; any other ref comes from this repository. Default `stock=upstream:v1.17.0-rc1 full=perf/v1.17.0-rc1`.
  - `planner_ref`: default `perf/v1.17.0-rc1`.
  - `scale`: `full` or `smoke`. `full` is `MODULES=1000 PER_MODULE=20` with `BLOB_KB` tuned in step 3. `smoke` is `MODULES=20 PER_MODULE=5 BLOB_KB=0`.
  - `plans`: default `p50 p300 p1500`.
  - `parallelism`: default `10 100`.
  - `runs`: default `2`.
  - `memory_gb`: default `10`.
  - `delay_ms`: default `50`.
  - `apply_timeout_min`: default `40`.
  - `kill_labels`: default empty, meaning no kill job.

  Its jobs, all on `ubuntu-24.04-arm`. If Docker images or swap aren't available there, use `ubuntu-latest` and say so in the README:
  - `matrix`: parses `builds` into a JSON matrix.
  - `build`: one per build, plus the planner.
    - Build with Go from the ref's `.go-version`.
    - Write `lines=<n>` using the Global Constraints pathspec, fetching `38bf979f34` by SHA; `stock` gets 0.
    - Run `go vet ./internal/...` and fail the build if vet fails.
    - Upload the binary and its line count.
  - `scenario`: start Vault, the mock and `fake-gcs-server` (the bucket is created with a `POST` to `/storage/v1/b?project=tfperf`), run `scenario.sh` with the planner, and upload `OUT` as an artifact.
  - `measure`: one job per plan, timeout 360 minutes. Add an 8 GB swap file, run `selftest.sh`, start the services, then run `apply.sh` with all builds.
  - `kill`: only if `kill_labels` is set. Runs `killtest.sh` with those labels.
  - `report`: merges each measure job's `times.tsv`, runs `summary.py` with the lines of every build, and writes `summary.md` and `kill.md` to the run summary. Uploads `summary.json`.

- [ ] **Step 1: Write the workflow and the README paragraph.** Commit with the message "Add a workflow that measures applying saved plans with any list of builds", then push `fork`.
- [ ] **Step 2: Smoke run.** Run `gh workflow run "Fork apply benchmark" --ref fork -f scale=smoke -f runs=1 -f kill_labels="stock full"`. Expected:
  - every job is green, and `selftest.sh` passes
  - the run summary has rows `stock` and `full` for `p50`, `p300` and `p1500` at parallelism 10 and 100, with `exit` 0
  - `checks.tsv` has no `stale-accepted`
  - `kill.md` shows lag 0 for SIGINT and SIGTERM for both labels

  Fix and re-run until all of that holds.
- [ ] **Step 3: Tune `BLOB_KB`.** Run with `scale=full -f plans=p50 -f runs=1 -f builds="full=perf/v1.17.0-rc1"`, and read `sizes.txt` in the `scenario` artifact. Set the `full` default of `BLOB_KB` so the state is 380–420 MB, commit with the message "Make the apply benchmark's state about 400 MB", and push.
- [ ] **Step 4: Reference run.** Run with `scale=full` and the default builds. Record in the progress notes (below) the stock and full-fork rows for `p300` at parallelism 100. Expected: `full` uses well under 10 GB with `swap_peak_mb` 0. If stock doesn't fit or times out, that is recorded as a result, not fixed.

## Part B: choosing the cut

Progress notes for Part B go in `~/tfperf/apply-bench/NOTES.md` (local, not committed): each run's id, its builds, and the table.

### Task 6: Subset branches

**Files:**
- Create: `bench/subset.sh` (on `fork`)

**Interfaces:**
- Produces: `subset.sh LABEL N...`, run in the scratch worktree `~/tfperf/stack/wt`.
  - It checks out a new branch `dev/apply-LABEL` at `38bf979f34` and cherry-picks patches `N...` in ascending order, with `rerere.enabled=true` and `rerere.autoupdate=true` set in the repository.
  - On a conflict that rerere can't resolve, it stops with the branch mid-pick and prints the files.
  - On success it prints the changed-lines count and pushes the branch.

- [ ] **Step 1: Write `subset.sh`,** commit it with the message "Make a branch of a subset of the fork's patches", and push `fork`.
- [ ] **Step 2: Make the round-1 singles,** each patch 1 plus one candidate with its prerequisites:

  | Label | Patches |
  |---|---|
  | `s18` | 1 18 |
  | `s9` | 1 4 5 6 9 |
  | `s12` | 1 12 |
  | `s13` | 1 12 13 |
  | `s10` | 1 10 |
  | `s15` | 1 15 |
  | `s14` | 1 14 |
  | `s7` | 1 7 |
  | `s17` | 1 7 17 |
  | `s8` | 1 8 |
  | `s26` | 1 26 |
  | `s25` | 1 25 |
  | `s24` | 1 24 |
  | `s22` | 1 22 |
  | `s19` | 1 19 |
  | `s27` | 1 27 |
  | `s33` | 1 33 |

  - For the known conflicts (18 and 27 in `internal/backend/local/backend_apply.go`, 22 in `internal/terraform/evaluate.go`), resolve by hand. Keep only what that patch itself adds, and adapt it to the upstream code around it. Then `git cherry-pick --continue`, so that rerere records the resolution.
- [ ] **Step 3: Run round 1.** Run `gh workflow run "Fork apply benchmark" --ref fork -f scale=full -f runs=1 -f builds="stock=upstream:v1.17.0-rc1 full=perf/v1.17.0-rc1 s18=dev/apply-s18 …"`.
  - For each single whose `build` job fails (compile or vet), find the missing names with `git log -S<name> --oneline 38bf979f34..origin/perf/v1.17.0-rc1`. Add the patch that defines each one as a prerequisite, remake the branch, and re-run just those builds.
  - Record each single's lines, and its `p300`/parallelism-100 memory and wall time relative to stock.
- [ ] **Step 4: Round 2, cumulative.**
  - Order the candidates by peak memory saved (`mem_peak_mb + swap_peak_mb`, `p300`, parallelism 100) per changed line, until the cumulative set is expected to bring `swap_peak_mb` to 0. Order the rest by wall time saved per changed line.
  - Drop any single that saved less than 2 % of stock's wall time and less than 2 % of its memory.
  - Make one branch per prefix of that order (`c1`, `c2`, …), each containing the union of patches so far, and run them all with the default plans and runs.
- [ ] **Step 5: Round 3, leave-one-out.** Take the smallest prefix where each further patch adds less than 3 % (wall or memory, `p300`/100). For each patch in it, make `x<N>`, the prefix without that patch, and run them. A patch whose removal costs less than 2 % on every plan and parallelism comes out of the set.
- [ ] **Step 6: Trim large patches.** For each kept patch over 300 changed lines, read its hunks and list those used only by `plan`, `show`, rendering or plan writing. If there are any:
  - Make `t<N>`, the set with that patch minus those hunks: `git cherry-pick -n`, then revert the hunks, then commit.
  - Run it. Keep the trimmed version only if `p300` and `p1500` stay within 5 % of the untrimmed set on wall and memory, and its `build` job passes vet.
- [ ] **Step 7: Kill test of the candidate set.** If patch 18 is in the set, run with `-f kill_labels="stock <set label>"`. Record the median and highest lags. If the set's highest `kill -9` lag is more than 25 % above stock's, stop and bring it to the owner before going on.
- [ ] **Step 8: Checkpoint with the owner.** Publish with `html_render` the table of rounds 2–3:
  - one row per prefix: changed lines; wall and memory per plan and parallelism; percentage of stock and of the full fork
  - the leave-one-out results
  - the kill-test lags

  Mark the proposed cut. Ask the owner to pick the cut; nothing in Part C starts before that. Then delete the `dev/apply-*` branches that aren't the chosen set: `git push origin --delete dev/apply-…` (they aren't covered by the ruleset).

## Part C: the branch (in the project checkout, `/home/fancybear/.t3/projects/terraform-speed`)

### Task 7: Lay out `perf-apply/v1.17.0-rc1` locally

**Files:**
- The chosen patches, unchanged except for their recorded conflict resolutions and trims.
- From `origin/perf/v1.17.0-rc1`, adapted in Tasks 8–10: `version/fork.go`, `internal/command/views/version.go` (patch 30); `README-FORK.md`, `Dockerfile.fork`, `Dockerfile.fork.dockerignore`, `.github/workflows/fork-image.yml` (patch 31 with its fixups); `fork-proof/**` and `.github/workflows/fork-proof.yml` (patch 32 with its fixups).

**Interfaces:**
- Produces: a local branch `perf-apply/v1.17.0-rc1` laid out as:
  1. `38bf979f34`
  2. the chosen code patches in stack order, except 33
  3. patch 30
  4. "Describe the fork's apply build, and publish a container image of it"
  5. "Add a proof, that others can repeat, that the apply build does what upstream does"
  6. patch 33, if chosen

- [ ] **Step 1: Make the branch.** In the project checkout, run `git switch -c perf-apply/v1.17.0-rc1 38bf979f34`, then cherry-pick the commits of the chosen `dev/apply-*` branch in the order above (rerere replays the recorded resolutions). Add patch 30 by cherry-pick. Add the files of 31 and 32 with `git checkout origin/perf/v1.17.0-rc1 -- <files>`, one commit each, with the subjects above.
- [ ] **Step 2: Check the content.** Expected: the changed-lines count of `HEAD` (Global Constraints pathspec) equals the chosen set's count plus 20 for patch 30, and `git diff --name-only 38bf979f34 HEAD` lists nothing outside the chosen patches' files, patch 30's two files, and the files of 31 and 32.
- [ ] **Step 3: Keep it local.** Nothing is pushed until Task 10.

### Task 8: The proof applies full-fork plans

**Files:**
- Modify: `fork-proof/main.go` (flag), `fork-proof/build.go` (`buildOptions`, `toolchain`, `prepare`), `fork-proof/scenario.go` (`actors`)
- Modify: `fork-proof/FORK_FILES`, `fork-proof/README.md`
- Modify: `.github/workflows/fork-proof.yml`
- Test: `fork-proof/actors_test.go`

**Interfaces:**
- Produces: the flag `-planner-ref REV`: a commit of the full fork that plans in two more actors (default empty: no such actors). It sets `buildOptions.plannerRef string`. `toolchain.planner string` is the path of a terraform built from that commit's tree by `prepare`, with the same `build("full fork terraform, which plans", …)` call as the fork; it is skipped under `-skip-build`, as the others are.
- Produces: when `t.planner != ""`, `actors()` appends `{name: "full-fork-plans-fork-applies", planner: t.planner, applier: t.fork, fork: true}` and `{name: "full-fork-plans-upstream-applies", planner: t.planner, applier: t.upstream, fork: true}`.
- Produces: the report gains `"planner": {"commit": …, "binary": <sha256>}` when set.

- [ ] **Step 1: Write the failing test** `TestActorsWithPlanner` in `fork-proof/actors_test.go`:
  - With `toolchain{upstream: "u", fork: "f", planner: "p"}`, `actors()` contains the two actors above with exactly those `planner`, `applier` and `fork` values.
  - With `planner: ""`, neither name is present, and the list equals the current five actors.
- [ ] **Step 2: Commit and push** to `dev/apply-proof` (made from the local `perf-apply/v1.17.0-rc1`). Run `gh workflow run "Fork image" --ref dev/apply-proof`. Expected: the proof job fails at its `go vet . && go test .` step on `TestActorsWithPlanner` (unknown field `planner`).
- [ ] **Step 3: Implement** the flag, the build and the actors.
  - In `fork-proof.yml`, fetch `perf/v1.17.0-rc1` from this repository before running the proof, and pass `-planner-ref "$(git rev-parse FETCH_HEAD)"`.
  - Regenerate the file list in `FORK_FILES` from `git diff --name-only 38bf979f34 HEAD` (keep its header).
  - Say in `fork-proof/README.md` what the two actors check.
- [ ] **Step 4: Commit** as a fixup of commit 5 of Task 7, then push `dev/apply-proof` again and re-run "Fork image" on it. Expected: unit tests, race tests and proof pass; the verdict is `identical`; the proof's log lists the actors `full-fork-plans-fork-applies` and `full-fork-plans-upstream-applies` for every scenario; and the report has `planner.commit` equal to the head of `perf/v1.17.0-rc1`.

### Task 9: The image workflow for the apply build

**Files:**
- Modify: `.github/workflows/fork-image.yml`, `Dockerfile.fork` (title label only)

**Interfaces:**
- Produces:
  - Triggers on pushes to `perf-apply/v*` only; tags are removed.
  - Publishes when the ref starts with `refs/heads/perf-apply/v`.
  - `version-dev=no` for `branch:perf-apply/v*`.
  - Tags `ghcr.io/cozybear-dev/terraform:perf-apply-v1.17.0-rc1` and `…:perf-apply-v1.17.0-rc1-<12-char sha>`.
  - The image title is "Terraform (apply build of the large state performance fork)".
  - A new step after the image is built: `docker run --rm <main-tag> version -json`, whose `terraform_version` must equal `$(cat version/VERSION)`; otherwise the job fails before publishing anything.

- [ ] **Step 1: Make the changes** and commit them as a fixup of commit 4 of Task 7. Push `dev/apply-proof` and run "Fork image" on it. Expected: it runs the three checks and publishes nothing, because the ref isn't `perf-apply/v*`. The version step only runs where an image is built, so it is exercised in Task 11, step 2.

### Task 10: README for the apply build

**Files:**
- Modify: `README-FORK.md`

- [ ] **Step 1: Rewrite `README-FORK.md`** for this build only, in the full fork's style. Sections:
  - **What it is for.** Applying plans saved by `perf/v1.17.0-rc1`.
  - **How to use it.** Plan with `ghcr.io/cozybear-dev/terraform:perf-v1.17.0-rc1-<sha>`, pinned by its sha tag, then apply with this image. Both report version `1.17.0-rc1`.
  - **The patches it keeps.** A table with patch, title, changed lines and the measured gain, plus the total.
  - **The table from Task 6.** Include the run ids.
  - **What differs from upstream.** Only for the kept patches. If 18 is kept, difference 1 is described for a GCS backend with the kill-test lags from Task 6, step 7, and the SIGINT/SIGTERM result.
  - **How it was verified.** The proof, including the two full-fork actors, and the full-fork commit and image digest last checked.
  - **How it's maintained.** A stack like the full fork's; moving it to a new upstream isn't automated yet.
- [ ] **Step 2: Fold it in** with a fixup of commit 4, and `git rebase --autosquash 38bf979f34` (the branch is unpublished, so folding is allowed). Check with `git log --oneline 38bf979f34..` that there is one commit per patch and no `fixup!`.
- [ ] **Step 3: Final check.** Push the result as `dev/apply-final` and run "Fork image" on it. Expected: green, verdict `identical`.

### Task 11: Publish

- [ ] **Step 1: Ask the owner** whether to add `refs/heads/perf-apply/v*` to ruleset 25032998, which refuses force-pushes and deletion. If yes, run `gh api repos/cozybear-dev/terraform/rulesets/25032998` to read it, add the pattern to `conditions.ref_name.include`, and `PUT` it back.
- [ ] **Step 2: Push** `perf-apply/v1.17.0-rc1` (same commit as `dev/apply-final`). Watch the "Fork image" run. Expected: green, the version step passes, and `ghcr.io/cozybear-dev/terraform:perf-apply-v1.17.0-rc1` exists.
- [ ] **Step 3: Measure the published build** with the benchmark: `builds="stock=upstream:v1.17.0-rc1 full=perf/v1.17.0-rc1 apply=perf-apply/v1.17.0-rc1"`, default plans and runs. Expected: `apply` within 5 % of the chosen row of the Task 6 table.
- [ ] **Step 4: Clean up.** Delete `dev/apply-*` branches. Update the memory note `apply-branch-project.md` with the branch, the image and the chosen patches. Tell the owner the image tag and the table.
