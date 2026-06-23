#!/usr/bin/env python3
"""
De-prefix construct names + migrate `mutation` -> `mutate` (C6, memql#2036).

Part of the DSL grammar redesign (epic #2031). C1-C5 made bare construct
names RESOLVE (structural dependency-tree validator: names resolve by
slot-kind + enclosing concept). This codemod removes the now-redundant
kind prefix from every declared construct NAME and migrates the mutation
header keyword from the noun `mutation` to the verb `mutate`.

Unlike scripts/rename/si_to_ai.py (a fixed curated token map), the
de-prefix is a TRANSFORM: strip the kind prefix from the declared name
and lower-case the first remaining letter. The script:

  (a) parses every dsl/**/*.memql declaration header
      `(query|mutation|mutate|spec|trait|logic) [<concept> ]<name> {`
  (b) computes oldName -> newName per the strip rule (only when the
      name actually begins with `<kind><UpperCase>`)
  (c) COLLISION CHECK: within the same resolution scope (slot-kind +
      enclosing concept) two names that de-prefix to the same bare name
      would collide. Build the map, detect collisions, HALT if any.
  (d) applies the exact-token rename map identifier-boundaried across
      .memql + .go (single combined pass -- no chaining/double-apply)
  (e) migrates `^mutation ` declaration headers to `mutate ` (.memql)

Kinds de-prefixed: query, mutation, spec, trait, logic.
(shape / seed names are already bare -- not in scope.)
The mutation->mutate header keyword swap is the ONLY place the bare word
`mutation` is touched; comments / docs / Go identifiers are left alone.

Usage:
  deprefix.py --root <repo>            # dry-run: print map + collisions + diff stat
  deprefix.py --root <repo> --apply    # write changes
  deprefix.py --root <repo> --map      # print the rename map only
"""
import argparse
import os
import re
import sys
from difflib import unified_diff

# Kinds whose NAMES carry a redundant prefix. The prefix string is the
# kind keyword; `mutation` names are prefixed `mutation` even though the
# header keyword migrates to `mutate`.
KIND_PREFIX = {
    "query": "query",
    "mutation": "mutation",
    "spec": "spec",
    "trait": "trait",
    "logic": "logic",
}

# Normalise mutation/mutate to one resolution slot for collision scoping.
SLOT = {"query": "query", "mutation": "mutate", "mutate": "mutate",
        "spec": "spec", "trait": "trait", "logic": "logic"}

# Declaration header: <kind> [<concept> ] <name> {  -- ALWAYS at column 0.
# (Indented `logic <name> { ... }` lines inside automation/logic bodies are
# step INVOCATIONS / references, not declarations -- they must not be parsed
# as headers.)
HEADER_RE = re.compile(
    r"(?m)^(query|mutation|mutate|spec|trait|logic)[ \t]+"
    r"(?:([A-Za-z_][A-Za-z0-9_]*)[ \t]+)?"
    r"([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{"
)

# mutation declaration-header keyword -> mutate. Matches the struct-form
# mutation header `[indent]mutation [<concept> ]<name> {` (mirrors the parser's
# mutationStructHeader). The trailing `{` requirement keeps it from touching a
# Go struct field named `mutation` or prose; applied to both .memql and .go
# (the latter migrates inline `.memql` test fixtures).
# The keyword may sit at column 0 (a .memql header or a raw-string fixture that
# opens with a newline), or immediately after a raw-string backtick / a
# double-quote (Go test fixtures: `src := ` + "`mutation ...`" or
# "mutation ...\n...").
MUTATION_KEYWORD_RE = re.compile(
    r'(?m)(^[ \t]*|`|")mutation('
    r"[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?[A-Za-z_][A-Za-z0-9_]*[ \t]*\{)"
)

LEFT = r"(?<![A-Za-z0-9_])"
RIGHT = r"(?![A-Za-z0-9_])"

EXCLUDED_DIR_NAMES = {
    ".git", "node_modules", "vendor", "dist", "build", ".next",
    "coverage", ".turbo", "__pycache__",
}
EXCLUDED_DIR_SUFFIXES = ("-worktrees",)
EXCLUDED_DIR_PREFIXES = ("memql-rel",)
APPLY_EXTS = {".go", ".memql"}


def iter_files(root, exts):
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [
            d for d in dirnames
            if d not in EXCLUDED_DIR_NAMES
            and not d.endswith(EXCLUDED_DIR_SUFFIXES)
            and not d.startswith(EXCLUDED_DIR_PREFIXES)
        ]
        for fn in filenames:
            if os.path.splitext(fn)[1] in exts:
                yield os.path.join(dirpath, fn)


def strip_prefix(kind, name):
    """Return the de-prefixed name, or the original if not prefixed."""
    prefix = KIND_PREFIX.get(kind)
    if not prefix:
        return name
    if (name.startswith(prefix) and len(name) > len(prefix)
            and name[len(prefix)].isupper()):
        rest = name[len(prefix):]
        return rest[0].lower() + rest[1:]
    return name


def parse_declarations(root):
    """Return list of dicts: file, kind, concept, old, new."""
    decls = []
    for path in iter_files(os.path.join(root, "dsl"), {".memql"}):
        with open(path, encoding="utf-8") as f:
            text = f.read()
        # Blank line-comments so a header-shaped line inside `//` is ignored.
        scan = re.sub(r"(?m)//.*$", "", text)
        for m in HEADER_RE.finditer(scan):
            kind, concept, name = m.group(1), m.group(2), m.group(3)
            decls.append({
                "file": path, "kind": kind, "concept": concept,
                "old": name, "new": strip_prefix(kind, name),
            })
    return decls


def collision_check(decls):
    """Return list of (scope, finalname, [oldnames]) HARD collisions.

    A hard collision is two constructs of the SAME resolution scope
    (slot-kind + enclosing concept) that de-prefix to the same bare name --
    the engine could not tell them apart. This HALTS the codemod.
    """
    scopes = {}
    for d in decls:
        scope = (SLOT.get(d["kind"], d["kind"]), d["concept"])
        scopes.setdefault(scope, {}).setdefault(d["new"], set()).add(d["old"])
    collisions = []
    for scope, finals in scopes.items():
        for final, olds in finals.items():
            if len(olds) > 1:
                collisions.append((scope, final, sorted(olds)))
    return collisions


# Kinds the Go/TS SDK flattens into one client namespace (sdk/gen collects
# query / mutate / logic / builtin). spec + trait are NOT emitted, so a
# spec/trait sharing a bare name is fine -- they never reach the SDK.
SDK_EMITTED_KINDS = {"query", "mutation", "mutate", "logic"}


def cross_namespace_collisions(decls):
    """Return [(bareName, [(kind, concept, old)])] for bare names claimed by
    two+ DISTINCT SDK-emitted declarations tree-wide (any concept).

    The engine resolves by slot-kind + concept so these are NOT engine
    ambiguities, but the Go/TS SDK flattens every emitted construct into one
    client namespace and rejects duplicate bare names -- so these need a manual
    disambiguating rename before `make sdk-gen-check` passes. Reported as a
    WARNING (not a halt); the de-prefix itself is still applied. (spec/trait are
    excluded -- they are not part of the SDK surface.)
    """
    by_name = {}
    for d in decls:
        if d["kind"] not in SDK_EMITTED_KINDS:
            continue
        key = (d["kind"], d["concept"], d["old"])
        by_name.setdefault(d["new"], set()).add(key)
    out = []
    for name, claimers in by_name.items():
        if len(claimers) > 1:
            out.append((name, sorted(claimers)))
    return out


def build_map(decls):
    """oldName -> newName for changed names. Asserts unique old keys."""
    rename = {}
    conflict = []
    for d in decls:
        if d["old"] == d["new"]:
            continue
        if d["old"] in rename and rename[d["old"]] != d["new"]:
            conflict.append((d["old"], rename[d["old"]], d["new"]))
        rename[d["old"]] = d["new"]
    return rename, conflict


def make_combined_pattern(rename):
    # Longest-first so a token is never pre-empted by a prefix of itself.
    keys = sorted(rename, key=len, reverse=True)
    if not keys:
        return None  # nothing to rename (already applied) -- keyword swap still runs
    alt = "|".join(re.escape(k) for k in keys)
    return re.compile(LEFT + "(?:" + alt + ")" + RIGHT)


def apply_to_text(text, ext, pattern, rename):
    counts = {}
    new = text

    if pattern is not None:
        def repl(m):
            tok = m.group(0)
            counts[tok] = counts.get(tok, 0) + 1
            return rename[tok]

        new = pattern.sub(repl, text)
    # Swap the mutation header keyword in .memql declarations and in inline
    # .memql fixtures embedded in .go test sources.
    new, kw = MUTATION_KEYWORD_RE.subn(r"\1mutate\2", new)
    return new, counts, kw


def main():
    ap = argparse.ArgumentParser(description="De-prefix construct names (C6)")
    ap.add_argument("--root", default=".")
    ap.add_argument("--apply", action="store_true")
    ap.add_argument("--map", action="store_true", help="print rename map only")
    ap.add_argument("--diff", action="store_true", help="print full unified diff")
    args = ap.parse_args()
    root = os.path.abspath(args.root)

    decls = parse_declarations(root)
    rename, conflict = build_map(decls)
    collisions = collision_check(decls)

    # Per-kind rename counts.
    by_kind = {}
    for d in decls:
        if d["old"] != d["new"]:
            by_kind[d["kind"]] = by_kind.get(d["kind"], 0) + 1

    if args.map:
        for old in sorted(rename):
            print(f"{old} -> {rename[old]}")
        print(f"\n# {len(rename)} names renamed", file=sys.stderr)
        return 0

    print("=" * 70, file=sys.stderr)
    print("RENAME COUNT BY KIND:", file=sys.stderr)
    for k in ("query", "mutation", "spec", "trait", "logic"):
        print(f"  {k:9s} {by_kind.get(k, 0)}", file=sys.stderr)
    print(f"  TOTAL     {sum(by_kind.values())}", file=sys.stderr)

    if conflict:
        print("\nMAP CONFLICT (same old -> two news!):", file=sys.stderr)
        for c in conflict:
            print(f"  {c}", file=sys.stderr)
        return 3

    print("\nCOLLISION CHECK (same scope -> same bare name):", file=sys.stderr)
    if collisions:
        for scope, final, olds in collisions:
            print(f"  HALT  scope={scope}  '{final}' <- {olds}", file=sys.stderr)
        print(f"\n{len(collisions)} collision(s) -- ABORTING.", file=sys.stderr)
        return 4
    print("  PASS -- zero same-scope collisions.", file=sys.stderr)

    # Cross-namespace (SDK-flat) collisions: a WARNING. The engine is fine
    # (slot-kind + concept resolution) but the SDK rejects duplicate bare
    # names, so each needs a manual disambiguating rename.
    cross = cross_namespace_collisions(decls)
    print("\nCROSS-NAMESPACE CHECK (SDK flat namespace -- WARNING only):",
          file=sys.stderr)
    if cross:
        for name, claimers in cross:
            who = ", ".join(f"{k} {c or '-'}/{old}" for k, c, old in claimers)
            print(f"  WARN  '{name}' claimed by: {who}", file=sys.stderr)
        print(f"  {len(cross)} cross-namespace dup(s) -- resolve by a manual "
              "disambiguating rename so `make sdk-gen-check` passes.",
              file=sys.stderr)
    else:
        print("  PASS -- zero cross-namespace dups.", file=sys.stderr)

    # Apply across .go + .memql.
    pattern = make_combined_pattern(rename)
    total_counts = {}
    changed = 0
    kw_total = 0
    for path in iter_files(root, APPLY_EXTS):
        ext = os.path.splitext(path)[1]
        try:
            with open(path, encoding="utf-8") as f:
                text = f.read()
        except (UnicodeDecodeError, OSError):
            continue
        new, counts, kw = apply_to_text(text, ext, pattern, rename)
        if new != text:
            changed += 1
            kw_total += kw
            for k, v in counts.items():
                total_counts[k] = total_counts.get(k, 0) + v
            if args.apply:
                with open(path, "w", encoding="utf-8") as f:
                    f.write(new)
            elif args.diff:
                rel = os.path.relpath(path, root)
                sys.stdout.writelines(unified_diff(
                    text.splitlines(keepends=True),
                    new.splitlines(keepends=True),
                    fromfile="a/" + rel, tofile="b/" + rel))

    print("\n" + "=" * 70, file=sys.stderr)
    print(f"{'APPLIED' if args.apply else 'DRY-RUN'}  root={root}", file=sys.stderr)
    print(f"files changed: {changed}", file=sys.stderr)
    print(f"name-reference substitutions: {sum(total_counts.values())}", file=sys.stderr)
    print(f"mutation->mutate header keyword swaps: {kw_total}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
