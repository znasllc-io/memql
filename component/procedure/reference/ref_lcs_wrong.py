#!/usr/bin/env python3
"""A DELIBERATELY WRONG reference, committed on purpose.

The parity harness's negative control runs against this one and FAILS if the
Go agrees with it. A harness that cannot detect a disagreement is a harness
whose green means nothing, and this file is the only way to know it can.

It is wrong in a specific, plausible way: it counts positional mismatches
instead of aligning on a longest common subsequence, which is the exact
mistake the Go implementation's comment warns about.
"""
import json
import sys


def wrong_distance(a, b):
    n = max(len(a), len(b))
    diff = 0
    for i in range(n):
        if i >= len(a) or i >= len(b) or a[i] != b[i]:
            diff += 1
    return diff


def main():
    doc = json.load(sys.stdin)
    json.dump({"distances": [wrong_distance(c["a"], c["b"]) for c in doc["cases"]]}, sys.stdout)


if __name__ == "__main__":
    main()
