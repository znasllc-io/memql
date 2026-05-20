#!/usr/bin/env python3
"""Migrate seed files to the new Form B + signature-bound shape.

INPUT (legacy):

    use agents.agentRole

    @version("1.0.0")
    @scope("global")
    @description("Corn / soy / wheat agronomy ...")
    seed row_crop_farmer {
      id:                    "row_crop_farmer"
      slug:                  "row_crop_farmer"
      name:                  "Row Crop Farmer"
      description:           "Corn / soy / wheat agronomy ..."
      category:              "agriculture"
      ...
      predefined:            true
    }

OUTPUT (canonical):

    use agents.concepts.{ agentRole }

    @description("Corn / soy / wheat agronomy ...")
    seed agentRole row_crop_farmer {
      slug:                  "row_crop_farmer"
      name:                  "Row Crop Farmer"
      category:              "agriculture"
      ...
      predefined:            true
    }

Transformations:
  - `use <ns>.<concept>` -> `use <ns>.concepts.{ <concept> }`
  - `seed <name> {` -> `seed <concept> <name> {`  (concept from file-top use)
  - drop `@scope("global")` (default)
  - drop `@version("...")` (vestigial)
  - drop `id:` body field (auto-derived from name by the loader)
  - drop `description:` body field when string matches @description annotation

What is NOT dropped (kept for now):
  - `slug:` body field (not auto-derived without concept schema lookup)
  - `predefined:` body field (concept-specific default; not auto-set)

Usage:
    python3 scripts/dsl-imports/migrate_seeds.py [--dry-run] [path-glob...]
    # default glob: dsl/**/*.memql files that contain at least one `seed` block
"""
import argparse
import re
import sys
from pathlib import Path

USE_CLAUSE_RE = re.compile(
    r"^([ \t]*)use[ \t]+([A-Za-z_][A-Za-z0-9_]*)\.([A-Za-z_][A-Za-z0-9_]*)[ \t]*$",
    re.MULTILINE,
)

SEED_HEADER_RE = re.compile(
    r"^([ \t]*)seed[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{",
    re.MULTILINE,
)

SCOPE_GLOBAL_RE = re.compile(r"^[ \t]*@scope\(\"global\"\)[ \t]*$\n?", re.MULTILINE)
VERSION_RE = re.compile(r"^[ \t]*@version\(\"[^\"]*\"\)[ \t]*$\n?", re.MULTILINE)
ID_FIELD_RE = re.compile(
    r"^[ \t]*id:[ \t]*\"([^\"]*)\"[ \t]*$\n?",
    re.MULTILINE,
)
DESC_ANNOTATION_RE = re.compile(
    r"@description\(\"((?:[^\"\\]|\\.)*)\"\)",
)
DESC_FIELD_RE = re.compile(
    r"^[ \t]*description:[ \t]*\"((?:[^\"\\]|\\.)*)\"[ \t]*$",
    re.MULTILINE,
)


def migrate(content: str, path: str) -> tuple[str, list[str]]:
    """Apply the seed migration to `content`. Returns (new_content, log)."""
    log: list[str] = []

    # Extract the file-top `use <ns>.<concept>` clause.
    use_match = USE_CLAUSE_RE.search(content)
    if not use_match:
        log.append("no file-top `use <ns>.<concept>` -- skipping (already migrated or non-seed file)")
        return content, log
    indent, ns, concept = use_match.group(1), use_match.group(2), use_match.group(3)
    new_use = f"{indent}use {ns}.concepts.{{ {concept} }}"
    content = content[: use_match.start()] + new_use + content[use_match.end() :]
    log.append(f"`use {ns}.{concept}` -> `{new_use.strip()}`")

    # Inject concept into each `seed <name> {` header.
    def signature_replace(m):
        seed_name = m.group(2)
        log.append(f"  seed {seed_name}: signature now `seed {concept} {seed_name}`")
        return f"{m.group(1)}seed {concept} {seed_name} {{"

    content, n = SEED_HEADER_RE.subn(signature_replace, content)
    log.append(f"  rewrote {n} seed signatures")

    # Drop @scope("global") annotation lines.
    content, n = SCOPE_GLOBAL_RE.subn("", content)
    if n:
        log.append(f"  stripped {n} @scope(\"global\") annotations")

    # Drop @version("...") annotation lines.
    content, n = VERSION_RE.subn("", content)
    if n:
        log.append(f"  stripped {n} @version annotations")

    # Drop body `id:` fields that match the seed declaration name. (We
    # walk all matches; the loader auto-derives id from the seed name
    # post-migration, so a body `id` is redundant.)
    content, n = ID_FIELD_RE.subn("", content)
    if n:
        log.append(f"  stripped {n} body `id:` fields (auto-derived from seed name)")

    # Drop body `description:` fields when they match the seed's
    # @description annotation. This requires per-seed parsing -- walk
    # each seed block and elide the duplicate.
    content = _drop_duplicate_description_fields(content, log)

    return content, log


def _drop_duplicate_description_fields(content: str, log: list[str]) -> str:
    """For each `seed Concept name { ... }` block, if the preceding
    `@description("X")` annotation has the same value as a body
    `description: "X"` field, drop the body field.
    """
    # Strategy: find each seed block; look back for the nearest
    # @description annotation in the seed's preamble; if it matches a
    # description field in the body, remove that field.
    seed_block_re = re.compile(
        r"(?P<header>seed\s+[A-Za-z_][A-Za-z0-9_]*\s+[A-Za-z_][A-Za-z0-9_]*\s*\{)",
    )
    pieces: list[str] = []
    last_end = 0
    while True:
        m = seed_block_re.search(content, last_end)
        if not m:
            break
        # Find @description in the preamble before this seed header.
        preamble = content[last_end : m.start()]
        desc_match = None
        for dm in DESC_ANNOTATION_RE.finditer(preamble):
            desc_match = dm  # take the last
        if desc_match:
            desc_value = desc_match.group(1)
            # Find the matching closing brace of this seed block.
            brace_pos = m.end() - 1  # index of `{`
            depth = 0
            end_brace = -1
            for j in range(brace_pos, len(content)):
                if content[j] == "{":
                    depth += 1
                elif content[j] == "}":
                    depth -= 1
                    if depth == 0:
                        end_brace = j
                        break
            if end_brace > 0:
                body = content[m.end() : end_brace]
                # Strip `description: "<desc_value>"` field lines from
                # the body when the literal value matches.
                new_body = re.sub(
                    r"^[ \t]*description:[ \t]*\"" + re.escape(desc_value) + r"\"[ \t]*\n?",
                    "",
                    body,
                    flags=re.MULTILINE,
                )
                if new_body != body:
                    log.append(
                        f"  dropped duplicate description: in seed block (matched @description)"
                    )
                pieces.append(content[last_end : m.end()])
                pieces.append(new_body)
                pieces.append(content[end_brace : end_brace + 1])
                last_end = end_brace + 1
                continue
        # No replacement; pass through.
        pieces.append(content[last_end : m.end()])
        last_end = m.end()
    pieces.append(content[last_end:])
    return "".join(pieces)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--dry-run", action="store_true", help="print diffs only")
    parser.add_argument("paths", nargs="*", help="files to migrate (default: all seed files)")
    args = parser.parse_args()

    root = Path(".").resolve()
    if args.paths:
        files = [Path(p) for p in args.paths]
    else:
        # default: every .memql file that contains a `seed ` line
        files = []
        for p in sorted((root / "dsl").rglob("*.memql")):
            if re.search(r"^[ \t]*seed[ \t]+", p.read_text(), re.MULTILINE):
                files.append(p)

    total = 0
    for path in files:
        original = path.read_text()
        new, log = migrate(original, str(path))
        if new == original:
            continue
        total += 1
        print(f"=== {path}")
        for entry in log:
            print(f"    {entry}")
        if not args.dry_run:
            path.write_text(new)
    print(f"\nmigrated {total}/{len(files)} files (dry-run={args.dry_run})")


if __name__ == "__main__":
    main()
