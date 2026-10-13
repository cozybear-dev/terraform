# Proposals

What was proposed here for running the fork with less of a person's time has
mostly been adopted, and what is left is the one thing that only the
repository's owner can do:

- What was proposed here for the image workflow has been adopted, in
  `.github/workflows/fork-image.yml`: it calls the proof first, publishes
  nothing unless the verdict is `identical`, and attaches the report to the
  image it publishes as a signed attestation, so that the claim and the
  image can't be separated.
- What was proposed for moving the fork to a newer upstream has been
  adopted too, as the "Fork rebase" workflow on the repository's default
  branch, `fork`. Claude does what needs judgement, in a job that can't
  push, and a script with no model in it refuses the result if anything
  that checks the fork was changed.
- `CODEOWNERS.fragment` lists the paths that decide what the proof means,
  which only a person may change. It takes effect once it's in a
  `CODEOWNERS` file and the fork's branches are protected with review by a
  code owner required, which is a setting of the repository.
