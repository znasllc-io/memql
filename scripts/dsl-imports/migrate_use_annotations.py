#!/usr/bin/env python3
"""Migrate @use* annotations to file-top Form B imports.

For each .memql file in dsl/:

  1. Find every @useConcept(X) / @useShape(X) / @useQuery(X) /
     @useMutation(X) / @useLogic(X) / @useBuiltin(X) annotation.
  2. Resolve each X via the construct-name index (build_index.py).
  3. Emit file-top `use <module>.{ names }` imports grouped by module.
  4. Strip the @use* annotation lines.
  5. For queries/mutations/shapes that previously carried @useConcept(X):
     hoist X into the signature ("query X name", "mutation X name",
     "shape X name"). The signature carries the binding now; the
     @useConcept line is gone.

Collisions: when one name resolves to multiple modules of different
kinds, the @use* annotation's kind (Concept/Shape/Query/Mutation/Logic/
Builtin) disambiguates. When two entries share the same kind, we abort
with a clear error so the author can rename one at source.

Usage:
    python3 scripts/dsl-imports/migrate_use_annotations.py [--dry-run] [path...]
"""
import argparse
import json
import re
import subprocess
import sys
from pathlib import Path

USE_RE = re.compile(
    r"^([ \t]*)@use(?P<kind>Concept|Shape|Query|Mutation|Logic|Builtin)\(([^)]+)\)[ \t]*$\n?",
    re.MULTILINE,
)

# Match query/mutation/shape declarations that need concept-in-signature.
# Captures: indent (g1), keyword (g2), name (g3).
CONSTRUCT_HEADER_RE = re.compile(
    r"^([ \t]*)(query|mutation|shape)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{",
    re.MULTILINE,
)

USE_KIND_TO_INDEX_KIND = {
    "Concept": "concept",
    "Shape": "shape",
    "Query": "query",
    "Mutation": "mutation",
    "Logic": "logic",
    "Builtin": "builtin",
}


def resolve(index: dict, name: str, want_kind: str, file_path: str) -> str:
    entries = index.get(name, [])
    if not entries:
        # Trailing-segment fallback: the legacy resolver matched bare
        # names against the trailing segment of canonical concept ids,
        # so @useConcept(request) could resolve a concept declared as
        # `clientToolRequest` with @namespace("cognition:client:tool")
        # (full id ends in `:request`). Try a lenient match: any
        # construct whose declared name ENDS WITH the requested name
        # (camelCase boundary).
        for k, vs in index.items():
            if k != name and (k.endswith(name[0].upper() + name[1:]) or k.endswith(name)):
                of_kind = [e for e in vs if e["kind"] == USE_KIND_TO_INDEX_KIND[want_kind]]
                if len(of_kind) == 1:
                    return of_kind[0]["module"]
        raise ValueError(f"{file_path}: @use{want_kind}({name}) but {name} is not declared anywhere in dsl/")
    of_kind = [e for e in entries if e["kind"] == USE_KIND_TO_INDEX_KIND[want_kind]]
    if len(of_kind) == 1:
        return of_kind[0]["module"]
    if not of_kind:
        # Fall back to ANY kind if only one entry exists (the kind hint is
        # advisory, not authoritative).
        if len(entries) == 1:
            return entries[0]["module"]
        kinds = ", ".join(f"{e['kind']}@{e['module']}" for e in entries)
        raise ValueError(f"{file_path}: @use{want_kind}({name}) — no {USE_KIND_TO_INDEX_KIND[want_kind]} of that name found; other matches: {kinds}")
    # Multiple of the same kind -- ambiguous.
    locs = ", ".join(e["module"] for e in of_kind)
    raise ValueError(f"{file_path}: @use{want_kind}({name}) is ambiguous across modules: {locs}")


def split_targets(raw: str) -> list[str]:
    """`@useConcept(a, b, c)` -> ['a', 'b', 'c']"""
    return [t.strip().strip('"') for t in raw.split(",") if t.strip()]


def migrate(content: str, index: dict, file_path: str, my_module: str) -> tuple[str, list[str]]:
    log: list[str] = []

    # Collect every @use* annotation as a whole line, with the names
    # it lists. We drop the whole line only when EVERY listed name
    # resolves; if any name is external/unresolved, the line stays.
    use_lines: list[dict] = []
    for m in USE_RE.finditer(content):
        kind = m.group("kind")
        names = split_targets(m.group(3))
        use_lines.append({
            "start": m.start(),
            "end": m.end(),
            "indent": m.group(1),
            "kind": kind,
            "names": names,
            "line": m.group(0),
        })

    imports: dict[str, set[str]] = {}
    use_concept_names: list[str] = []
    drop_line: list[bool] = []
    for ul in use_lines:
        if ul["kind"] == "Concept":
            use_concept_names.extend(ul["names"])
        all_resolved = True
        for name in ul["names"]:
            try:
                target_module = resolve(index, name, ul["kind"], file_path)
            except ValueError as exc:
                log.append(f"  WARN: {exc}; leaving @use{ul['kind']}({name}) in place")
                all_resolved = False
                continue
            if target_module == my_module:
                continue  # same-file: no import needed
            imports.setdefault(target_module, set()).add(name)
        drop_line.append(all_resolved)

    # Build the new file-top import block. Sort by module path; sort
    # names within each module so output is stable across runs.
    import_lines = []
    for module in sorted(imports):
        names_sorted = sorted(imports[module])
        import_lines.append(f"use {module}.{{ {', '.join(names_sorted)} }}")

    # Strip @use* lines whose names all resolved (per `drop_line`).
    # Lines with unresolved names stay.
    pieces: list[str] = []
    pos = 0
    stripped_count = 0
    for ul, drop in zip(use_lines, drop_line):
        pieces.append(content[pos : ul["start"]])
        if drop:
            stripped_count += 1
        else:
            pieces.append(ul["line"])
        pos = ul["end"]
    pieces.append(content[pos:])
    body = "".join(pieces)
    if stripped_count:
        log.append(f"  stripped {stripped_count} @use* annotation lines")

    # Hoist @useConcept(name) into the signature of query / mutation /
    # shape declarations. We pair each removed @useConcept with the
    # NEXT non-skipped declaration. The pairing relies on the fact
    # that authors write @useConcept directly above the construct they
    # bind (the existing declared-usage validator enforces this rule).
    if use_concept_names:
        # Iterate construct headers in order; consume use_concept_names
        # FIFO per header that doesn't already carry a signature concept.
        concept_iter = iter(use_concept_names)
        pending_concept = next(concept_iter, None)

        def replace_header(m: re.Match) -> str:
            nonlocal pending_concept
            indent, keyword, name = m.group(1), m.group(2), m.group(3)
            if pending_concept is None:
                return m.group(0)
            new_header = f"{indent}{keyword} {pending_concept} {name} {{"
            log.append(f"  {keyword} {name}: signature now `{keyword} {pending_concept} {name}`")
            pending_concept = next(concept_iter, None)
            return new_header

        body = CONSTRUCT_HEADER_RE.sub(replace_header, body)
        if pending_concept is not None or len(use_concept_names) > 0:
            # Best-effort: warn if any unconsumed concepts remain.
            remaining = []
            if pending_concept is not None:
                remaining.append(pending_concept)
                remaining.extend(list(concept_iter))
            if remaining:
                log.append(f"  WARN: leftover @useConcept names not paired with a header: {remaining}")

    # Prepend the new import block. Insert after any leading comment
    # block (preserves the file header doc-comment). The cleanest
    # place: just before the first non-comment, non-blank line.
    if import_lines:
        block = "\n".join(import_lines) + "\n\n"
        # Find the first non-comment non-blank non-`use` line.
        insert_at = _find_import_insertion_point(body)
        body = body[:insert_at] + block + body[insert_at:]
        log.append(f"  prepended {len(import_lines)} Form B import line(s)")

    return body, log


def _find_import_insertion_point(text: str) -> int:
    """Skip leading file-header comment block + blank lines + any
    existing `use ...` clauses. Returns the byte offset for inserting
    new imports.
    """
    i = 0
    while i < len(text):
        # Find end of line.
        eol = text.find("\n", i)
        if eol == -1:
            eol = len(text)
        line = text[i:eol].strip()
        if line == "" or line.startswith("//"):
            i = eol + 1
            continue
        if line.startswith("use "):
            # Form B may span multiple lines; advance through balanced braces.
            j = i
            depth = 0
            seen_brace = False
            while j < len(text):
                ch = text[j]
                if ch == "{":
                    depth += 1
                    seen_brace = True
                elif ch == "}":
                    depth -= 1
                    if seen_brace and depth == 0:
                        j += 1
                        break
                elif ch == "\n" and not seen_brace:
                    break
                j += 1
            i = j
            if i < len(text) and text[i] == "\n":
                i += 1
            continue
        return i
    return i


def path_to_module(p: Path, root: Path) -> str:
    rel = p.relative_to(root).with_suffix("")
    parts = rel.parts
    if parts and parts[0] == "dsl":
        parts = parts[1:]
    return ".".join(parts)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("paths", nargs="*")
    args = parser.parse_args()

    root = Path(".").resolve()

    # (Re-)build the construct index fresh; we want it to reflect the
    # current tree (e.g. seeds that were just migrated).
    proc = subprocess.run(
        ["python3", "scripts/dsl-imports/build_index.py"],
        cwd=root, check=True, capture_output=True, text=True,
    )
    index = json.loads(proc.stdout)

    if args.paths:
        files = [Path(p) for p in args.paths]
    else:
        files = []
        for p in sorted((root / "dsl").rglob("*.memql")):
            if p.name.startswith("_"):
                continue
            if "@use" in p.read_text():
                files.append(p)

    total_migrated = 0
    total_errors = 0
    for path in files:
        original = path.read_text()
        my_module = path_to_module(path, root)
        try:
            new, log = migrate(original, index, str(path), my_module)
        except ValueError as exc:
            print(f"ERROR {path}: {exc}", file=sys.stderr)
            total_errors += 1
            continue
        if new == original:
            continue
        total_migrated += 1
        print(f"=== {path}")
        for entry in log:
            print(f"    {entry}")
        if not args.dry_run:
            path.write_text(new)

    print(f"\nmigrated {total_migrated}/{len(files)} files; {total_errors} errors (dry-run={args.dry_run})")
    if total_errors:
        sys.exit(1)


if __name__ == "__main__":
    main()
