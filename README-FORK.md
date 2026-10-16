# Terraform, large-state performance fork

This is an unofficial fork of [hashicorp/terraform](https://github.com/hashicorp/terraform)
that makes `terraform plan` and `terraform apply` much faster, and much less
memory-hungry, when the state is very large: hundreds of megabytes and tens of
thousands of resource instances.

It exists because, with a state of that size, most of the time of a plan or an
apply is not spent talking to providers. It is spent at the start and at the
end of each operation, in work that runs on one CPU or that grows with the
square of the number of resource instances: reading and writing state
snapshots, comparing them, building the graph, writing and rendering the plan,
and rewriting the whole state after every resource.

- It is kept as a stack of patches on top of a version of upstream, on a
  branch for each version: see [Branches and versions](#branches-and-versions).
  Moving it to a new version is one workflow, started with the tag.
- Not reviewed or endorsed by HashiCorp. Try it on a non-production workspace
  first, and read [What differs from upstream](#what-differs-from-upstream)
  before pointing it at state you care about.

## Contents

- [Getting it](#getting-it)
- [Branches and versions](#branches-and-versions)
- [Results](#results)
- [What changed and why](#what-changed-and-why)
- [What is the same as upstream](#what-is-the-same-as-upstream)
- [What differs from upstream](#what-differs-from-upstream)
- [Compatibility with stock Terraform](#compatibility-with-stock-terraform)
- [How it was verified](#how-it-was-verified)
- [Known limits and open issues](#known-limits-and-open-issues)
- [Profiling your own workload](#profiling-your-own-workload)
- [Where the changes are](#where-the-changes-are)
- [How the fork is maintained](#how-the-fork-is-maintained)

## Getting it

### Container image

Every push to one of the fork's branches builds an image for `linux/amd64`
and `linux/arm64` with [`.github/workflows/fork-image.yml`](.github/workflows/fork-image.yml)
and publishes it to the GitHub Container Registry. Nothing is published
unless three things pass for that commit:

- upstream's own unit tests, for every Go module in the repository;
- the race detector, on the packages upstream runs it on and the ones the
  fork changed;
- the [integrity proof](fork-proof/README.md), which builds upstream at the
  commit the fork is based on and compares what the two do.

The proof's report is attached to the image as a signed attestation, and so
is a statement of which commit and workflow built it:

```sh
gh attestation verify oci://ghcr.io/cozybear-dev/terraform@<digest> --repo cozybear-dev/terraform
gh attestation verify oci://ghcr.io/cozybear-dev/terraform@<digest> --repo cozybear-dev/terraform \
  --predicate-type https://cozybear.dev/terraform/fork-proof/v1
```

| Tag | Meaning |
|---|---|
| `ghcr.io/cozybear-dev/terraform:<branch>` | Latest build of a branch, with `/` written as `-`, so `perf-v` and the version of upstream |
| `ghcr.io/cozybear-dev/terraform:<branch>-<commit>` | One specific commit (first 12 characters of its hash) |

In the examples below, `<tag>` stands for one of those. How the branches are
named is in [Branches and versions](#branches-and-versions).

The image is laid out like the official `hashicorp/terraform` image: Alpine
with `git` and an SSH client, and `/bin/terraform` as the entrypoint. It can
stand in for that image.

```sh
docker run --rm ghcr.io/cozybear-dev/terraform:<tag> version

docker run --rm -it \
  -v "$PWD":/workspace -w /workspace \
  -v "$HOME/.config/gcloud":/root/.config/gcloud:ro \
  ghcr.io/cozybear-dev/terraform:<tag> plan -out=tfplan
```

The binary is statically linked, so it can also be copied out of the image
and run directly on any Linux machine of the same architecture:

```sh
id=$(docker create ghcr.io/cozybear-dev/terraform:<tag>)
docker cp "$id":/bin/terraform ./terraform-fork && docker rm "$id"
```

A plan must be applied by the same build that made it, so use the same tag
for `plan` and `apply`. Pin the per-commit tag for anything repeatable.

### From source

```sh
go build -o terraform .                      # needs the Go version in .go-version
docker buildx build -f Dockerfile.fork --platform linux/amd64,linux/arm64 .
```

## Branches and versions

Each version of upstream that the fork has been carried to has a branch of
its own, named for it: the branch `perf/v1.17.0` is the fork on upstream's
tag `v1.17.0`, its images are tagged `perf-v1.17.0`, and `terraform version`
in them says `Terraform v1.17.0` as upstream's own build of that version
does, with a line after it that says which commit of the fork was built. The
newest such branch is the one to use. The exact upstream commit that a
branch is on is in its `fork-proof/UPSTREAM_REF`, with any
[later fixes of upstream's](#fixes-of-upstreams-that-a-version-lacks) that
it carries in `fork-proof/UPSTREAM_BACKPORTS`.

- **A branch for a version is never moved** to another version. It is the
  fork as [a stack of patches](#how-the-fork-is-maintained) on that version,
  and when upstream releases the next, the fork gets a new branch.
  [One workflow](#moving-to-a-newer-version-of-upstream) does that.
- **`perf/large-state` is gone.** It is where the work was first done, on
  upstream's development branch as it was at the time (`main` at
  `0828b3d2d7cb`, which calls itself `1.18.0-dev`). It was retired when the
  first branch for a version, `perf/v1.17.0-rc1`, was published, and has
  been deleted since. An image tagged `perf-large-state` is from it, and
  nothing new is published under that name: its last one,
  `perf-large-state-f507e5f186b0`, has the same changes as
  `perf/v1.17.0-rc1` had when that was first published, on a different
  upstream.
- **There is no branch for 1.16**, and none is planned. The fork's graph
  patches are written for the graph package as upstream rewrote it for
  1.17, and 1.16 has the earlier one, whose walker also does things that
  later versions dropped.
  The first six patches apply to `v1.16.5` as they are. The two for the
  graph package don't, and would have to be written again for it; the
  patches after those weren't tried.

## Results

Measured on one machine (linux/arm64, 12 cores) with a provider that has no
remote API, so that the numbers show Terraform itself. Each cell is wall
time and peak memory. State is in a local file unless stated.

| Scenario | Upstream | This fork |
|---|---|---|
| Plan, 358 MB state, 30,600 instances, one change | 124 s, 9.9 GB | 11 s, 2.0 GB |
| Plan with `-refresh=false`, same state | 105 s, 7.7 GB | 9 s, 1.8 GB |
| Apply of that saved plan | 117 s | 3 to 5 s, 1.8 to 2.1 GB |
| Plan, 196 MB state, 15,000 separate resources with long dependency lists | 92 s | 7.9 s, 1.5 GB |
| Plan, 90 MB state | 22.9 s | 3.4 s, 0.6 GB |
| Apply, 90 MB state | 22.5 s | 1.4 s, 0.6 GB |
| Plan with `-refresh=false`, 56,000 instances on the real Vault and Google providers | 190 s, 3.6 GB | 34 s, 1.4 GB |
| Apply of that saved plan (one change) | 25.8 s, 3.9 GB | 2.4 s, 1.0 GB (see note) |
| `init` installing 300 modules from a bucket, 60 ms per request | 98 s | 3.5 s |
| Apply creating 504 instances in 96 module instances (24 module calls, each passing 30 variables to 3 nested modules with 25 locals and 26 outputs) | 45 s | 1.0 s |
| Plan of a change to that configuration (432 updates) | 23.5 s | 0.8 s |
| Plan with `-refresh=false`, the same configuration with 1,000 module calls: 4,000 module instances, 21,000 instances, 208 MB state | not run | 25 s, 2.0 GB |
| The same with `TF_SKIP_VALIDATE=1` | | 18 s |
| `show -no-color` of a saved plan, 30,000 instances on the real Vault and Google providers in 5,000 module calls, each with a module directory of its own (on GitHub's runners) | 21.5 s | 7.7 s (10.9 s before patch 33) |
| `show -no-color` of a saved plan, 56,000 instances on the real Vault and Google providers in 100 module calls (on GitHub's runners) | 20.3 s | 1.3 s |

Notes:

- Upstream's apply on the 358 MB state had to run under a memory limit to
  fit on the machine, which makes it slower than it would be with more
  memory.
- Of the fork's 3 to 5 seconds for that apply, 1 to 3 seconds is writing the
  358 MB local state file and its backup and syncing them to disk. That part
  varied a lot from run to run on this machine.
- The Vault and Google apply was measured before the last change to how the
  local state file is written (see difference 3 below). Expect it to be a
  little slower than shown.
- The module-heavy configuration is large in configuration rather than in
  state. Upstream's time for it grows much faster than the configuration
  does, which is why the 1,000-call version was not run with upstream: the
  24-call version already takes 45 seconds. Before the changes for it, this
  fork needed more than ten minutes for the 1,000-call plan.

What is left in the fork's plan numbers is almost entirely the graph walk,
and during the walk the workers are waiting for the providers. On the Vault
and Google run, about 9 of the 10 default workers are inside a provider call
at any moment, and at `-parallelism=100` about 98 of 100.

The `show` rows were measured by the **Fork benchmark** workflow on the
repository's default branch, on GitHub's 4-CPU linux/amd64 runners. A
module call whose source is a registry or a bucket gets a module directory
of its own, and what `show` does with those grew with the square of their
number until patch 33. In a configuration with many more of them than the
benchmark has, that was most of a minute.

These are synthetic workloads. They were built to resemble a real large
state, but they are not one.

## What changed and why

### Reading state, at the start of every operation

**Why:** upstream reads a state file on one CPU, with the standard JSON
decoder validating the whole document and then scanning each object's
attributes a second time, and an operation reads the state more than once.

- The file is split at resource boundaries and the resources are decoded on
  all CPUs. For a large file the boundaries themselves are found
  concurrently, with each scan confirming that the next one started in the
  right place, so the result is the same as a sequential scan.
- Each resource is decoded by a purpose-built decoder that validates every
  value as it finds the end of it. It accepts only the plain form that
  Terraform itself writes. Anything else, including anything invalid, is
  handed to the standard decoder exactly as before.
- Dependency lists are decoded, and their addresses parsed, in that same
  concurrent phase. Objects with identical dependencies share one list.
- The local state file is read once per operation rather than twice.

### Building the graph

**Why:** transitive reduction and dependency attachment were quadratic or
worse in the number of nodes, and adding and removing edges one at a time
dominated the apply-graph build for a plan with many changes.

- Transitive reduction and the attachment of dependencies to resources are
  indexed, single-pass implementations, and the reduction runs on all CPUs.
- References are resolved for all nodes concurrently and edges are added and
  removed in bulk.
- Checking a graph for self-references no longer builds a list of every edge.
- The reduction no longer explores everything below a node to find out which
  of its edges are redundant. Each node is labelled with two ranges that
  everything reachable from it falls within, and a search abandons any
  branch whose range cannot contain a target that is still wanted.
- Cycle detection keeps a set of the nodes on its stack instead of searching
  the stack for each edge, which was quadratic in the depth of the graph.
- Connecting the nodes of each module to the node that expands it, and the
  nodes that close modules to each other, uses an index of nodes by module
  instead of looking through every node in the graph for every module.
- The table of references that data resources use for `depends_on` is only
  built if the graph contains a data resource.
- Log messages that nothing will receive are no longer formatted.

The reduction, the cycle detection and the module index, together with the
index of output changes described below, took the plan of a configuration
with 1,000 chained module calls from more than ten minutes to under a
minute.

### Walking the graph

**Why:** several things were done once per resource instance that cost time
in proportion to the size of the whole resource, or of the whole state.

- The walker starts a goroutine for a node when its dependencies have
  completed, and lets that goroutine carry on with a node that was waiting
  only for it. Upstream starts two goroutines for every node in the graph
  before it begins, each waiting on channels and timers, and the garbage
  collector rescans all of their stacks every time it runs. In the 1,000
  module call benchmark that alone was 7 seconds and 1.5 GB.
- Planned changes are indexed instead of being scanned for every lookup.
  That now includes output values, which upstream looks for by comparing
  the string form of every recorded output, once for each output.
- The values of variables, locals and outputs each have their own
  read/write lock rather than sharing one lock, and the key for a value is
  built before the lock is taken rather than while holding it.
- The instances of a module are found by their keys rather than by looking
  through every module instance beside them, and the answer for a module is
  remembered, since it cannot change once it can be given at all. Every
  node that belongs to a module asks for it.
- Each resource in the configuration looks in the previous state for
  instances that no longer have a module instance to belong to. Upstream
  does that by looking through every module in the state, while holding the
  lock that stops anything else reading or writing that state. With many
  thousands of modules in the state the whole walk queued behind that one
  search: in a profile of a real plan, nine in ten of the running nodes were
  waiting for that lock, and the walk was 156 of the plan's 239 seconds. The
  search now uses an index of the state's modules, which is rebuilt only
  when a module is added or removed, and holds the lock in the way that
  lets other readers continue.
- Working out the order of `moved` statements compares every statement with
  every other one, twice per plan. That now uses all CPUs, and finding the
  moves implied by adding or removing `count` uses the same index of
  modules rather than a search of the state for each module.
- A resource's `for_each` value is evaluated once per walk instead of several
  times per instance.
- The value of a resource, as seen by expressions that refer to it, is built
  once and reused until that resource changes. When many instances ask at
  the same moment, one builds it and the rest wait.
- Each graph node is given the state of its own instance, not a copy of the
  state of the whole resource.
- An object that refresh and plan leave unchanged keeps the value it already
  had, and is serialized once rather than each time it is stored.
- Values are encoded for providers, and for the plan, by a faster msgpack
  encoder that produces the same bytes as the library.
- The implied type of each schema block and the function table of each
  module are cached.

### Saving state while applying

**Why:** upstream writes the entire state after every resource instance
finishes, while holding the lock on the state. For a 400 MB state that is a
400 MB write per resource, and nothing else that needs the state can proceed
while it happens. This single behaviour made apply time quadratic.

- For a large state, snapshots are written by a background task on a timer.
  The interval is nine times the time the previous snapshot took to write,
  so writing them takes about a tenth of the run.
- With a remote backend, the periodic upload of the state (every 20 seconds
  by default) is done by that background task too. Upstream does the upload
  inside the state update of whichever resource happens to finish next,
  while holding the lock on the state, so every other resource waits for the
  upload to complete.
- The final state is always written, in full, at the end.
- On interrupt (Ctrl-C), the latest state is written immediately, and from
  then on every update is written before the next one proceeds, as upstream
  does.

This is the most important behaviour change in the fork. It is described
under [What differs from upstream](#what-differs-from-upstream).

### Writing state, at the end

**Why:** upstream serializes a state on one CPU, with the standard library
validating, compacting and then re-indenting each object's attributes in
separate passes, and it decides whether a state has changed by serializing
both versions in full.

- Objects are encoded on all CPUs, in windows, so that the encoded form of
  the whole state is never in memory at once.
- Attributes are validated, escaped and indented in a single pass.
  Dependency addresses are written straight into the output.
- Whether a state has changed is decided from a digest, or from a cheap
  structural comparison that recognizes shared data, instead of serializing
  both states.

### Plan files and plan output

**Why:** a plan file holds the plan and two complete snapshots of the state.
Compressing those on one CPU dominated the time taken to save a plan, and
rendering did work for every unchanged resource that is never shown.

- Plan file members are compressed in parallel chunks.
- When refreshing found nothing to update, the two snapshots differ only in
  a few lines of metadata. Their common body is serialized and compressed
  once when writing, and parsed once when reading.
- Planned changes are encoded, decoded and converted concurrently.
- When applying a saved plan, the values of no-op changes are decoded only
  if something refers to them.
- The human-readable plan is computed and rendered in parallel batches, and
  nothing is computed for no-op changes.
- `terraform show` of a saved plan, and applying one, load the
  configuration from the copy of it inside the plan file. Finding each file
  in that copy meant looking through every module of the configuration, so
  the time grew with the square of the number of modules, and a module
  call whose source is a registry or a bucket has a module of its own. With
  thousands of them that was most of the time `terraform show` took. A
  table from each module's directory to the module, built once, replaces
  the search. It gives the same answers. Where two modules have the same
  directory, which is the case when module calls share a local module and
  then they have the same files, it takes the one that comes first by name,
  where upstream took whichever it came to first.

### Remote state in Google Cloud Storage

**Why:** with the state in a bucket, the start and end of every operation
include downloading and uploading the whole state.

- A state larger than 16 MiB is uploaded in one request instead of in 16 MiB
  chunks that each wait for the previous one. For a 358 MB state that is one
  request instead of about 23. Against a local emulator this took the final
  save of an apply from 8.2 s to 2.0 s.
- The state is downloaded once per operation instead of twice. Before the
  second download, the fork asks for the object's generation number and
  skips the download if it has not changed.
- A state of 64 MiB or more is downloaded as up to eight ranges at once
  rather than as one stream, because a single download from Cloud Storage is
  limited to some tens of megabytes per second however fast the network is.
  Every range asks for the same generation of the object, and the result is
  used only if its CRC32C is the one that the service reports for the whole
  object. If a range fails, or the checksum differs, the state is read as
  one stream exactly as before.
- Optionally, the state can be stored gzip-compressed. See
  [Optional settings](#optional-settings).

### Installing modules from Google Cloud Storage (`terraform init`)

**Why:** `init` installs modules strictly one after another, and for a
module in a bucket nearly all of that time is waiting for the network. For
every module, upstream also creates a new storage client, which has to
authenticate and connect again, and downloads the files of a directory one
at a time. With hundreds of modules the waits add up to minutes.

- All module downloads share one authenticated storage client.
- The files of a module that is a directory are downloaded eight at a time.
- As soon as a module has been loaded, the bucket modules that it calls
  start downloading in the background, up to sixteen at a time. The modules
  are still installed in turn, exactly as before; each one just finds its
  package already downloaded, or on its way.

This applies to module calls whose `source` is a literal `gcs::` address.
The installed files and the module manifest are identical to upstream's.

### Memory

**Why:** peak memory, not just time, decides whether a large state can be
planned at all on a given machine.

- Copies of a state share the bytes of unchanged objects and their
  dependency lists instead of duplicating them. Terraform holds several
  copies of the state at once.
- Decoded values are dropped as soon as only their serialized form is still
  needed.
- Buffers for reading state files and for encoding objects are reused.
- For a large state, memory that is no longer needed is returned to the
  operating system at three points: after loading, after planning, and
  before the final state is written.

### Profiling

Environment variables that write profiles of a run. See
[Profiling your own workload](#profiling-your-own-workload).

## What is the same as upstream

The rule followed throughout is that the fork must produce exactly what
upstream produces, and that every fast path falls back to upstream's own
code for any input it does not recognize.

For the same configuration, state and provider responses, these are intended
to be identical, and were checked to be (see
[How it was verified](#how-it-was-verified)):

- The bytes of a state file, apart from `serial` (see below).
- The backup file `terraform.tfstate.backup`.
- The human-readable plan and the JSON plan (`terraform show -json`).
- The content of every member of a plan file except the plan message
  itself, which says the same thing with its fields in a different order
  (difference 4).
- What is planned and applied.
- Errors and diagnostics, with the exceptions listed below.
- Locking, lineage, and how conflicts between writers are detected.

Small states take the same code paths as upstream wherever the difference
could matter. In particular a state with fewer than 2,000 resource
instances is saved after every resource exactly as upstream saves it,
however long that takes.

## What differs from upstream

Differences 1 to 3 are about what is on disk if something goes wrong. Read
those first.

### 1. Intermediate state snapshots during apply

This applies once the state has at least 2,000 resource instances, and only
then. How long a snapshot takes to write plays no part in the decision. (An
earlier version of the fork also applied it once a write had taken 5 ms,
which on an ordinary disk is every write, because writing a local state
file includes syncing it to disk.)

| | Upstream | This fork |
|---|---|---|
| After each resource instance is applied | The whole state is written before anything else proceeds | The state is marked as changed; a background task writes a snapshot of the latest state |
| First snapshot | After the first resource | After about as long as a snapshot is expected to take to write (30 µs per instance, at most 3 s) |
| Later snapshots | After every resource | Starting nine times the previous write's duration after it finished, if anything changed |
| On Ctrl-C / SIGINT | Latest state persisted; every later update written | The same: pending changes are written at once, then every update is written before the next proceeds |
| On an error from a provider | Final state written at the end | The same |
| At the end | Final state written | The same; the background task is stopped first, so it cannot overwrite the final state |

**After a graceful stop nothing is missing,** with either build. That
covers an error from a provider, a failed apply, and one Ctrl-C.

**After a hard stop, the fork's state can be further behind.** A hard stop
is anything that gives Terraform no chance to react: `kill -9`, an
out-of-memory kill, a CI job timeout, a second Ctrl-C, a panic, or the
machine losing power.

- *Local state file.* Upstream's file is current to the last completed
  resource. The fork's file is current to the last background snapshot, so
  it can be missing everything completed in about eleven times the duration
  of one snapshot write. On the test machine one write of a 358 MB file,
  including the sync to disk, took 1.4 to 3.2 seconds, so that is roughly
  15 to 35 seconds of completed work.
- *Remote state.* Upstream itself only uploads an intermediate snapshot
  every 20 seconds. The fork uploads on the same schedule, from the
  background task.

Resources that were applied but are missing from the state are not lost in
the provider: they exist, and Terraform no longer knows about them. Recovery
is the same as for upstream after a hard stop (import, or remove by hand),
but there can be more of them.

**Measured.** An apply to a 15 MB state, with a provider that takes 40 ms
per resource, was killed with `kill -9` at arbitrary moments:

| | Kills | State file afterwards |
|---|---|---|
| This fork | 12 | 11 readable, missing between 0 and 40 completed resources; 1 partial file that Terraform refuses to read; none empty |
| Upstream | 3 | Empty (0 bytes) every time |

Upstream rewrites the file almost continuously, and empties it at the start
of each rewrite, so a kill usually lands while it is empty. Terraform reads
an empty state file as "there is no state", not as an error. In every case
the backup file still held the state from before the apply.

If your state is several hundred megabytes and in a bucket, each
intermediate upload is the whole state. Consider raising the interval with
upstream's own `TF_STATE_PERSIST_INTERVAL` (in seconds; values below 20 are
ignored).

### 2. The `serial` in the state

Upstream increments `serial` each time it writes a snapshot that differs
from the one before, which during an apply is once per resource. The fork
writes fewer intermediate snapshots, so the `serial` after an apply is lower
than upstream's would be. It still increases with every write that changes
the state, which is all that Terraform relies on.

### 3. When the local state file is emptied

Both builds empty the state file and then write the new snapshot into it, so
that an interrupted write can only leave the beginning of the new snapshot,
which Terraform refuses to read.

- Upstream empties the file first and then prepares the snapshot. The file
  is empty for as long as preparing takes.
- The fork prepares first and empties the file as it writes the first bytes.
  Until then the file still holds the previous snapshot. If preparing the
  snapshot fails, the previous snapshot is left in place; upstream leaves
  the file empty.

Also, the fork reads the existing file for the backup when the state is
first refreshed or written, rather than when the file is opened. A state
file that cannot be read is therefore reported at the first refresh instead
of while taking the lock.

As with upstream, the backup file is written before the state file is
touched.

### 4. Plan files are compressed differently

A plan file is still a standard zip archive. The two state snapshots, the
configuration and the lock file decompress to exactly what upstream would
have stored. The plan message decompresses to a different sequence of bytes
that means the same thing. The bytes of the archive differ:

- Members are compressed in independent 1 MiB chunks.
- The two state snapshots can share their compressed body.
- The plan and the two state snapshots are stored without a trailing data
  descriptor or an extended timestamp, and with their modification time in
  UTC, where upstream and the other members use local time. Nothing reads
  those times.
- Resource changes come last inside the plan message, which is written as
  several messages one after another. Protocol buffers define that as the
  same message, and upstream reads it as such.

Upstream Terraform of the same version reads these files. Do not expect two
builds to produce plan files with the same checksum.

### 5. Google Cloud Storage backend

- A state over 16 MiB is uploaded in a single request. If that fails with an
  error worth retrying, the whole upload is retried up to three times, with
  pauses of one and two seconds, and then the original chunked upload is
  used. States of 16 MiB or less are uploaded exactly as before.
- One fewer download of the state per operation, and one extra metadata
  request for the object's generation.
- A state of 64 MiB or more is read with up to eight range requests for one
  generation of the object, in addition to the request that opens it. The
  parallel download has been run against a local emulator and a test
  server, not the real service. If it does not help on your network, it
  costs nothing but the extra requests.
- Downloads ask for the object as stored (`Accept-Encoding: gzip`) and
  decompress it locally if it was stored compressed.
- After Ctrl-C the state can be uploaded twice: once as it was at the last
  background snapshot, and once with everything completed since.

### 6. Installing modules from GCS

- Bucket modules are downloaded concurrently and ahead of when they are
  installed, so more requests are in flight at once, and a module can be
  downloaded even though `init` then stops with an error before reaching it.
  Such downloads go to `.terraform/modules/.prefetch`, which is removed when
  `init` finishes.
- Nothing is downloaded ahead when a policy hook that can refuse module
  sources is active, or for sources that are expressions rather than literal
  strings; those are fetched in turn as upstream does.
- One storage client, and so one set of credentials, is used for all module
  downloads in a run. Upstream looks the credentials up again for every
  module.
- Other kinds of module source (git, registry, HTTP, S3, local) are
  untouched.

### 7. Memory and CPU

- Peak memory is several times lower.
- Encoding, decoding, compression and rendering use every CPU, in short
  bursts. `-parallelism` limits provider operations as before; it does not
  limit these.
- For states with at least 2,000 resource instances, Terraform explicitly
  returns memory to the operating system at three points in an operation.
  Each costs a few tenths of a second.

### 8. Errors that are reported differently

- **A saved plan whose record of an unchanged resource cannot be decoded.**
  Upstream rejects the plan before applying anything. The fork decodes those
  values only when something needs them, and reports a problem as
  `Invalid planned change` at the end of the apply, or not at all if nothing
  needed them. Values of resources that are actually changing are still
  decoded and checked before anything is applied.
- **A failed intermediate snapshot.** Upstream halts the apply at the
  resource whose update could not be saved. The fork halts at the next
  resource to finish, and names that one. If no other resource finishes, the
  failure is not reported; the final write, which is always attempted,
  decides whether the apply succeeded.
- **A state file edited by hand** so that `dependencies` is not a list of
  strings: still an error, worded differently.

### 9. Evaluation details

- A `for_each` expression is evaluated once per module instance per walk.
  If its values contain a function that returns something different each
  time, such as `uuid()` or `timestamp()`, every use sees the same result.
  Upstream evaluates it several times and can see several results.
- When a provider returns an object equal to the one already in the state,
  the fork carries on with the one from the state. A number in it can then
  be sent to the provider later in a different encoding of the same number,
  for example as a decimal string instead of a float. Upstream does the
  same whenever `-refresh=false` is used. State and plan output are
  unaffected.

### 10. Trace logging

With `TF_LOG` set, some of the faster graph-building paths are not used,
because the log is expected to describe each step in order. Results are the
same; a run with logging enabled is slower than one without, by more than it
is upstream.

The message that says which dependency a node is still waiting for, every
five seconds, now comes from one reporter for each walk, including the walk
of each dynamically expanded subgraph, rather than from a timer for every
node, and is only produced when logging is enabled.

### 11. Additions

- Profiling variables: `TF_PROFILE_DIR`, `TF_PROFILE_PHASES`,
  `TF_PROFILE_WALL`, `TF_PROFILE_HEAP`, `TF_PROFILE_TRACE`,
  `TF_PROFILE_MUTEX`.
- `TF_GCS_STATE_GZIP` and `TF_SKIP_VALIDATE`, described next.
- `internal/provider-bench`, a provider with no remote API, used only for
  benchmarks. It is not part of the `terraform` binary.
- For code that embeds Terraform: hooks' `PostStateUpdate` can now be called
  concurrently, while a read lock on the state is held, and is given the
  state itself rather than a copy of it. A hook must not modify the state
  or keep a reference to it after returning; upstream's hooks could.
- `terraform version` prints upstream's version number, and in a published
  image a line after it that says which commit of the fork was built and
  which upstream commit it's based on, beginning `fork:`. The JSON form of
  the command is unchanged, and so is the version recorded in state and
  plan files, which is always upstream's. A build made without the linker
  flag that sets that line, such as `go build` from source, prints exactly
  what upstream does.

### Optional settings

**`TF_SKIP_VALIDATE=1`** (or `true`) makes `plan`, `apply` and `refresh`
leave out the validation of the configuration that they otherwise begin
with, including when applying a saved plan. Any other value, such as `0`,
leaves validation on. Validation builds and walks a graph of the whole
configuration before the operation builds and walks its own, and for a very
large configuration that is a third of a plan.

It is off by default because it removes checks, and **it should only be
used for a configuration that `terraform validate` has already passed**, for
example in an earlier pipeline step. Without validation:

- **Some errors move from before the apply to the middle of it.** An invalid
  argument to a provisioner passes the plan, and the apply then fails after
  the resource has been created, leaving it tainted so that the next apply
  replaces it.
- **Some mistakes are accepted silently.** A resource's `depends_on` that
  names a resource that isn't declared is ignored, so the ordering it was
  meant to give is lost. An `ignore_changes` entry that names an attribute
  that doesn't exist is ignored too.
- **Warnings that only validation gives are not shown**, such as a redundant
  `ignore_changes` entry.
- **Nothing inside a resource or module with no instances is checked**, so a
  bad reference under `count = 0` goes unnoticed until it has instances.

The operation still reports the problems it runs into itself: everything
the providers say about each resource's configuration, unsupported
attributes in expressions, an output with a sensitive value that isn't
marked sensitive, and a `depends_on` on an output that names something
undeclared. With the variable set, the phase report has no
`load configuration` line and that time is counted in the next one.

**`TF_GCS_STATE_GZIP=1`** makes the GCS backend store each state snapshot
gzip-compressed, labelled so that Google serves it decompressed to any
client that does not ask for the stored form. A 358 MB state is stored as
22 MB. It is off by default because it changes how the object is stored for
everything else that reads the bucket.

- A compressed state is always read correctly, whether or not the variable
  is set. Unsetting it returns to uncompressed writes at the next write.
- On a fast link it roughly breaks even, because decompression runs on one
  CPU. It pays off when the network is the bottleneck.
- It has only been run against a local emulator of GCS, not the real
  service. Try it on a non-production bucket first, and check that
  `terraform state pull` with stock Terraform still works for you.

## Compatibility with stock Terraform

- **State files** are format version 4, as upstream's are. Stock Terraform
  reads state written by the fork, and the fork reads state written by stock
  Terraform. The state records `terraform_version` as the version of upstream
  that the branch is based on, as upstream's own build of that version does.
  Terraform does not refuse a state for having a newer version than its own,
  but anything upstream has changed between the release you use and the one
  the branch is based on applies here too.
- **Plan files** must be applied by the build that created them. This is
  upstream's rule: the plan records the version, and a build of the fork
  has the version of the upstream it's based on.
- **Providers, lock files, modules, configuration and CLI** are unchanged.
- **Going back** to stock Terraform needs no migration.

## How it was verified

### Tests

- **Differential tests.** Each replacement for a library routine is tested
  against that routine on generated and deliberately corrupted input: the
  state encoder and decoder, the JSON indenter and validator, the JSON to
  value decoder, the msgpack encoder, the dependency list reader and writer,
  the address formatter, the mark comparison, the checksum combiner, and
  the parallel compressor.
- **Round trips.** Seven state files, including ones holding resources of
  the real Vault, Google and AWS providers, are read and written back byte
  for byte, and the concurrent reader gives the same result as the
  sequential one.
- **End to end against upstream.** A scenario with 1,621 updates and 180
  deletions on a 15 MB state is planned and applied by both builds. The
  human-readable plan, the JSON plan, the backup file and the final state
  (apart from `serial`) are identical. Upstream reads the fork's plan file
  and gives the same JSON, and finds no changes when planning against state
  written by the fork.
- **Real providers.** On 45,002 resources managed by the Vault and Google
  providers, the plan output and JSON are identical to upstream's, and
  applying the same change with each build gives the same final state.
- **Module-heavy configuration against upstream.** A configuration of 24
  chained module calls (96 module instances, 504 resource instances) is
  created, changed (432 updates) and applied by both builds. The states
  after each apply, the human-readable plan and the JSON plan are identical
  apart from `lineage`, `serial`, the timestamp and the random ids of the
  `terraform_data` resources, and upstream reads the fork's plan file and
  gives the same JSON.
- **Removed module instances against upstream.** A configuration of
  `count` and `for_each` modules with nested modules is applied, and then
  planned and applied with fewer instances, so that 95 objects are destroyed
  because their module instance is gone. The states and the human-readable
  plan are identical to upstream's, and the JSON plan is identical once the
  modules in `planned_values` are sorted, which upstream lists in an order
  that differs from one run to the next.
- **The index of modules in the state** is compared with a search of all the
  modules after each of about 12,000 random modifications, of every kind that the
  state allows.
- **Race detector** on the packages that do work concurrently.
- **Upstream's own test suite** passes.

### Comparison with upstream, repeatable

[`fork-proof/`](fork-proof/README.md) builds upstream at the commit this
fork is based on and the fork from source, runs both through a corpus of
scenarios and generated inputs, compares what they produce, stops the fork
part way through applies, and writes a report. It is what to run before
trusting a build, and what the claims in this section should rest on from
here on. Its README says what it does and doesn't establish.

Its first run found three differences from upstream that none of the
checks above had: two in how a value is written to the state, and one in
which state snapshot a plan file holds. All three involve values that are
equal but aren't written alike, and none can lose or damage an object. They
are described, with fixes, in the commits that added it.

### Independent review

The review described here covered everything up to the work on GCS modules.
The later changes for module-heavy configurations (the graph walker, the
graph-building indexes, and the locking of named values and module
expansions) have been tested as described above but have not been through
a review of this kind.

After the work was finished, a separate review pass read every changed
source file with data integrity as its only priority, fuzzed the hand-written
encoders and decoders against the standard library (1.0 to 5.8 million
inputs each, no differences), killed applies at arbitrary moments, and fixed
what it found. In normal operation it found no difference from upstream.
Everything it found was in failure and interrupt paths:

| Severity | Problem | Fix |
|---|---|---|
| Critical | The local state file was overwritten in place and shortened afterwards. A kill part-way left the start of the new snapshot followed by the rest of the old one, and about one time in five that mixture was valid JSON: a state that never existed | `cd5419c6a3`, `fa8d2b465d`. See difference 3 |
| Important | On Ctrl-C, a resource finishing at the wrong moment had its update overwritten by a slightly older snapshot | `001d623996` |
| Important | On Ctrl-C, the apply could hang waiting for a background snapshot | `d8b5c9cfe1` |
| Important | Data race between copying and sharing the state snapshots when applying a saved plan | `dea1e1ab56` |
| Minor | A damaged second snapshot in a plan file could be accepted when it matched the first | `cfcba22d6b` |
| Minor | A hand-made plan file with state content in an unexpected place gave both snapshots the same state | `8fde196df9` |
| Minor | An impossible size recorded in a plan file caused a crash instead of an error | `dddaa27cd3` |
| Minor | Data race on a change's action while its values were decoded | `f679f61043` |

Each fix has a test that fails without it.

## Known limits and open issues

Limits of what was tested:

- Nothing here has been run against a real state of the size it was built
  for. All measurements are on generated workloads.
- The GCS changes, for state and for modules, have only been run against a
  local emulator and in-process test doubles, not Google Cloud Storage
  itself. In particular, authentication is not exercised by an emulator.
- Only the local and GCS backends were examined. Other backends, including
  HCP Terraform, get the general improvements and the new way of saving
  state during apply, and were not tested with it.
- Not tested on Windows or on 32-bit platforms.
- Fuzz targets for the encoders and decoders are in the repository (see
  `go test -fuzz` in each package). The graph changes are compared with
  upstream on generated graphs by `fork-proof`, and aren't fuzzed with
  coverage guidance.
- A branch is on the version of upstream that it's named for, which can be
  a release candidate and not a final release. There is no branch for 1.16.

Known and not fixed:

- After a hard stop, a large local state can be further behind than
  upstream's would be (difference 1).
- A saved plan with an undecodable no-op change is not rejected up front,
  and a failed intermediate snapshot can go unreported (difference 8).
- A kill in the middle of writing the local state file leaves a partial,
  unreadable file, as it does upstream. Writing to a temporary file and
  renaming it would avoid that, and is not done.
- After Ctrl-C with a remote backend, the state can be uploaded twice.

Performance that remains:

- The time left is mostly the graph walk waiting for providers, which this
  fork does not change.
- A plan or apply in which every instance changes (30,000 at once) still
  takes about 20 seconds and 4 GB on the 358 MB benchmark state.

## Profiling your own workload

```sh
TF_PROFILE_PHASES=1 TF_PROFILE_DIR=/tmp/tfprof-plan \
  terraform plan -out=tfplan 2> plan-phases.txt

TF_PROFILE_PHASES=1 TF_PROFILE_DIR=/tmp/tfprof-apply \
  terraform apply tfplan 2> apply-phases.txt
```

| Variable | Effect |
|---|---|
| `TF_PROFILE_PHASES=1` | Prints the wall time of each phase to stderr as `[phase]` lines, with the number of CPUs that Terraform itself kept busy during it. A phase near 1.0 is running on one thread |
| `TF_PROFILE_DIR=<dir>` | Writes `cpu.pprof` and `heap.pprof` there. Required by the variables below |
| `TF_PROFILE_WALL=1` | Samples every goroutine every 100 ms, which shows time spent waiting (network, locks) that a CPU profile cannot |
| `TF_PROFILE_HEAP=1` or an interval such as `300ms` | Writes a heap profile periodically |
| `TF_PROFILE_MUTEX=1` | Writes lock contention and blocking profiles |
| `TF_PROFILE_TRACE=1` | Writes a Go execution trace |

In the container, mount a directory for the profiles and point
`TF_PROFILE_DIR` at it:

```sh
docker run --rm -it -v "$PWD":/workspace -w /workspace \
  -v /tmp/tfprof-plan:/prof -e TF_PROFILE_PHASES=1 -e TF_PROFILE_DIR=/prof \
  ghcr.io/cozybear-dev/terraform:<tag> plan -out=tfplan 2> plan-phases.txt
```

## Where the changes are

| Area | Packages |
|---|---|
| State files | `internal/states/statefile` |
| State in memory | `internal/states` |
| State managers | `internal/states/statemgr`, `internal/states/remote`, `internal/backend/remote-state/gcs` |
| Saving state during apply | `internal/backend/local` (`hook_state.go`) |
| Plan files | `internal/plans`, `internal/plans/planfile`, `internal/configs/configload` (`loader_snapshot.go`) |
| Module installation | `internal/getmodules`, `internal/initwd` |
| Graph | `internal/dag`, `internal/terraform` (`transform_*.go`, `graph_builder.go`) |
| Graph walk and evaluation | `internal/terraform`, `internal/lang` |
| Value encoding | `internal/lang/fastjson`, `internal/lang/fastmsgpack`, `internal/lang/marks` |
| Plan output | `internal/command/jsonplan`, `internal/command/jsonformat` |
| Helpers | `internal/parallel`, `internal/logging`, `profile.go` |
| Benchmark provider | `internal/provider-bench` |
| This document, the image and its workflow | `README-FORK.md`, `Dockerfile.fork`, `.github/workflows/fork-image.yml` |

A branch for a version of upstream has one commit for each area, listed in
[How the fork is maintained](#how-the-fork-is-maintained).

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

## How the fork is maintained

Upstream hasn't taken these changes, so the fork has to be carried from one
version of upstream to the next. It is kept in a form that makes that a
routine job, and that lets a machine do most of it without anyone having to
trust the machine: whether the result is right is decided by checks that
give the same answer whoever runs them.

### A stack of patches

A branch for a version of upstream is that version, then any fixes of
upstream's own that it lacks and the fork needs (see below), then one commit
for each area of the fork, 33 of them. Each builds, and passes `go vet`,
which compiles every test, on top of the ones before it.

| | Patch | Can be left out on its own |
|---|---|---|
| 1 | Add parallel.For, and a way to tell whether logging is enabled | No: nearly everything uses it |
| 2 | Add profiling hooks for investigating Terraform's own performance | Yes |
| 3 | Add a provider with no remote API, for benchmarks | Yes |
| 4 | Build the string forms of addresses with less work | No: 9 builds on it |
| 5 | Don't rebuild a value to find out that it has no marks | No: 9 builds on it |
| 6 | Decode JSON values and encode msgpack values with less work | No: 9 builds on it |
| 7 | Make the graph algorithms scale with the size of the graph | No: 17 builds on it |
| 8 | Start a goroutine for a graph vertex only when it's ready to be visited | Yes |
| 9 | Keep less, and do less, for each object of a state in memory | No: most of what follows builds on it |
| 10 | Read and write state files on all of the processors | No: 15 builds on it |
| 11 | Read the local state file once, and don't serialize it to compare it | Yes |
| 12 | Don't retrieve or serialize an unchanged remote state again | No: 13 builds on it |
| 13 | Transfer a large state to and from GCS in fewer, larger requests | Yes |
| 14 | Index the planned changes, and encode and decode them concurrently | No: 22 builds on it |
| 15 | Read and write the parts of a plan file concurrently | Yes |
| 16 | Render the changes of a plan concurrently | Yes |
| 17 | Build graphs without work that grows with the square of their size | Yes |
| 18 | Save snapshots of a large state in the background while applying | Yes |
| 19 | Evaluate a resource's for_each expression once in a graph walk | Yes |
| 20 | Share the table of functions between the scopes of a module | Yes |
| 21 | Remember the implied type of a schema block | Yes |
| 22 | Build the value of a resource once for everything that refers to it | Yes |
| 23 | Keep the value an object already has when refresh and plan don't change it | Yes |
| 24 | Give each kind of named value its own lock | Yes |
| 25 | Find the instances of a module by key, and remember the answer | Yes |
| 26 | Don't search the whole state for each resource or module of the configuration | Yes |
| 27 | Release memory between the stages of an operation on a large state | Yes |
| 28 | Allow the validation that begins an operation to be skipped | Yes |
| 29 | Fetch modules from GCS concurrently and with one storage client | Yes |
| 30 | Say in the output of "terraform version" which build of the fork this is | Yes |
| 31 | Describe the fork, and publish a container image of it | Yes |
| 32 | Add a proof, that others can repeat, that the fork does what upstream does | Yes |
| 33 | Find files in the configuration saved in a plan without searching every module | Yes |

"Can be left out on its own" means that with every other patch applied and
that one not, the fork still builds and passes `go vet`. That was tried for
each of the first 32. The 33rd changes one file of upstream's, and adds the
test of it, and nothing else uses what it adds. It's what to do when upstream changes the code under a
patch in a way that can't be reconciled with confidence: the fork without
one optimisation is still the fork, and a patch carried over wrongly is a
fault. Patch 18 is the one deliberate difference in what's on disk while an
apply runs (difference 1), and leaving it out gives upstream's behaviour
there with everything else kept. The tests and the proof weren't run for
each of those variants. They have to pass for whatever is published,
and they run for every push.

A change to the fork belongs in the patch for its area. Make it as a commit
on top that names the patch it belongs to (`git commit --fixup=<patch>`).
On a branch that hasn't been published yet, fold it in straight away
(`git rebase --autosquash`). On one that has, it stays on top, because a
published branch isn't rewritten, and is folded in when the stack next
moves, so that each new branch starts as one commit for each area.

### Moving to a newer version of upstream

Each version of upstream that the fork is carried to gets a branch of its
own, named for it, and a branch is never moved once it's published.

The repository's default branch, `fork`, has a workflow that does the move.
In the **Actions** tab, choose **Fork rebase**, press **Run workflow**, and
give it upstream's tag, such as `v1.17.0`. The `README.md` on that branch
says what it does at each step and what to do if it stops. In short:

1. A script replays the commits of the fork's newest branch onto the tag,
   folding in whatever was added on top of the stack since.
2. It builds the result and runs upstream's tests and the proof on it.
3. If a commit doesn't apply, or a check fails, Claude works out what
   upstream changed and what the fork's commit should now be. Where it
   can't be sure, it leaves the patch out and says so.
4. A second job, with no model in it, refuses the result if it differs from
   the branch it came from in anything that checks the fork: `fork-proof/`,
   the fork's two workflows, `Dockerfile.fork`, this file, or any test of
   upstream's.
5. The result is pushed as `perf/<tag>`, which starts the checks in the
   next section, and an image is published if they pass.

The same script does it by hand, in a checkout of the fork's branch, with
the `fork` branch checked out beside it:

```sh
../fork-control/rebase/rebase.sh run v1.17.0    # leaves the result on the local branch "candidate"
../fork-control/rebase/guard.sh "$(git rev-parse fork-rebase-from)" candidate v1.17.0
git push origin candidate:refs/heads/perf/v1.17.0
```

What it does in git terms, for whoever has to do it without the script:
`git rebase --interactive --autosquash --onto <new tag> <old upstream commit>`
on a copy of the branch, then the new tag's commit goes into
`fork-proof/UPSTREAM_REF`, as an amendment of the patch that adds the
proof.

Where upstream has changed code that a patch replaces, the replacement has
to be derived again from upstream's new code, not kept as it was: a patch
that still applies can be carrying what upstream used to do. The proof is
what notices that, because it compares the fork with the new upstream. The
summary of a move lists the files of the fork's that upstream changed
between the two versions, which are where to look if the proof objects.

### Fixes of upstream's that a version lacks

`fork-proof/UPSTREAM_BACKPORTS` lists commits of upstream's own, later than
the version a branch is based on, that the branch carries as they are,
before the fork's patches. The proof applies the same commits to upstream
before comparing the fork with it. The file says, for each one, why it's
there.

There is one reason to carry one: without it, upstream at that version
gives a different result from one run to the next, and then nothing can be
compared with it, the fork included. The first branch for a version needed
one, because of the way the fork remembers an answer that upstream works out
afresh each time, in code where upstream 1.17 as released depends on the
order in which Go happens to iterate over a map.

When the fork moves to a version of upstream that includes such a fix, the
fix drops out of the stack and out of the list by itself.

### What decides whether a build is published

Nothing is published from a branch unless, for that commit:

1. upstream's unit tests pass, for every Go module in the repository;
2. the race detector passes on the packages upstream runs it on and the
   ones the fork changed;
3. the proof's verdict is `identical`.

The proof also fails if the fork differs from upstream in any file that
`fork-proof/FORK_FILES` doesn't list, or changes or removes any test file
or test data of upstream's. So upstream's tests, run on the fork, are
upstream's expectations as upstream wrote them, and a test can't be edited
to make it pass without the proof saying so.

Those checks are only as good as their own definitions. `fork-proof/`,
`.github/` and this file are what a person has to approve changes to,
whoever or whatever maintains the rest
(`fork-proof/proposals/workflows/CODEOWNERS.fragment`). The workflow that
moves the fork holds itself to that: what it produces is refused if any of
them differs from the branch it started from.
