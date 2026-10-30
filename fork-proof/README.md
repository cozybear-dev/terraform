# Integrity proof for the large-state fork

This directory is how the fork shows, to someone who has no reason to take
its word for it, that it does what upstream Terraform does.

It builds upstream at a pinned commit and the fork from source, has both do
the same things, and compares what they produce. It fails if anything
differs. It writes a report that says exactly what was compared with what.

It is evidence, not a mathematical proof. [What this establishes, and what
it doesn't](#what-this-establishes-and-what-it-doesnt) says where the line
is.

```sh
cd fork-proof
git fetch https://github.com/hashicorp/terraform "$(cat UPSTREAM_REF)"   # once
go test .          # the rules for what's disregarded
go run .           # the proof; exits non-zero unless the verdict is "identical"
```

It needs Go (the version in `../.go-version`), git, tar, and about 3 GB of
memory. It uses no network once the upstream commit and the Go modules are
there, and no cloud account. It has only been run on Linux; the part that
interrupts applies uses signals and won't work on Windows.

## Contents

- [What is compared](#what-is-compared)
- [What is disregarded](#what-is-disregarded)
- [The scenarios](#the-scenarios)
- [The component comparisons](#the-component-comparisons)
- [Interrupted applies](#interrupted-applies)
- [Coverage](#coverage)
- [The report](#the-report)
- [Options, and how long it takes](#options-and-how-long-it-takes)
- [Comparing something of your own](#comparing-something-of-your-own)
- [Testing the proof itself](#testing-the-proof-itself)
- [Making it a gate](#making-it-a-gate)
- [What this establishes, and what it doesn't](#what-this-establishes-and-what-it-doesnt)
- [Changing it](#changing-it)

## What is compared

There are three parts. Each compares the fork with upstream itself, built
from upstream's source, and not with a copy of upstream's code kept in the
fork or with a library the fork believes it matches.

Upstream is the commit in `UPSTREAM_REF`, with the commits listed in
`UPSTREAM_BACKPORTS`, if there are any, applied to it. Those are commits of
upstream's own, later than the one the fork is based on, that the fork
carries as they are. There is one case for it: a fix for something that
makes upstream give a different result from one run to the next, which
upstream has made and hasn't released in the version the fork is on.
Upstream without the fix can't be compared with anything, itself included,
so the fork takes the fix and is compared with upstream as it is with it.
The report names every commit applied, and the tree that resulted.

**Scenarios.** Each scenario in `scenarios/` is a series of steps: install a
configuration, then plan and apply, or run a command. Every scenario is run
six ways, or eight with `-planner-ref`:

| Run | Plans with | Applies with | What it's for |
|---|---|---|---|
| upstream | upstream | upstream | The reference |
| upstream-again | upstream | upstream | The control: must equal the reference, or the comparison itself is unsound |
| fork | fork | fork | The claim |
| upstream-plans-fork-applies | upstream | fork | The fork reads upstream's plan files and state |
| fork-plans-upstream-applies | fork | upstream | Upstream reads the fork's plan files and state |
| full-fork-plans-fork-applies | the full fork, at `-planner-ref` | fork | Only with `-planner-ref`, for a build that is only for applying the plans the full fork saves: it applies them as upstream applies its own |
| full-fork-plans-upstream-applies | the full fork, at `-planner-ref` | upstream | Only with `-planner-ref`: upstream applies the full fork's plans as it applies its own |
| fork-coverage | fork, instrumented | same | Records which of the fork's changes ran |

After every step, each run's artefacts are compared with the reference's:

- the exit status of each command;
- the state file and its backup, byte for byte apart from [four
  things](#what-is-disregarded), or the state held by the state server for
  the scenario that uses one, along with how many requests of each kind the
  server received;
- the plan as `terraform show` prints it, exactly;
- the plan as `terraform show -json` describes it;
- the same description of the same plan file given by the *other* build,
  which must be the reference's own;
- each member of the plan file other than the plan message itself: both
  state snapshots, the configuration snapshot and the lock file;
- what the commands printed, as a set of lines;
- every request the provider received, with a digest of the values in it;
- for the steps that declare one, the order in which changes that depend on
  one another were made;
- that the state's serial never goes backwards and increases when the state
  changes.

The provider is `_overlay/provider`. It is built in **upstream's** source
tree, so nothing of the fork is in it, and both builds talk to the same
binary. Everything it computes is a pure function of the configuration.

**Components.** `_overlay/driver` is one program, compiled twice: once in
upstream's source tree and once in the fork's. Given a seed, each of its
modes generates inputs for a component that the fork reimplements, runs
them through it, and prints a line per input. The two builds get the same
seeds and their output must be identical. See [The component
comparisons](#the-component-comparisons).

**Interrupted applies.** The fork is stopped part way through an apply, at a
different moment each time, and what's left on disk is examined. See
[Interrupted applies](#interrupted-applies).

## What is disregarded

A difference between the fork and upstream could only be hidden in what the
comparison disregards, so all of it is in one file, `canonical.go`, with
the reason for each rule beside it, and `canonical_test.go` pins down what
each rule must *not* disregard. This table is the whole list.

| # | In | Disregarded | Why |
|---|---|---|---|
| 1 | State | The value of the top-level `lineage` | A random UUID generated when a state is first created |
| 2 | State | The value of the top-level `serial` | The fork documents writing fewer intermediate snapshots, and the serial counts them (difference 2 in `README-FORK.md`). In its place: each run's serial must never go backwards, and must increase when the state changes |
| 3 | State | The order of `check_results`, and of the objects in each | Upstream writes them in map order, which differs from one run of upstream to the next |
| 4 | State, plan, output | The eight hex digits that identify a deposed object | Chosen at random by upstream |
| 5 | Plan JSON | The value of `timestamp` | Wall-clock time |
| 6 | Plan JSON | The order of `relevant_attributes` | Upstream emits them in map order |
| 7 | Plan JSON | The order of `child_modules` at each level | Upstream emits them in map order |
| 8 | Command output | Elapsed times, the "Still creating..." lines, and the two lines that say a lock on the state is taking a while | Timing |
| 9 | Command output | The order of the lines | Resources are worked on concurrently, by both builds. The plan itself is compared in order, from `terraform show` |
| 10 | Provider requests | Their order and sequence numbers | The same. The order that matters is checked against what each scenario declares |

Sorting is used rather than removal wherever possible, so that the same
things must still be present the same number of times.

Three more things are not rules of the canonicaliser but belong in this
list, because they too are places a difference isn't looked for:

- **The bytes of the plan message inside a plan file.** The fork documents
  writing its fields in a different order (difference 4). Its meaning is
  compared instead: by each build's description of it, and by each build's
  description of the *other's* plan file.
- **The wording of the error for an unreadable state file**, in the
  component comparison only. The fork documents one such difference
  (difference 8). Whether a file is rejected is compared; the message isn't.
- **Requests to validate configuration, in the one scenario that sets
  `TF_SKIP_VALIDATE`**, since making fewer of those is what the setting does.

Rules 3, 4, 6 and 7 exist because upstream disagrees *with itself*. They
weren't assumed: the control run found each of them. A rule that is missing
shows up as a `control` result, which is reported as a fault in the proof
rather than in the fork.

## The scenarios

Twenty-six scenarios, about a hundred steps. `COVERAGE.md` maps each change
in the fork to the scenarios and component modes that exercise it.

| Scenario | What it does |
|---|---|
| 01-create-update-replace-delete | The four basic actions, on resources with attributes of every kind |
| 02-count-and-for-each | `count`, `for_each` over maps and sets, instances referring to each other, then fewer and different instances |
| 03-nested-modules | Modules two deep with `for_each` and `count`, objects passed down and outputs passed up |
| 04-module-instances-removed | Module instances that stop existing while their objects are in the state, then whole module calls removed |
| 05-moved-blocks | Renames, chained moves, implied moves, a moved instance key, a renamed module, an instance moved between module calls |
| 06-import | Import blocks: as is, with a change in the same plan, and with `for_each` |
| 07-data-sources-and-depends-on | Data sources read during planning, deferred to apply, and ordered only by `depends_on` |
| 08-create-before-destroy | Replacement in both orders, with deposed objects and declared orderings |
| 09-deposed-and-tainted | A failed destroy leaving a deposed object, a failed create leaving a tainted one, `taint` and `untaint` |
| 10-sensitive-and-unknown | Sensitive values from every source, single sensitive elements in collections, values unknown until apply |
| 11-outputs-and-checks | Many outputs, preconditions and postconditions, variable validation, check blocks that fail and then pass |
| 12-refresh-and-drift | An object changed outside Terraform: `-refresh=false`, `-refresh-only`, and a normal plan |
| 13-destroy | Destroy by saved plan and without one, in reverse dependency order |
| 14-targeted-and-replace | `-target` on a resource, a module instance and one instance; `-replace` |
| 15-failures-during-apply | A resource that fails when applied: what's saved, what's skipped; then one that fails when planned |
| 16-large-and-unusual-values | A 500 kB string, awkward strings, numbers at every limit, dynamic values of every shape, 1,500-element collections |
| 17-many-instances | 2,250 instances: past every threshold at which the fork does something differently; then a failing apply at that size |
| 18-chained-modules | Fourteen module calls chained through their outputs |
| 19-state-commands | `state list`, `show`, `mv`, `rm`, then a plan that must find nothing to do |
| 20-state-from-elsewhere | A hand-written state file in every form the fork's own decoder declines |
| 21-validation-skipped | Scenario 03 with `TF_SKIP_VALIDATE=1` |
| 22-ordering | A chain of twenty, a diamond, and `depends_on` across modules, with declared orderings |
| 23-provisioners | Provisioners that succeed, fail, and run at destroy |
| 24-equal-values-written-differently | Outputs that are equal as numbers each time but that upstream doesn't write the same way each time |
| 25-json-syntax-aliases-and-functions | JSON-syntax configuration, two configurations of one provider, file and template functions |
| 26-state-in-a-remote-backend | Scenario 02 with its state in an HTTP server that the proof runs, then a failing apply and a destroy |

Each scenario's `scenario.json` says what upstream is expected to do in each
step (exit statuses, and text it must print). If upstream doesn't, the
scenario is reported as `invalid` and the verdict is `incomplete`. That is
what stops a scenario from passing because both builds failed in the same
way before reaching what it was written to exercise.

## The component comparisons

| Mode | What goes in | What's compared |
|---|---|---|
| `values` | Values of generated types: numbers held with every precision, awkward strings, nested collections, dynamic types, sensitive marks | The bytes written to the state, the bytes written to a plan, the value read back, the bytes written again, and the same after a trip through a state file |
| `dyntypes` | Dynamically-typed values whose types are equal but serialize differently | That each is written as upstream writes it, whatever was written before it |
| `decode` | JSON for a generated type, formatted every way JSON allows, with duplicate and missing properties, wrong types, and syntax errors | Whether it's accepted, the error if not, the value, and how it's written again |
| `statefile` | Whole state files in four layouts, a third of them damaged at random, some large enough for the fork's concurrent paths | Whether the file is accepted, the state read from it, the bytes written, and the same again from those bytes |
| `dag` | Graphs of several shapes, some with cycles | The transitive reduction, the cycles found, and a walk with some vertices failing: which are visited, which are skipped, and that none starts before what it depends on |
| `changes` | Sequences of planned changes being recorded, looked up and removed | Every result, in the order returned |
| `expander` | Trees of module calls with every kind of expansion, registered and queried as the graph walk does | Every result |

Every mode is also run a second time with upstream. An input on which
upstream disagrees with itself is excluded and counted in the report. Two
runs only notice that when they happen to disagree, so before a difference
is reported, upstream is given that one input sixty more times, and if it
ever gives the fork's result, the input is counted as one of those instead.
That can't hide a difference, since it only accepts what upstream itself
does with the same input.

Two kinds of input were found on which upstream's result varies from run
to run, and both are kept out of what's generated, with the reason, rather
than left to that check: a tuple type with no elements, which upstream
writes as `[]` or `null` depending on how the type was built, and a map
with two keys that are different bytes but the same string once
normalized, of which upstream keeps one value or the other depending on
the order it happens to traverse a Go map in.

`dyntypes` exists because of the first of those. The case was taken out of
`values`, where it made upstream's output vary, and given a mode of its own
in which it's deterministic, since it is also something the fork got wrong.

## Interrupted applies

The fork's one deliberate change to what's on disk while an apply runs is
that, for a large state, it saves snapshots in the background rather than
after every resource (difference 1). This part checks what that leaves
behind when an apply stops unexpectedly.

An apply that creates 56 objects, each taking a fifth of a second, eight of
them in a chain, is run on top of a state that already has 30 objects
(small enough that the fork saves after each one, like upstream) or 2,200
(large enough that it doesn't). It is stopped at a random moment, by
`SIGKILL` to Terraform alone or by one `SIGINT`. Afterwards:

- The state file must be a complete snapshot or recognizably not one. If it
  isn't valid, Terraform must refuse it. An empty file is counted, not
  failed: Terraform reads that as "no state", and upstream leaves one too.
- A complete snapshot must have every object that was there before, exactly
  as it was, and no object the provider didn't finish creating, each with
  the values the provider returned.
- An object in the chain may only be there if the one before it is.
- The backup must be the state from before the apply. If the kill arrived
  while the backup was being written, which both builds do before touching
  the state file and neither does atomically, it can be the beginning of
  that state instead, and then the state file must be exactly as it was.
- Planning again must propose to create exactly what's missing.
- After `SIGINT`, nothing the provider finished creating may be missing.

After a kill, objects that were created may be missing from the state. For
the variant with 2,200 objects that is the documented difference, so it's
reported, for every attempt, and isn't a failure. Read the numbers.

For the variant with 30 objects it is a failure if more are missing than
Terraform works on at once, which is ten. The fork saves a state of fewer
than 2,000 instances after every object, before the graph node that created
it does anything else, so each of the ten is either still being created or
waiting for its turn to be saved, and upstream can leave out as many. (The
fork used to decide that a state was large from how long a snapshot took to
write as well, which on a disk that takes 5 ms to sync a file made the
30-object variant behave like the other: a kill left up to 20 of 40 created
objects out of the state on the machine this was written on, and 48 of 56
with the disk busy. That is what this bound is there to catch.)

## Coverage

The scenarios are also run with a build of the fork that's instrumented for
coverage, and the component driver likewise. The report says how many of
the statements *that the fork added or changed* (from `git diff` against
the pinned upstream commit, tests excluded) were executed, per file, and
`.work/coverage/uncovered.txt` lists every changed block that wasn't.

A changed statement that never ran is one about which this proof says
nothing. The number is there so that this is visible, and so that it can be
driven up by adding scenarios. It isn't a gate, because some of what's
uncovered can't be reached without a cloud service (the GCS backend and
the GCS module installer, which appear in the figure as wholly uncovered),
and some is error handling for things that shouldn't happen.

## The report

`report.json` (format `forkproof/1`) records:

- the upstream commit and the fork commit, and whether the fork was built
  from a tree with uncommitted changes;
- the SHA-256 of each `terraform` binary that was compared;
- the Go toolchain and the platform;
- a digest of the corpus (scenarios, provider, driver, and the source of
  the comparison itself), and separately of the canonicaliser, so that two
  reports can be seen to have made the same comparisons;
- for each scenario: its result, how many comparisons were made, a digest
  of everything upstream produced, and each difference found;
- for each component mode: the seed, the number of inputs, how many
  differed, how many were excluded, and examples;
- for each interruption variant: what was found after each stop;
- coverage of the changed statements, per file.

A scenario's result is `identical`, `different` (the fork differs from
upstream), `control` (upstream differs from itself, so the comparison can't
be trusted until that's understood) or `invalid` (the scenario didn't do
what it declares). The verdict is `identical` only if every result is;
`different` if anything is; otherwise `incomplete`.

The files that differ are kept in `.work/differences/<scenario>/<step>/`,
one for upstream and one for each run that disagreed, so they can be
compared with `diff`.

## Options, and how long it takes

```
-tier ci|nightly      How many generated inputs and interruptions (default ci)
-fork-ref REF         Commit of the fork to prove (default HEAD); WORKTREE for the tracked files as they are
-upstream-ref REF     Upstream commit (default: the content of UPSTREAM_REF)
-upstream-backports F File of upstream commits to apply to it (default UPSTREAM_BACKPORTS; empty for none)
-fork-bin PATH        Compare this binary instead of building one, e.g. one copied out of a published image
-only LIST            scenarios, components, crash
-scenarios REGEXP     Only the scenarios whose names match
-jobs N               How many terraform commands to run at once
-seed N               Seed for the generated inputs and for when applies are interrupted
-coverage=false       Don't build the instrumented binaries
-skip-build           Use what the last run built
-keep                 Keep the working directories of scenarios that pass
-report PATH          Where to write the report
```

On a 12-core arm64 machine the `ci` tier takes about seven minutes when
nothing else is running, and two more the first time, for the builds:
about four for the scenarios (nearly all of it upstream applying scenario
17, which rewrites and syncs the whole state after each of 2,250
instances, the cost the fork exists to remove), one for the components and
one for the interruptions. It took fifteen with the disk busy. Budget
twenty minutes on a four-core hosted runner. The `nightly` tier runs twenty
times the generated inputs and ten times the interruptions; it hasn't been
timed, and should take under an hour.

To check a published image rather than a build from source:

```sh
id=$(docker create ghcr.io/cozybear-dev/terraform:<branch>-<commit>)
docker cp "$id":/bin/terraform ./terraform-from-image && docker rm "$id"
go run . -fork-ref <commit> -fork-bin ./terraform-from-image
```

## Comparing something of your own

The corpus can't contain your configuration, your state or your providers.
The rules it compares with can be applied to anything, though:

```sh
go run . compare state      upstream.tfstate fork.tfstate
go run . compare plan-json  upstream.json    fork.json     # terraform show -json
go run . compare plan-text  upstream.txt     fork.txt      # terraform show -no-color
go run . compare plan-file  upstream.tfplan  fork.tfplan
go run . compare output     upstream.txt     fork.txt      # what plan or apply printed
```

The most useful single use of it is to take a copy of a real state, plan
against it with stock Terraform and with the fork (`-refresh=false`, so
that nothing is asked of a provider but its schema), and compare the two
plan files and the two descriptions of them. That is what was done with a
90 MB generated state when this was written: the plans, their 260 MB JSON
descriptions, both snapshots in each plan file, and each build's reading of
the other's plan file were all identical.

## Testing the proof itself

`canaries/` holds patches that each put a known fault into the fork. With
any of them applied the verdict must be `different`. That is how to tell
that the proof hasn't been weakened, which its passing can't. See
`canaries/README.md`.

`FORK_FILES` lists every file in which the fork may differ from upstream,
and the proof reports any other. Separately, and whatever that list says,
it reports any test file or test data of upstream's that the fork changes
or removes. The fork's own tests are in files that it adds, so upstream's
suite, run on the fork's tree, is upstream's expectations exactly as
upstream wrote them, and a test edited to make it pass can't go unnoticed.

## Making it a gate

`.github/workflows/fork-proof.yml` runs the proof and fails unless the
verdict is `identical`. It runs when started by hand or called by another
workflow, and the image workflow (`.github/workflows/fork-image.yml`) calls
it: no image is published without a passing proof, and the report is
attached to the image as a signed attestation.

That gates what is published. To gate what is merged as well:

1. Protect the branch, and require the check "Compare with upstream".
2. Add a `pull_request` trigger to the workflow.
3. Require review from a code owner for `fork-proof/`, `.github/` and
   `README-FORK.md` (`proposals/workflows/CODEOWNERS.fragment`).

The third step matters as much as the first. The proof is only as good as
its corpus and its rules, and both live in this directory. Whoever can
change them can make the proof pass.

## What this establishes, and what it doesn't

**It establishes** that, for the upstream commit, the fork commit, the
toolchain and the platform named in the report:

- For every step of every scenario, the fork left the same state, wrote the
  same backup, made the same plan, described it the same way, sent the
  provider the same requests with the same values, made dependent changes
  in an order that respects the dependencies, and exited with the same
  status, as upstream did, apart from what the table above lists.
- Each build read the other's state and plan files and acted on them
  identically.
- For every generated input, each reimplemented component gave the result
  that upstream's implementation gave.
- After each interruption, what was on disk satisfied the properties listed
  under [Interrupted applies](#interrupted-applies).
- Upstream, run twice, agreed with itself about everything compared.

**It does not establish:**

- *That the fork is equivalent to upstream.* It establishes equivalence on
  a finite corpus. A configuration, a state, a provider behaviour or a
  timing that the corpus doesn't contain could still behave differently.
  The coverage figure says how much of the changed code the corpus reaches;
  it can't say whether a reached statement was reached with the input that
  would have exposed a fault.
- *Anything about concurrency that didn't happen to occur.* A data race or
  an ordering fault that needs a particular interleaving won't show up in a
  handful of runs. The race detector and repeated runs reduce this; they
  don't remove it.
- *Anything about real providers.* The provider here is a few hundred lines
  with no remote API. It never returns a value that's unknown after apply,
  never changes its schema, never uses the legacy SDK's looser type system,
  and never takes long enough for timing to matter outside the
  interruption test.
- *Anything about the GCS backend or the GCS module installer*, which need
  the service or an emulator. They have unit tests against in-process
  doubles and nothing here. Scenario 26 takes the state through the code
  that every remote backend shares, with a server that does nothing a
  bucket doesn't, but none of the fork's changes to the GCS client itself
  (one-request uploads, ranged downloads, compression, skipping a download
  when the generation hasn't changed) are reached.
- *Anything about other backends*, HCP Terraform, workspaces, contention
  for the state lock, `terraform test`, Stacks, or other platforms. Only
  Linux has been run; the fork is only cross-compiled for the others.
- *Anything about a hard stop of the machine.* The interruption test kills
  the process. It doesn't cut power, so nothing here bears on what the
  filesystem holds after a crash of the host.
- *That states at the scale the fork is for behave the same.* Scenario 17
  has 2,250 instances and a megabyte of state, enough to cross every
  threshold in the code, and nothing like 400 MB.
- *That the proof is itself correct.* The control run, the tests of the
  canonicaliser and the `invalid` result guard against the proof passing
  for the wrong reason. They are not a proof of the proof.

**How to reduce what remains**, in rough order of value:

1. Run the same comparison once against the real state and configuration
   the fork is for, with real providers, in plan-only mode: both builds
   plan with `-refresh=false` against a copy of the state, and the plans
   and plan files are compared with this canonicaliser. That covers the
   scale and the provider behaviour that the corpus can't.
2. Add a scenario for every bug found, before fixing it. The three
   differences this proof found on its first run are now in the corpus.
3. Run the nightly tier on a schedule, with a different seed each night.
4. Run the scenarios with a race-detector build of the fork
   (`go build -race`, then `-fork-bin` and `-coverage=false`). The first
   time this was done it found a data race that no unit test had.
5. Put the GCS backend under the same treatment with an emulator.
6. Raise the coverage of changed statements, starting from
   `uncovered.txt`.

## Changing it

- **A new scenario** is a directory with a `scenario.json` and one
  directory per configuration. Say what it covers, using the names in
  `COVERAGE.md`, and what upstream is expected to do in each step.
- **A new rule in `canonical.go`** needs a reason that is about upstream,
  not about the fork (or a documented difference in `README-FORK.md`), a
  test of what it must not disregard, and a row in the table above.
- **A line in `UPSTREAM_BACKPORTS`** changes what the fork is being compared
  with, like moving `UPSTREAM_REF`. It has to be a commit of upstream's,
  taken whole and unaltered, and the reason has to be that upstream at
  `UPSTREAM_REF` doesn't agree with itself: the control run, or the count
  of inputs "unstable in upstream" for a component, shows that. A commit
  that only makes the fork's result the same as upstream's by changing what
  upstream does is not a reason. It comes out again when `UPSTREAM_REF`
  moves to a version that has it.
- **Moving `UPSTREAM_REF`** changes what the fork is being compared with.
  If the proof then fails, either the fork has a fault against the new
  upstream, or upstream changed behaviour that the fork's replacement code
  still has the old version of. Both are faults in the fork. Neither is a
  reason to change a rule.
