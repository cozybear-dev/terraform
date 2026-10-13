# Proposed workflows

These are proposals, kept out of `.github/workflows` so that nothing runs
them. Each says at the top what it's for and what has to be true before it's
enabled. The reasoning is in the audit report that came with this branch,
section C; the short version:

- What was proposed here for the image workflow has been adopted, in
  `.github/workflows/fork-image.yml`: it calls the proof first, publishes
  nothing unless the verdict is `identical`, and attaches the report to the
  image it publishes as a signed attestation, so that the claim and the
  image can't be separated.
- `fork-sync.yml` is the scheduled job that moves the fork to a newer
  upstream. An agent does the mechanical part. Whether the result is
  acceptable is decided by the proof, run from the protected branch, and
  never by the agent.
- `CODEOWNERS.fragment` lists the paths that decide what the proof means,
  which only a person may change.

The action versions marked "pin" are given as tags because their commits
weren't verified when this was written. Pin them, as the existing workflow
pins its actions, before enabling anything.
