#!/usr/bin/env python3
"""
SI -> AI rename tool (epic memql#1889, issue 1.1).

Curated, identifier-boundary, allowlist/denylist renamer. Dry-run by default;
emits a unified diff for review. Re-runnable and idempotent (an applied token
no longer matches its rule, and no replacement string contains "SI").

The allowlist + denylist live in scripts/rename/rules.json. This engine never
invents renames -- it only applies the explicit rules. That is what keeps
SIGTERM / SESSION / SIMLI / POSIX / SID and friends untouched: they are never
equal to an allowlisted token, and the denylist scan asserts it.

Usage:
  # Dry-run a group over a repo (prints diff + summary):
  si_to_ai.py --root ../memql --group go-internal

  # Dry-run everything applicable to a repo:
  si_to_ai.py --root ../copresent

  # Apply (writes files):
  si_to_ai.py --root ../memql --group proto --apply

  # G1 verification scan -- residual SI-as-AI identifiers outside the denylist:
  si_to_ai.py --root ../memql --scan
"""
import argparse
import json
import os
import re
import sys
from difflib import unified_diff

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_RULES = os.path.join(HERE, "rules.json")

# Directories never walked (worktrees, sibling checkouts, vendored/generated).
EXCLUDED_DIR_NAMES = {
    ".git", "node_modules", "vendor", "dist", "build", ".next",
    "coverage", ".turbo", "__pycache__",
}
EXCLUDED_DIR_SUFFIXES = ("-worktrees",)
EXCLUDED_DIR_PREFIXES = ("memql-rel",)  # sibling release checkouts e.g. memql-rel087

ALL_EXTS = {".go", ".memql", ".proto", ".ts", ".tsx", ".md"}
LEFT = r"(?<![A-Za-z0-9_])"
RIGHT = r"(?![A-Za-z0-9_])"


def load_rules(path):
    with open(path) as f:
        data = json.load(f)
    return data


def compiled_rules(data, groups, exts_filter):
    """Return list of (group, old, new, compiled_regex, applicable_exts)."""
    out = []
    for gname, rules in data["groups"].items():
        if groups and gname not in groups:
            continue
        # Longest 'old' first so a short token never pre-empts a longer one.
        for r in sorted(rules, key=lambda r: len(r["old"]), reverse=True):
            exts = set(r.get("exts", list(ALL_EXTS)))
            if exts_filter:
                exts = exts & set(exts_filter)
            if not exts:
                continue
            mode = r.get("mode", "token")
            if r.get("regex") or mode == "regex":
                pat = re.compile(LEFT + r["old"])
            elif mode == "prefix":
                # token STARTS with old (left identifier boundary, any suffix).
                pat = re.compile(LEFT + re.escape(r["old"]))
            elif mode == "infix":
                # old appears anywhere inside an identifier. Use only for long,
                # unambiguous AI stems (SIProvider, SIChat, ...) -- the denylist
                # overlap check refuses any match that touches a protected span.
                pat = re.compile(re.escape(r["old"]))
            else:  # token: full identifier boundary both sides
                pat = re.compile(LEFT + re.escape(r["old"]) + RIGHT)
            out.append((gname, r["old"], r["new"], pat, exts))
    return out


def compiled_denylist(data):
    return [re.compile(LEFT + p + RIGHT) for p in data.get("denylist", [])]


def iter_files(root, exts):
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [
            d for d in dirnames
            if d not in EXCLUDED_DIR_NAMES
            and not d.endswith(EXCLUDED_DIR_SUFFIXES)
            and not d.startswith(EXCLUDED_DIR_PREFIXES)
        ]
        for fn in filenames:
            ext = os.path.splitext(fn)[1]
            if ext in exts:
                yield os.path.join(dirpath, fn)


def denylist_spans(text, denylist):
    spans = []
    for pat in denylist:
        for m in pat.finditer(text):
            spans.append((m.start(), m.end()))
    return spans


def apply_rules_to_text(text, rules, ext, denylist):
    """Return (new_text, per_rule_counts, denylist_violations)."""
    counts = {}
    violations = []
    deny = denylist_spans(text, denylist)

    def overlaps_deny(start, end):
        for ds, de in deny:
            if start < de and ds < end:
                return (ds, de)
        return None

    for gname, old, new, pat, exts in rules:
        if ext not in exts:
            continue
        def repl(m):
            ov = overlaps_deny(m.start(), m.end())
            if ov:
                violations.append((old, text[ov[0]:ov[1]]))
                return m.group(0)  # refuse to touch a denylisted span
            counts[old] = counts.get(old, 0) + 1
            return new
        text = pat.sub(repl, text)
    return text, counts, violations


def cmd_rename(args, data):
    rules = compiled_rules(data, args.group, args.ext)
    denylist = compiled_denylist(data)
    exts = set(e for _, _, _, _, es in rules for e in es)
    total_counts = {}
    changed_files = 0
    all_violations = []
    for path in iter_files(args.root, exts):
        ext = os.path.splitext(path)[1]
        try:
            with open(path, encoding="utf-8") as f:
                text = f.read()
        except (UnicodeDecodeError, OSError):
            continue
        new_text, counts, violations = apply_rules_to_text(text, rules, ext, denylist)
        if violations:
            all_violations.extend((path, *v) for v in violations)
        if new_text != text:
            changed_files += 1
            for k, v in counts.items():
                total_counts[k] = total_counts.get(k, 0) + v
            rel = os.path.relpath(path, args.root)
            if args.apply:
                with open(path, "w", encoding="utf-8") as f:
                    f.write(new_text)
            else:
                diff = unified_diff(
                    text.splitlines(keepends=True),
                    new_text.splitlines(keepends=True),
                    fromfile="a/" + rel, tofile="b/" + rel,
                )
                sys.stdout.writelines(diff)

    print("\n" + "=" * 70, file=sys.stderr)
    print(f"{'APPLIED' if args.apply else 'DRY-RUN'}  root={args.root}  "
          f"groups={args.group or 'ALL'}", file=sys.stderr)
    print(f"files changed: {changed_files}", file=sys.stderr)
    for k in sorted(total_counts, key=lambda k: -total_counts[k]):
        print(f"  {total_counts[k]:5d}  {k} -> {rule_target(data, k)}", file=sys.stderr)
    if all_violations:
        print("\nDENYLIST VIOLATIONS (refused, NOT changed):", file=sys.stderr)
        for path, old, hit in all_violations:
            print(f"  {path}: rule {old} overlapped denylisted {hit}", file=sys.stderr)
        return 2
    return 0


def rule_target(data, old):
    for rules in data["groups"].values():
        for r in rules:
            if r["old"] == old:
                return r["new"]
    return "?"


# --- G1 verification scan -------------------------------------------------

# Compound identifier carrying an uppercase 'SI' acronym (camel/Pascal/snake/const).
SCAN_COMPOUND = re.compile(LEFT + r"[A-Za-z_]*SI[A-Za-z0-9_]*" + RIGHT)
SCAN_CAMEL = re.compile(LEFT + r"si[A-Z][A-Za-z0-9_]*" + RIGHT)  # camelCase siFoo
SCAN_KEYWORD = re.compile(LEFT + r"si\(")            # .memql DSL keyword
SCAN_PROTO_SNAKE = re.compile(LEFT + r"si_[a-z0-9_]*" + RIGHT)  # .proto field


def cmd_scan(args, data):
    """Report residual SI-as-AI identifiers outside the denylist (G1 gate)."""
    denylist = compiled_denylist(data)
    substr_deny = data.get("denylist_substrings", [])
    exts = set(args.ext) if args.ext else ALL_EXTS
    actionable = 0
    bare_si = 0
    for path in iter_files(args.root, exts):
        ext = os.path.splitext(path)[1]
        try:
            with open(path, encoding="utf-8") as f:
                lines = f.readlines()
        except (UnicodeDecodeError, OSError):
            continue
        deny_pat = denylist
        for ln, line in enumerate(lines, 1):
            hits = set()
            for m in SCAN_COMPOUND.finditer(line):
                tok = m.group(0)
                if tok == "SI":
                    continue  # bare acronym -> prose bucket below
                if any(p.fullmatch(tok) for p in deny_pat):
                    continue
                if any(s in tok for s in substr_deny):
                    continue  # contains an uppercase English root (VERSIONING, etc.)
                hits.add(tok)
            if ext in (".go", ".memql", ".ts", ".tsx"):
                for m in SCAN_CAMEL.finditer(line):
                    tok = m.group(0)
                    if any(p.fullmatch(tok) for p in deny_pat):
                        continue  # deferred lowercase keys (siParticipantId, ...)
                    hits.add(tok)
            if ext == ".memql":
                if SCAN_KEYWORD.search(line):
                    hits.add("si(")
            if ext == ".proto":
                for m in SCAN_PROTO_SNAKE.finditer(line):
                    tok = m.group(0)
                    if any(p.fullmatch(tok) for p in deny_pat):
                        continue  # deferred config fields (si_openai_api_key, ...)
                    hits.add(tok)
            if hits:
                actionable += len(hits)
                rel = os.path.relpath(path, args.root)
                print(f"{rel}:{ln}: {', '.join(sorted(hits))}")
            # informational: bare SI (prose / strings)
            for m in re.finditer(LEFT + r"SI" + RIGHT, line):
                bare_si += 1

    print("\n" + "=" * 70, file=sys.stderr)
    print(f"SCAN root={args.root}", file=sys.stderr)
    print(f"actionable SI-as-AI identifier hits (must be 0 for G1): {actionable}",
          file=sys.stderr)
    print(f"bare 'SI' occurrences (prose/strings, informational): {bare_si}",
          file=sys.stderr)
    return 1 if actionable else 0


def main():
    ap = argparse.ArgumentParser(description="SI -> AI rename tool")
    ap.add_argument("--root", default=".", help="repo root to operate on")
    ap.add_argument("--rules", default=DEFAULT_RULES)
    ap.add_argument("--group", action="append", default=[],
                    help="rule group(s) to apply (repeatable). Default: all.")
    ap.add_argument("--ext", action="append", default=[],
                    help="restrict to file extension(s), e.g. --ext .go")
    ap.add_argument("--apply", action="store_true",
                    help="write changes (default: dry-run diff)")
    ap.add_argument("--scan", action="store_true",
                    help="G1 verification: report residual SI identifiers")
    args = ap.parse_args()
    args.root = os.path.abspath(args.root)
    data = load_rules(args.rules)
    if args.scan:
        return cmd_scan(args, data)
    return cmd_rename(args, data)


if __name__ == "__main__":
    sys.exit(main())
