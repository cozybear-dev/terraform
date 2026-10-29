#!/usr/bin/env python3
"""Summarizes the applies that apply.sh measured.

usage: summary.py OUT [LINES_TSV]

Reads OUT/times.tsv (one line per apply: label, plan, parallelism, run,
wall, user and system seconds, peak resident KB, peak memory and swap of its
cgroup in MB, OOM kills, exit), OUT/checks.tsv if there is one (label, plan,
check, result), and LINES_TSV (label, changed lines). Writes
OUT/summary.json and OUT/summary.md: for each build, in the order they first
appear, and each plan and parallelism, the median wall time, the most memory
(in memory and swap together) and swap, and what went wrong, with wall time
and memory also as a percentage of the build labelled "stock".
"""
import collections, json, os, statistics, sys

out = sys.argv[1]
lines = {}
if len(sys.argv) > 2:
    for line in open(sys.argv[2]):
        if line.strip():
            label, count = line.split("\t")
            lines[label] = int(count)

runs = collections.defaultdict(list)
labels, keys = [], []
for line in open(os.path.join(out, "times.tsv")):
    f = line.rstrip("\n").split("\t")
    if len(f) != 12:
        continue
    label, plan, par = f[0], f[1], f[2]
    key = "%s/%s" % (plan, par)
    if label not in labels:
        labels.append(label)
    if key not in keys:
        keys.append(key)
    num = lambda v: None if v in ("NA", "") else int(v)
    runs[(label, key)].append({"wall": float(f[4]), "mem": num(f[8]), "swap": num(f[9]),
                               "ooms": num(f[10]) or 0, "exit": f[11]})

checks = collections.defaultdict(list)
if os.path.exists(os.path.join(out, "checks.tsv")):
    for line in open(os.path.join(out, "checks.tsv")):
        f = line.rstrip("\n").split("\t")
        if len(f) == 4 and f[3] != "ok":
            checks[(f[0], f[1])].append(f[3])


def cell(label, key):
    rs = runs.get((label, key))
    if not rs:
        return None
    problems = []
    for r in rs:
        if r["exit"] == "124":
            problems.append("timeout")
        elif r["exit"] != "0":
            problems.append(r["exit"] if not r["exit"].isdigit() else "exit " + r["exit"])
        if r["ooms"]:
            problems.append("oom")
    problems += checks[(label, key.split("/")[0])]
    mems = [r["mem"] + (r["swap"] or 0) for r in rs if r["mem"] is not None]
    swaps = [r["swap"] for r in rs if r["swap"] is not None]
    return {"wall": round(statistics.median(r["wall"] for r in rs), 1),
            "mem_mb": max(mems) if mems else None,
            "swap_mb": max(swaps) if swaps else None,
            "problems": sorted(set(problems)), "runs": len(rs)}


rows = []
for label in labels:
    rows.append({"label": label, "lines": lines.get(label), "cells": {k: cell(label, k) for k in keys}})
stock = next((r for r in rows if r["label"] == "stock"), None)
for row in rows:
    for key, c in row["cells"].items():
        if c is None:
            continue
        base = stock["cells"].get(key) if stock else None
        c["wall_pct"] = round(100 * c["wall"] / base["wall"], 1) if base and base["wall"] else None
        c["mem_pct"] = round(100 * c["mem_mb"] / base["mem_mb"], 1) if base and base["mem_mb"] and c["mem_mb"] is not None else None

with open(os.path.join(out, "summary.json"), "w") as f:
    json.dump({"rows": rows, "keys": keys}, f, indent=1)


def show(c):
    if c is None:
        return "| | |"
    wall = "%.1f s" % c["wall"] + (" (%.0f%%)" % c["wall_pct"] if c.get("wall_pct") is not None else "")
    mem = "?" if c["mem_mb"] is None else "%.1f GB" % (c["mem_mb"] / 1024) + (
        " (%.0f%%)" % c["mem_pct"] if c.get("mem_pct") is not None else "")
    if c["swap_mb"]:
        mem += ", %.1f GB swapped" % (c["swap_mb"] / 1024)
    if c["problems"]:
        wall += " **" + ", ".join(c["problems"]) + "**"
    return "| %s | %s |" % (wall, mem)


md = []
for plan in dict.fromkeys(k.split("/")[0] for k in keys):
    pars = [k for k in keys if k.split("/")[0] == plan]
    md.append("#### Plan `%s`" % plan)
    md.append("")
    md.append("| build | changed lines " + "".join("| wall, -parallelism=%s | peak memory " % k.split("/")[1] for k in pars) + "|")
    md.append("|---|---" + "|---|---" * len(pars) + "|")
    for row in rows:
        md.append("| %s | %s " % (row["label"], "" if row["lines"] is None else row["lines"])
                  + "".join(show(row["cells"][k])[:-1] for k in pars) + "|")
    md.append("")
md.append("Wall time is the median of the runs; peak memory is the most that the apply held in memory and swap together, at any one moment of any run. Percentages are of `stock`.")
with open(os.path.join(out, "summary.md"), "w") as f:
    f.write("\n".join(md) + "\n")
