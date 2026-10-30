# Terraform, apply build of the large-state performance fork

This is an unofficial build of [hashicorp/terraform](https://github.com/hashicorp/terraform)
for one job: `terraform apply` of a plan that the
[large-state performance fork](https://github.com/cozybear-dev/terraform/tree/perf/v1.17.0-rc1)
saved, when the state is very large, hundreds of megabytes and tens of
thousands of resource instances.

It has 14 of the full fork's 33 patches, two of them only in part: the ones
that applying a saved plan measurably needs, chosen by measuring each of
them. It has a README, an image workflow and a proof of its own. That is 6,697 changed lines instead of 10,656, so that what it changes
can be read and understood. On a 400 MB state it applies 300 changes in
about 14 seconds and 3.8 GB, where upstream takes 35 minutes and 16.5 GB,
and the full fork 12 seconds and 3.2 GB.

- Plan with the full fork, and apply that plan with this build. See
  [How to use it](#how-to-use-it).
- Not reviewed or endorsed by HashiCorp. Try it on a non-production workspace
  first, and read [What differs from upstream](#what-differs-from-upstream)
  before pointing it at state you care about.

## Contents

- [How to use it](#how-to-use-it)
- [The patches it keeps](#the-patches-it-keeps)
- [Measurements](#measurements)
- [What changed and why](#what-changed-and-why)
- [What is the same as upstream](#what-is-the-same-as-upstream)
- [What differs from upstream](#what-differs-from-upstream)
- [Compatibility](#compatibility)
- [How it was verified](#how-it-was-verified)
- [Known limits and open issues](#known-limits-and-open-issues)
- [Where the changes are](#where-the-changes-are)
- [How it's maintained](#how-its-maintained)

## How to use it

Every push to a branch `perf-apply/v<version>` builds an image for
`linux/amd64` and `linux/arm64` with
[`.github/workflows/fork-image.yml`](.github/workflows/fork-image.yml) and
publishes it to the GitHub Container Registry. Nothing is published unless
four things pass for that commit:

- upstream's own unit tests, for every Go module in the repository;
- the race detector, on the packages upstream runs it on and the ones this
  build changes;
- the [integrity proof](fork-proof/README.md), which builds upstream at the
  commit this build is based on, this build, and the full fork, and compares
  what they do, including this build applying plans that the full fork saved;
- `terraform version -json` in the image reporting upstream's version, as the
  full fork's image does. A plan can only be applied by a build that reports
  the version that saved it.

| Tag | Meaning |
|---|---|
| `ghcr.io/cozybear-dev/terraform:perf-apply-v1.17.0-rc1` | Latest build of the branch `perf-apply/v1.17.0-rc1` |
| `ghcr.io/cozybear-dev/terraform:perf-apply-v1.17.0-rc1-<commit>` | One specific commit (first 12 characters of its hash) |

Plan with the full fork's image and apply with this one, both pinned by
their per-commit tags. Both report `Terraform v1.17.0-rc1`, so the plan is
accepted:

```sh
docker run --rm -it -v "$PWD":/workspace -w /workspace \
  -v "$HOME/.config/gcloud":/root/.config/gcloud:ro \
  ghcr.io/cozybear-dev/terraform:perf-v1.17.0-rc1-<commit> plan -out=tfplan

docker run --rm -it --stop-timeout 600 -v "$PWD":/workspace -w /workspace \
  -v "$HOME/.config/gcloud":/root/.config/gcloud:ro \
  ghcr.io/cozybear-dev/terraform:perf-apply-v1.17.0-rc1-<commit> apply tfplan
```

`--stop-timeout` gives the apply time to save the state if the container is
stopped. See [difference 1](#1-intermediate-state-snapshots-during-apply).

Use a full-fork commit that this build's proof has checked: the proof plans
with the head of `perf/v1.17.0-rc1` on every run, and the commit it last
checked is in [How it was verified](#how-it-was-verified). Before moving
the plan image to a newer commit of the full fork, run **Fork image** on
this build's branch, which checks that commit.

The image is laid out like the official `hashicorp/terraform` image: Alpine
with `git` and an SSH client, and `/bin/terraform` as the entrypoint. Its
proof report and a statement of which commit and workflow built it are
attached as signed attestations:

```sh
gh attestation verify oci://ghcr.io/cozybear-dev/terraform@<digest> --repo cozybear-dev/terraform
gh attestation verify oci://ghcr.io/cozybear-dev/terraform@<digest> --repo cozybear-dev/terraform \
  --predicate-type https://cozybear.dev/terraform/fork-proof/v1
```

From source: `go build -o terraform .` with the Go version in `.go-version`.
A build from source reports `1.17.0-dev`, and so refuses the full fork's
published plans; build with `-ldflags "-X github.com/hashicorp/terraform/version.dev=no"`
as `Dockerfile.fork` does.

It can plan too, being a complete Terraform, but it doesn't have the full
fork's patches for planning, so plan with the full fork.

## The patches it keeps

The branch is upstream `v1.17.0-rc1`, then upstream's `3c8cba7d99` (see
`fork-proof/UPSTREAM_BACKPORTS`), then one commit for each patch below, in
the full fork's order, then this file and the image workflow, then the proof.
Patch numbers are the full fork's. 6a and 15r are the original lines of half
a patch, unchanged.

What a patch is worth was measured by leaving it out of the set and applying
again, at `-parallelism=100`, unless it says otherwise (see
[Measurements](#measurements)).

| Patch | Title | Changed lines | What leaving it out costs |
|---|---|---|---|
| 1 | Add parallel.For, and a way to tell whether logging is enabled | 126 | Needed by 10, 14, 15r, 18, 22 and 27 |
| 4 | Build the string forms of addresses with less work | 112 | Needed by 9 and 10 |
| 5 | Don't rebuild a value to find out that it has no marks | 145 | Needed by 9 and 14 |
| 6a | Decode JSON values with less work (the decoding half of 6) | 593 | Needed by 9. The msgpack half, 286 lines, made no measurable difference |
| 9 | Keep less, and do less, for each object of a state in memory | 1,113 | With 1, 4, 5 and 6 alone: 1,234 s to 190 s for 50 changes, at parallelism 10. 15r, 18, 22 and 27 build on it |
| 10 | Read and write state files on all of the processors | 2,439 | With 12 and 13: 73 s to 17.5 s for 300 changes, the fixed cost of reading and writing the state |
| 12 | Don't retrieve or serialize an unchanged remote state again | 90 | Nothing on its own; 13 builds on it |
| 13 | Transfer a large state to and from GCS in fewer, larger requests | 300 | 6.5 s for 300 changes |
| 14 | Index the planned changes, and encode and decode them concurrently | 654 | With 22: 32 s and 3 GB. Without them, the set without 10 swaps |
| 15r | Read the parts of a plan file concurrently (the reading half of 15) | 475 | 4.5 s and 1 GB, measured for all of 15. Applying never writes a plan file, which is the other half |
| 18 | Save snapshots of a large state in the background while applying | 393 | 11 s for 300 changes, 58 s for 1,500 |
| 22 | Build the value of a resource once for everything that refers to it | 196 | 2.5 s and 1.5 GB |
| 27 | Release memory between the stages of an operation on a large state | 41 | 1.6 to 2.6 GB, without 10, 12, 13 and 15r |
| 30 | Say in the output of "terraform version" which build of the fork this is | 20 | Nothing; says which build this is |
| | **Total** | **6,697** | |

Changed lines leave out tests, test data, documentation, workflows,
Dockerfiles and the proof.

Left out, and why:

| Patches | What they do | Why they're left out |
|---|---|---|
| 7, 8, 17 | Graph algorithms, starting goroutines when ready, graph building | No measurable change to an apply: within 1 to 2 s of the set without them |
| 19, 20, 21, 24, 25, 26, 33 | Smaller evaluation and lookup savings | No measurable change to an apply |
| 11 | Read the local state file once | Only for the local backend |
| 16, 23, 28 | Render a plan, keep values across refresh, skip validation | Used when planning, not when applying a saved plan |
| 2, 3, 29 | Profiling hooks, benchmark provider, module installation | Not part of an apply |

## Measurements

Measured by the **Fork apply benchmark** workflow on the repository's `fork`
branch, on GitHub's linux/arm64 runners (4 CPUs, 16 GB). Each build applies
plans saved by the full fork to a 400 MB state of 90,002 resource instances
in a GCS emulator, with the real Vault and Google providers talking to a
Vault server and a mock of Google's API that are 50 ms away each way, under
a 10 GB memory limit with swap. The plan of 300 changes has 61 to add, 200
to change and 62 to destroy, a replacement counting as an add and a
destroy; the others are mixed alike. Each cell is the median wall time of 2
runs and the most memory, RAM and swap, that an apply held at once.

| Build | Changed lines | 50 changes, parallelism 10 / 100 | 300 changes, 10 / 100 | 1,500 changes, 10 / 100 |
|---|---|---|---|---|
| Upstream v1.17.0-rc1 | 0 | 1,234 s, 17.3 GB / 425 s, 15.8 GB | stopped at 40 min / 2,132 s, 16.5 GB | not run |
| Without 10, 12, 13 and 15r | 3,393 | 74.9 s, 4.7 GB / 72.9 s, 5.3 GB | 86.3 s, 5.3 GB / 74.1 s, 5.1 GB | 148 s, 7.9 GB / 82.2 s, 5.1 GB |
| Without 15r | 6,222 | 20.4 s, 4.6 GB / 17.4 s, 5.0 GB | 30.7 s, 4.7 GB / 19.1 s, 5.1 GB | 92.3 s, 5.4 GB / 27.1 s, 5.2 GB |
| **This build** | **6,697** | 15.7 s, 3.6 GB / 14.1 s, 3.7 GB | 26.6 s, 3.6 GB / 15.3 s, 3.7 GB | 87.2 s, 3.9 GB / 23.1 s, 3.8 GB |
| With all of 6 and 15, without 30 | 7,470 | 15.9 s, 3.7 GB / 14.0 s, 3.7 GB | 26.3 s, 3.8 GB / 14.4 s, 3.7 GB | 86.6 s, 4.4 GB / 21.2 s, 3.6 GB |
| Full fork | 10,656 | 13.3 s, 3.0 GB / 10.9 s, 3.1 GB | 23.9 s, 3.0 GB / 12.1 s, 3.1 GB | 82.8 s, 3.7 GB / 18.0 s, 3.3 GB |

- Run 40974104063, except upstream, which is run 40939372232 with one run
  each. One apply of 1,500 changes would take upstream hours.
- The two rows above this build's ran on another runner than it. On one
  runner, with 3 runs each (run 40983322808), this build took 14.3 s and
  3.8 GB for 300 changes at parallelism 100, and 21.9 s and 3.8 GB for
  1,500; with all of 6 and 15, 14.5 s and 3.7 GB, and 21.9 s and 3.7 GB.
- At parallelism 10, 1,500 changes spend about 80 s waiting for the
  APIs, whatever the build.
- Leaving out each patch: run 40967178710. The patches left out: run
  40958726372.

Every apply by the builds in this table other than upstream stayed out of
swap, every apply left the expected number of objects in the state, and
every build refused the same plan a second time as stale.

These are synthetic workloads, built to resemble a real large state.

## What changed and why

### Reading state, at the start of an apply

**Why:** upstream reads a state file on one CPU, with the standard JSON
decoder validating the whole document and then scanning each object's
attributes a second time. Applying a saved plan reads the state from the
backend, and from the plan file, which holds two snapshots of it.

- The file is split at resource boundaries and the resources are decoded on
  all CPUs, by a decoder that accepts only the plain form that Terraform
  itself writes. Anything else, including anything invalid, is handed to
  the standard decoder exactly as before (patch 10).
- Dependency lists are decoded, and their addresses parsed, in that same
  phase. Objects with identical dependencies share one list.
- The plan and the two state snapshots in the plan file are read
  concurrently, each snapshot is parsed once, and it shares its data with
  the state already loaded from the backend (15r).
- Resource instance objects keep the value they decode to and the bytes they
  were decoded from, so an object that doesn't change isn't decoded or
  encoded again, and copies of a state share the bytes of unchanged objects
  (9). Values are decoded from JSON in one pass (6a).

### The walk

- Planned changes are indexed instead of being scanned for every lookup,
  and are decoded concurrently. The values of no-op changes are decoded
  only if something refers to them (14).
- The value of a resource, as seen by expressions that refer to it, is built
  once and reused until that resource changes (22).

### Saving state while applying

**Why:** upstream copies the entire state after every resource instance
finishes, while holding the lock on the state, and with a remote backend it
uploads the whole state, every 20 seconds, inside the state update of
whichever resource finishes next, so that every other resource waits for
the upload. This made apply time grow with the square of the state.

- For a state of at least 2,000 resource instances, snapshots are taken by
  a background task, and the periodic upload is done by it too, on the same
  20-second schedule (18).
- The final state is always written, in full, at the end.
- On interrupt (Ctrl-C), the latest state is written immediately, and from
  then on every update is written before the next one proceeds, as upstream
  does.

This is the most important behaviour change. It is
[difference 1](#1-intermediate-state-snapshots-during-apply).

### Writing state, at the end

- Objects are encoded on all CPUs, in windows, so that the encoded form of
  the whole state is never in memory at once (10).
- Whether a state has changed is decided from a digest, or from a cheap
  structural comparison, instead of serializing both states (10), and an
  unchanged remote state isn't retrieved or serialized again (12).

### Remote state in Google Cloud Storage

- A state larger than 16 MiB is uploaded in one request instead of in 16 MiB
  chunks that each wait for the previous one.
- The state is downloaded once per operation instead of twice: before the
  second download, the object's generation is checked.
- A state of 64 MiB or more is downloaded as up to eight ranges of one
  generation at once, and used only if its CRC32C is the one the service
  reports for the object; otherwise it's read as one stream as before.

### Memory

- For a state of at least 2,000 resource instances, memory that is no
  longer needed is returned to the operating system after loading, after
  planning and before the final state is written (27).
- With logging off, Terraform's trace messages aren't formatted at all (1).

## What is the same as upstream

The rule followed throughout is that the build must produce exactly what
upstream produces, and that every fast path falls back to upstream's own
code for any input it doesn't recognize.

For the same configuration, plan, state and provider responses, these are
intended to be identical, and were checked to be (see
[How it was verified](#how-it-was-verified)):

- The bytes of a state file, apart from `serial` (difference 2).
- What is planned and applied.
- Errors and diagnostics, with the exceptions listed below.
- Locking, lineage, and how conflicts between writers are detected.

A state with fewer than 2,000 resource instances is saved after every
resource exactly as upstream saves it.

## What differs from upstream

These are the full fork's differences that come from the patches this build
keeps, numbered for this build. Difference 1 is about what is in the bucket
if an apply is stopped. Read it first.

### 1. Intermediate state snapshots during apply

This applies once the state has at least 2,000 resource instances, and only
then.

| | Upstream | This build |
|---|---|---|
| After each resource instance is applied | A copy of the whole state is taken before anything else proceeds | The state is marked as changed; a background task takes a snapshot of the latest state |
| Upload to the bucket | Every 20 s, inside the update of the next resource to finish | Every 20 s, from the background task |
| On Ctrl-C, SIGINT or SIGTERM | Latest state uploaded; every later update written | The same: pending changes are written at once, then every update is written before the next proceeds |
| On an error from a provider | Final state written at the end | The same |
| At the end | Final state written | The same; the background task is stopped first, so it can't overwrite the final state |

**After a graceful stop nothing is missing,** with either build. That
covers an error from a provider, a failed apply, Ctrl-C, and `docker stop`
if it waits long enough. After SIGTERM, Terraform lets the operations in
progress finish and then writes and uploads the state, which for a state of
hundreds of megabytes takes longer than the 10 seconds that `docker stop`
waits by default before it sends SIGKILL, and that is a hard stop. Start the
container with `--stop-timeout 600`, or stop it with `docker stop -t 600`.

**After a hard stop, objects that were created can be missing from the
state in the bucket.** A hard stop is anything that gives Terraform no
chance to react: `kill -9`, an out-of-memory kill, a CI job timeout, a
second Ctrl-C, a panic, or the machine losing power. With either build, the
bucket holds the state as of the last upload, up to 20 seconds and the
time an upload takes before the stop. This build applies far more in that
time than upstream does, because upstream applies each resource far more
slowly.

**Measured** (run 40974104063): a plan that creates 1,498 objects on the
benchmark's 400 MB state, about 330 of them Pub/Sub topics, was applied,
and stopped 10 times with `kill -9` at random moments, once with SIGINT and
once with SIGTERM. What was counted is the Pub/Sub topics that the
provider had created and that the state in the bucket didn't record; the
other kinds of object created in the same moments are missing alike, and
weren't counted.

| Build | kill -9: median topics missing | most | SIGINT | SIGTERM | States upstream couldn't read |
|---|---|---|---|---|---|
| This build | 62 | 193 | 0 | 0 | 0 of 12 |
| Full fork | 79.5 | 249 | 0 | 0 | 0 of 12 |

One apply of that plan took this build about 96 seconds. What was missing
after a kill had been created since the last upload, so within about 20
seconds and the time an upload takes; a kill after the creates had finished
found nothing or almost nothing missing. Upstream wasn't stopped this way,
because one apply of that plan would take it about 2.5 hours.

Objects that were created but are missing from the state are not lost in
the provider: they exist, and Terraform no longer knows about them.
Recovery is the same as for upstream after a hard stop (import, or remove
by hand), but there can be more of them.

With a local state file, upstream's file is current to the last completed
resource, and this build's to the last background snapshot, which can be
missing about eleven times the duration of one snapshot's write.

Each intermediate upload is the whole state. Upstream's own
`TF_STATE_PERSIST_INTERVAL` (in seconds; values below 20 are ignored) sets
the interval, for both builds.

### 2. The `serial` in the state

Upstream increments `serial` each time it writes a snapshot that differs
from the one before. This build writes fewer intermediate snapshots, so the
`serial` after an apply can be lower than upstream's. It still increases
with every write that changes the state, which is all that Terraform relies
on.

### 3. Google Cloud Storage backend

- A state over 16 MiB is uploaded in a single request. If that fails with an
  error worth retrying, the whole upload is retried up to three times, with
  pauses of one and two seconds, and then the original chunked upload is
  used. States of 16 MiB or less are uploaded exactly as before.
- One fewer download of the state per operation, and one extra metadata
  request for the object's generation.
- A state of 64 MiB or more is read with up to eight range requests for one
  generation of the object, in addition to the request that opens it. That
  has been run against a local emulator and a test server, not the real
  service.
- Downloads ask for the object as stored (`Accept-Encoding: gzip`) and
  decompress it locally if it was stored compressed.
- After Ctrl-C the state can be uploaded twice: once as it was at the last
  background snapshot, and once with everything completed since.

### 4. Memory and CPU

- Peak memory is several times lower.
- Decoding and encoding use every CPU, in short bursts. `-parallelism`
  limits provider operations as before; it doesn't limit these.
- For states with at least 2,000 resource instances, Terraform explicitly
  returns memory to the operating system at three points in an operation.
  Each costs a few tenths of a second.

### 5. Errors that are reported differently

- **A saved plan whose record of an unchanged resource can't be decoded.**
  Upstream rejects the plan before applying anything. This build decodes
  those values only when something needs them, and reports a problem as
  `Invalid planned change` at the end of the apply, or not at all if nothing
  needed them. Values of resources that are actually changing are still
  decoded and checked before anything is applied.
- **A failed intermediate snapshot.** Upstream halts the apply at the
  resource whose update couldn't be saved. This build halts at the next
  resource to finish, and names that one. If no other resource finishes, the
  failure isn't reported; the final write, which is always attempted,
  decides whether the apply succeeded.
- **A state file edited by hand** so that `dependencies` isn't a list of
  strings: still an error, worded differently.

### 6. Additions

- `TF_GCS_STATE_GZIP=1` makes the GCS backend store each state snapshot
  gzip-compressed, labelled so that Google serves it decompressed to any
  client that doesn't ask for the stored form. It is off by default because
  it changes how the object is stored for everything else that reads the
  bucket. A compressed state is always read correctly, whether or not the
  variable is set. It has only been run against an emulator.
- For code that embeds Terraform: hooks' `PostStateUpdate` can now be called
  concurrently, while a read lock on the state is held, and is given the
  state itself rather than a copy of it. A hook must not modify the state
  or keep a reference to it after returning.
- `terraform version` prints upstream's version number, and in a published
  image a line after it, beginning `fork:`, that says which commit was built
  and which upstream commit it's based on. The JSON form of the command is
  unchanged, and so is the version recorded in state files, which is always
  upstream's.

## Compatibility

- **Plans saved by the full fork** of the same upstream version are applied
  by this build, and the proof checks that the result is what upstream gives
  when it applies the same plan. Both builds report upstream's version, which
  is what a plan records.
- **State files** are format version 4, as upstream's are. Stock Terraform
  reads state written by this build, and this build reads state written by
  stock Terraform.
- **Providers, lock files, modules, configuration and CLI** are unchanged.
- **Going back** to stock Terraform, or to the full fork, needs no migration.

## How it was verified

### The proof

[`fork-proof/`](fork-proof/README.md) builds upstream at the commit this
build is based on, this build, and the full fork at the head of
`perf/v1.17.0-rc1`. It runs them through a corpus of scenarios and
generated inputs, compares what they produce, stops this build part way
through applies, and writes a report. Each scenario is planned and applied
in eight ways, among them a plan saved by the full fork applied by this
build, and the same plan applied by upstream. Its README says what it does
and doesn't establish.

Last checked: the full fork at `81041ca42e41`, whose image is
`ghcr.io/cozybear-dev/terraform:perf-v1.17.0-rc1-81041ca42e41`
(`sha256:c504794b3d71d594791c7028c029b6df24fe0488ee9baec839e08de9ca3a5ef3`).

### Tests

The patches bring their own tests, in files of their own beside upstream's:
the state encoder and decoder, the JSON to value decoder, the dependency
list reader and writer, the address formatter and the mark comparison are
each tested against the routine they replace, on generated and deliberately
corrupted input, with fuzz targets for the encoders and decoders; and the
index of modules in the state is compared with a search of all the modules
after random modifications. Upstream's own tests pass, and the race
detector runs on the packages that work concurrently.

### Review

The full fork's `README-FORK.md` describes the independent review of the
full fork, and the review pass that read every changed file for data
integrity alone, and what they covered. This build has no code of its own:
every line is from the full fork's patches, unchanged, and 6a and 15r are
halves of two of them.

## Known limits and open issues

- Nothing here has been run against a real state of the size it was built
  for. All measurements are on generated workloads.
- The GCS changes have only been run against a local emulator and
  in-process test doubles, not Google Cloud Storage itself.
- Only the local and GCS backends were examined.
- Not tested on Windows or on 32-bit platforms.
- Plans must come from the full fork of the same upstream version, at a
  commit this build's proof has checked.
- After a hard stop, more created objects can be missing from the state than
  with upstream (difference 1).
- A saved plan with an undecodable no-op change isn't rejected up front,
  and a failed intermediate snapshot can go unreported (difference 5).
- A kill in the middle of writing a local state file leaves a partial,
  unreadable file, as it does upstream.
- Compared with the full fork, an apply is 2 to 5 seconds slower and holds
  about 0.5 GB more, on the benchmark's state.

## Where the changes are

| Area | Packages |
|---|---|
| State files | `internal/states/statefile` |
| State in memory | `internal/states` |
| State managers | `internal/states/remote`, `internal/backend/remote-state/gcs` |
| Saving state during apply | `internal/backend/local` (`hook_state.go`), `internal/terraform` (`update_state_hook.go`) |
| Plan files | `internal/plans`, `internal/plans/planfile` |
| The walk and evaluation | `internal/terraform`, `internal/addrs`, `internal/lang/marks` |
| Value decoding | `internal/lang/fastjson` |
| Helpers | `internal/parallel`, `internal/logging` |
| Version | `version/fork.go`, `internal/command/views/version.go` |
| This document, the image and its workflow | `README-FORK.md`, `Dockerfile.fork`, `.github/workflows/fork-image.yml` |
| The proof and its workflow | `fork-proof/`, `.github/workflows/fork-proof.yml` |

### Use of `unsafe`

Four files use `unsafe`, each for one purpose:

- `internal/states/instance_object.go` and `instance_object_src.go`: to tell
  cheaply whether a value is the very same value that was decoded earlier,
  by comparing the four machine words of the two values. A check at start-up
  disables this if the value library's layout is not what is expected, and
  it can only ever answer "not the same" when unsure.
- `internal/states/statefile/version4.go`: to make the strings of a
  dependency list refer to the bytes they were parsed from, which are never
  modified, instead of copying each one.
- `internal/states/statefile/version4_encode.go`: to use the address of a
  slice's first element as a cache key.

## How it's maintained

It is kept as the full fork is: a branch for each version of upstream,
named for it, that is never moved once published, holding upstream, the
fixes of upstream's it needs, and one commit for each patch. A change
belongs in the commit of its patch: make it with `git commit --fixup`, and
fold it in when the stack next moves. Moving this build to a new version of
upstream isn't automated yet; the full fork's **Fork rebase** workflow
moves only the full fork.

Nothing is published from a branch unless, for that commit, upstream's unit
tests and the race detector pass, the proof's verdict is `identical`, and
the image reports upstream's version. The proof also fails if this build
differs from upstream in any file that `fork-proof/FORK_FILES` doesn't
list, or changes or removes any test file or test data of upstream's.
`fork-proof/`, `.github/` and this file are what a person has to approve
changes to.
