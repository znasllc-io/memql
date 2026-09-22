#!/usr/bin/env python3
"""Reference closed frequent sub-sequence mining with a gap tolerance.

An INDEPENDENT implementation from the definition: enumerate candidate
sub-sequences, keep those occurring in at least minSupport sequences with at
most `gap` unmatched symbols between adjacent elements, drop any that a longer
candidate contains at the same support, then rank by coverage, cohesion,
length, support and finally the symbols themselves.

Reads the fixture document on stdin, writes
{"top":[["sym", ...] | null, ...]} on stdout -- the winning pattern per case,
or null where nothing clears the floor.
"""
import json
import sys
from itertools import combinations


def match_from(seq, pattern, gap):
    n, m = len(seq), len(pattern)
    for start in range(n - m + 1):
        if seq[start] != pattern[0]:
            continue
        pos = [start]
        cur = start
        ok = True
        for want in pattern[1:]:
            found = -1
            for j in range(cur + 1, min(cur + 2 + gap, n)):
                if seq[j] == want:
                    found = j
                    break
            if found < 0:
                ok = False
                break
            pos.append(found)
            cur = found
        if ok:
            return pos
    return None


def occurrences(sequences, pattern, gap):
    out = []
    for si, seq in enumerate(sequences):
        pos = match_from(seq, pattern, gap)
        if pos is not None:
            out.append(pos)
    return out


def candidates(sequences, min_support, gap, max_len=None):
    # The length cap is DERIVED from the corpus, never a constant. A fixed cap
    # silently truncates the winner on any corpus whose best pattern is longer
    # than it -- which is what the first version of this file did on the
    # contiguous-beats-scattered fixture, reporting a length-six runner-up and
    # making the harness look like it had found a Go bug.
    if max_len is None:
        max_len = max((len(s) for s in sequences), default=0)
    alphabet = sorted({s for seq in sequences for s in seq})
    found = {}
    frontier = [[s] for s in alphabet]
    while frontier:
        nxt = []
        for pat in frontier:
            occ = occurrences(sequences, pat, gap)
            if len(occ) < min_support:
                continue
            if len(pat) >= 2:
                found[tuple(pat)] = occ
            if len(pat) < max_len:
                for s in alphabet:
                    nxt.append(pat + [s])
        frontier = nxt
    return found


def contains_subsequence(hay, needle):
    i = 0
    for h in hay:
        if i < len(needle) and h == needle[i]:
            i += 1
    return i == len(needle)


def closed(found):
    out = {}
    for pat, occ in found.items():
        keep = True
        for other, oocc in found.items():
            if len(other) > len(pat) and len(oocc) == len(occ) and contains_subsequence(other, pat):
                keep = False
                break
        if keep:
            out[pat] = occ
    return out


def score(pat, occ, total):
    support = len(occ)
    coverage = support * len(pat) / total
    gaps = [p[i] - p[i - 1] - 1 for p in occ for i in range(1, len(p))]
    cohesion = 1.0 if not gaps else 1 / (1 + sum(gaps) / len(gaps))
    return coverage, cohesion


def top_pattern(sequences, min_support, gap):
    total = sum(len(s) for s in sequences)
    if total == 0:
        return None
    found = closed(candidates(sequences, min_support, gap))
    if not found:
        return None
    ranked = []
    for pat, occ in found.items():
        coverage, cohesion = score(pat, occ, total)
        ranked.append((-coverage, -cohesion, -len(pat), -len(occ), "\x00".join(pat), pat))
    ranked.sort()
    return list(ranked[0][5])


def main():
    doc = json.load(sys.stdin)
    out = [top_pattern(c["sequences"], c["minSupport"], c["gap"]) for c in doc["cases"]]
    json.dump({"top": out}, sys.stdout)


if __name__ == "__main__":
    main()
