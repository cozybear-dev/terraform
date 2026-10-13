#!/usr/bin/env python3
"""Prints the newest of the fork's branches named on standard input.

The fork's branches are named perf/v<version of upstream>. The newest is the
one for the highest version, where a release is higher than its own release
candidates and other pre-releases, which sorting the names as text gets wrong.
"""
import re
import sys


def key(name):
    m = re.fullmatch(r"perf/v(\d+)\.(\d+)\.(\d+)(?:-(.+))?", name)
    if not m:
        return None
    major, minor, patch, pre = m.groups()
    # A pre-release is compared piece by piece, numbers as numbers, so that
    # rc10 comes after rc2 and beta comes before rc.
    pieces = tuple(
        (0, int(piece), "") if piece.isdigit() else (1, 0, piece)
        for piece in re.findall(r"\d+|[^\d.]+", pre or "")
    )
    return (int(major), int(minor), int(patch), pre is None, pieces)


def newest(names):
    versions = [(key(name), name) for name in names]
    versions = [(k, name) for k, name in versions if k is not None]
    return max(versions)[1] if versions else None


if __name__ == "__main__":
    result = newest(line.strip() for line in sys.stdin if line.strip())
    if result is None:
        sys.exit("none of the branches is named perf/v<version>")
    print(result)
