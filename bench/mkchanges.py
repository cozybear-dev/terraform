#!/usr/bin/env python3
"""Edit a configuration from gen4.py (made with COPIES=1) so that planning it
changes about TARGET resources, and print how many of each kind to expect.

usage: mkchanges.py DIR TARGET MIX [SEED]

MIX is "mixed" (70% updates, 10% each of creates, deletes and replacements)
or "create" (creates only). It prints JSON:
{"update": U, "create": C, "delete": D, "replace": R}.

- An update changes the label "tier" of a module's Pub/Sub topics: one
  update for each topic of the module.
- A create raises a module's n, which adds a topic, a subscription, a Vault
  policy and a Vault secret for each new key, and a Secret Manager secret for
  every second one.
- A delete lowers n, removing the same things.
- A replacement renames a module's Secret Manager secrets, whose names can't
  be changed in place.

Modules are taken in an order fixed by SEED, and no module gets two kinds of
edit, so the counts add up.
"""
import json, os, random, re, sys


def creates(n, k):
    # Four resources for each key, and a secret for every second one.
    return 4 * k + (n + k) // 2 - n // 2


def deletes(n, k):
    return 4 * k + n // 2 - (n - k) // 2


def main():
    root, target, mix = sys.argv[1], int(sys.argv[2]), sys.argv[3]
    seed = int(sys.argv[4]) if len(sys.argv) > 4 else 1
    if mix == "mixed":
        want = {"create": round(target * 0.1), "delete": round(target * 0.1), "replace": round(target * 0.1)}
        want["update"] = target - sum(want.values())
    elif mix == "create":
        want = {"update": 0, "create": target, "delete": 0, "replace": 0}
    else:
        sys.exit("MIX must be mixed or create")

    main_tf = os.path.join(root, "main.tf")
    with open(main_tf) as f:
        config = f.read()
    # The n of each module call, which gen4.py writes as `  n      = N`.
    calls = {int(m.group(1)): int(m.group(2)) for m in re.finditer(
        r'module "app_(\d+)" \{\n  source = "\./modules/app_\d+"\n  name   = "[^"]*"\n  n      = (\d+)\n', config)}
    if not calls:
        sys.exit("%s isn't a configuration from gen4.py with COPIES=1" % root)
    order = sorted(calls)
    random.Random(seed).shuffle(order)

    def module_file(i):
        return os.path.join(root, "modules", "app_%03d" % i, "main.tf")

    def edit_module(i, old, new):
        path = module_file(i)
        with open(path) as f:
            text = f.read()
        if old not in text:
            sys.exit("%s has no %r" % (path, old))
        with open(path, "w") as f:
            f.write(text.replace(old, new))

    def set_n(i, n):
        nonlocal config
        config, count = re.subn(
            r'(module "app_%03d" \{\n  source = "[^"]*"\n  name   = "[^"]*"\n  n      = )\d+\n' % i,
            r"\g<1>%d\n" % n, config)
        assert count == 1, i

    got = {"update": 0, "create": 0, "delete": 0, "replace": 0}
    modules = iter(order)

    def take():
        try:
            return next(modules)
        except StopIteration:
            sys.exit("there aren't enough modules to make %d changes" % target)

    # Whole modules for the kinds whose size is fixed by n, while they get
    # closer to what's wanted; then creates and deletes, which can be any size.
    for kind, per_module, old, new in (
            ("replace", lambda n: n // 2, '-secret-', '-secret2-'),
            ("update", lambda n: n, 'tier   = "bench"', 'tier   = "bench2"')):
        while want[kind] - got[kind] > 0:
            i = take()
            size = per_module(calls[i])
            # Every kind that's wanted gets at least one module, so that each
            # kind of change is in the plan however small it is.
            if size == 0 or (got[kind] > 0 and abs(want[kind] - got[kind] - size) >= want[kind] - got[kind]):
                break
            edit_module(i, old, new)
            got[kind] += size
    while want["create"] - got["create"] > 0:
        i = take()
        n, left = calls[i], want["create"] - got["create"]
        # Up to doubling the module, the smallest k that reaches what's left.
        k = next((k for k in range(1, n + 1) if creates(n, k) >= left), n)
        if k > 1 and creates(n, k) - left > left - creates(n, k - 1):
            k -= 1
        if abs(left - creates(n, k)) >= left:
            break
        set_n(i, n + k)
        got["create"] += creates(n, k)
    while want["delete"] - got["delete"] > 0:
        i = take()
        n, left = calls[i], want["delete"] - got["delete"]
        # The first key stays: the module's output refers to it.
        k = next((k for k in range(1, n) if deletes(n, k) >= left), n - 1)
        if k > 1 and deletes(n, k) - left > left - deletes(n, k - 1):
            k -= 1
        if k < 1:
            continue
        if abs(left - deletes(n, k)) >= left:
            break
        set_n(i, n - k)
        got["delete"] += deletes(n, k)

    with open(main_tf, "w") as f:
        f.write(config)
    print(json.dumps(got, sort_keys=True))


if __name__ == "__main__":
    main()
