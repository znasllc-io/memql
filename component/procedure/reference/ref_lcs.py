#!/usr/bin/env python3
"""Reference anti-unification distance over two flat sequences.

An INDEPENDENT implementation, deliberately written from the definition rather
than from the Go: the distance is the number of positions that are not part of
a longest common subsequence, counted on both sides. The Go implementation
reaches the same number by building the generalization and charging for each
hole, which is a different route to the same claim -- which is the point of a
parity harness.

Reads {"cases":[{"a":[...],"b":[...]}]} on stdin, writes
{"distances":[n, ...]} on stdout.
"""
import json
import sys


def lcs_length(a, b):
    n, m = len(a), len(b)
    table = [[0] * (m + 1) for _ in range(n + 1)]
    for i in range(n - 1, -1, -1):
        for j in range(m - 1, -1, -1):
            if a[i] == b[j]:
                table[i][j] = table[i + 1][j + 1] + 1
            else:
                table[i][j] = max(table[i + 1][j], table[i][j + 1])
    return table[0][0]


def distance(a, b):
    common = lcs_length(a, b)
    # Everything outside the common subsequence had to be generalized away.
    # Where both sides have an unmatched element at the same spot they
    # generalize together and cost one, not two.
    left, right = len(a) - common, len(b) - common
    return max(left, right)


def main():
    doc = json.load(sys.stdin)
    out = [distance(c["a"], c["b"]) for c in doc["cases"]]
    json.dump({"distances": out}, sys.stdout)


if __name__ == "__main__":
    main()
