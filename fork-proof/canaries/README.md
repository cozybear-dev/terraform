# Canaries

Each patch here puts a fault into the fork: a way of differing from
upstream that is known to matter. Three are faults the fork really had,
which the proof found on its first run; one is made up.

They exist to test the proof, not the fork. A proof that has been weakened,
by a rule added to `canonical.go`, a scenario removed, or a comparison
skipped, still says `identical` about a correct fork, so its saying so
isn't evidence that it would notice an incorrect one. Applying each of
these in turn and requiring the verdict to be `different` is.

```sh
for patch in fork-proof/canaries/*.patch; do
  git apply "$patch"
  (cd fork-proof && go run . -fork-ref WORKTREE -coverage=false -only scenarios,components) && echo "NOT CAUGHT: $patch"
  git apply -R "$patch"
done
```

| Patch | The fault | What catches it |
|---|---|---|
| 01 | A whole number held as a float is written digit for digit, where upstream writes the shortest form that identifies it | Component mode `values` |
| 02 | A type is given the serialized form of an equal type that was seen earlier | Component modes `dyntypes` and `values` |
| 03 | The two state snapshots in a plan file are treated as the same when their outputs are equal but not written alike | Scenario 24 |
| 04 | The walker runs a vertex whose dependency failed | Component mode `dag` (4,074 of 6,000 graphs); scenarios 15, 17 and 23 |

A canary that the proof doesn't catch is a gap in the corpus, and is worth
more than one it does: it says exactly what to add.

If the fork's code changes so that a patch no longer applies, write it
again against the new code; don't delete it.

## One that only a unit test can catch

`unit/05-module-index-never-rebuilt.patch` stops the index of the modules
in a state from ever being rebuilt after a module is added or removed. It
was written to find out whether the scenarios exercise that, and they
don't: with it applied, scenarios 04, 05, 14, 19 and 20 are all still
identical to upstream.

That turns out not to be a gap that a scenario can fill. The index is only
used to look, in the state from the previous run, for objects whose module
instance is no longer declared. While a plan is in progress, nothing adds a
module to that state, and a module is only removed from it once it's empty,
after which finding it again finds nothing in it. So an index that is out
of date gives the right answer every time, here. The invalidation matters
for the next thing that uses the index, not for this one.

It is caught by the fork's own test of the index against a search of every
module, `TestSyncStateResourcesWhileRLocked` in `internal/states`:

```sh
git apply fork-proof/canaries/unit/05-module-index-never-rebuilt.patch
go test ./internal/states/ && echo "NOT CAUGHT"
git apply -R fork-proof/canaries/unit/05-module-index-never-rebuilt.patch
```

It's kept as a reminder of the difference: the proof compares what
Terraform does, and a fault that can't yet change what Terraform does is
invisible to it, however real it is in the code.
