#!/usr/bin/env python3
# Migrate all dsl/v1/shapes/v1/**.memql files to the new Phase G shape
# form. Today's shapes look like:
#
#   @description("...")
#   @concepts("v1:cognition:space")
#   shape spaceCard {
#     id
#     payload.name
#     createdAt
#   }
#
# Phase G adds the `@row` annotation and `row.` prefix on every path:
#
#   @description("...")
#   @row
#   @concepts("v1:cognition:space")
#   shape spaceCard {
#     row.id
#     row.payload.name
#     row.createdAt
#   }
#
# Every existing shape in the tree is row-kind (all have @concepts).
# This script only touches struct-form shapes; legacy func (Shape)
# files are left alone (none exist today but we don't want to break
# them if any reappear).
from __future__ import annotations

import os
import re
import sys

root = os.environ.get("ROOT")
if not root:
    sys.stderr.write("ROOT env var is required\n")
    sys.exit(1)

shapes_dir = os.path.join(root, "dsl", "v1", "shapes")
if not os.path.isdir(shapes_dir):
    sys.stderr.write(f"missing {shapes_dir}\n")
    sys.exit(1)

# Struct-form shape header.
SHAPE_HEADER_RE = re.compile(
    r"(?m)^[ \t]*shape[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{"
)

# Already-migrated marker: any line starting with @row or @caller.
KIND_ANNOT_RE = re.compile(r"(?m)^[ \t]*@(row|caller)\b")

# A body line that's a single dotted path (the lines we want to
# prefix with `row.`).
PATH_LINE_RE = re.compile(
    r"^([ \t]*)([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*(,?)\s*(//.*)?$"
)


def find_matching_close_brace(text: str, open_idx: int) -> int:
    depth = 0
    for i in range(open_idx, len(text)):
        c = text[i]
        if c == "{":
            depth += 1
        elif c == "}":
            depth -= 1
            if depth == 0:
                return i
    return -1


def find_concept_line(text: str) -> tuple[int, int]:
    """Returns (start, end) of the first `@concepts(...)` annotation
    line. Used as the anchor for inserting `@row` right above it.
    Returns (-1, -1) if not found."""
    m = re.search(r"(?m)^[ \t]*@concepts\s*\(", text)
    if not m:
        return -1, -1
    return m.start(), m.end()


def migrate(text: str) -> tuple[str, bool, str]:
    """Returns (new_text, changed, message)."""
    header = SHAPE_HEADER_RE.search(text)
    if not header:
        return text, False, "no struct-form shape header"

    if KIND_ANNOT_RE.search(text):
        return text, False, "already has @row/@caller"

    cs, ce = find_concept_line(text)
    if cs < 0:
        return text, False, "no @concepts(...) anchor; cannot infer @row vs @caller"

    # Insert `@row` on its own line immediately BEFORE the @concepts line.
    line_start = text.rfind("\n", 0, cs) + 1
    indent = text[line_start:cs]
    insert_text = f"{indent}@row\n"
    new_text = text[:line_start] + insert_text + text[line_start:]

    # Now find the (new) shape header in the rewritten source and
    # prefix every path line in the body with `row.`.
    header2 = SHAPE_HEADER_RE.search(new_text)
    if not header2:
        return text, False, "internal: shape header vanished"
    open_brace = header2.end() - 1
    close_brace = find_matching_close_brace(new_text, open_brace)
    if close_brace < 0:
        return text, False, "unbalanced braces in shape body"

    body = new_text[open_brace + 1 : close_brace]
    body_lines = body.split("\n")
    out_lines = []
    for line in body_lines:
        # Preserve blank lines and pure comment lines as-is.
        stripped = line.strip()
        if not stripped or stripped.startswith("//"):
            out_lines.append(line)
            continue
        m = PATH_LINE_RE.match(line)
        if not m:
            # Unrecognised line shape -- leave alone.
            out_lines.append(line)
            continue
        indent_line, path, trailing_comma, trailing_comment = m.groups()
        # Already prefixed (defensive — shouldn't happen since we
        # guard with KIND_ANNOT_RE above).
        if path.startswith("row."):
            out_lines.append(line)
            continue
        new_line = f"{indent_line}row.{path}"
        if trailing_comma:
            new_line += trailing_comma
        if trailing_comment:
            new_line += " " + trailing_comment
        out_lines.append(new_line)

    new_body = "\n".join(out_lines)
    new_text = new_text[: open_brace + 1] + new_body + new_text[close_brace:]
    return new_text, True, "migrated"


def main() -> int:
    migrated = 0
    unchanged = 0
    skipped = 0
    skipped_reasons: dict[str, int] = {}

    for cur_root, _, files in os.walk(shapes_dir):
        for fname in files:
            if not fname.endswith(".memql"):
                continue
            if fname.startswith("_"):
                continue
            path = os.path.join(cur_root, fname)
            with open(path, "r", encoding="utf-8") as f:
                content = f.read()
            new_content, changed, msg = migrate(content)
            if changed:
                with open(path, "w", encoding="utf-8") as f:
                    f.write(new_content)
                migrated += 1
            elif "already" in msg:
                unchanged += 1
            else:
                skipped += 1
                skipped_reasons[msg] = skipped_reasons.get(msg, 0) + 1

    print(f"migrated: {migrated}  unchanged: {unchanged}  skipped: {skipped}")
    for reason, count in sorted(skipped_reasons.items()):
        print(f"  {count}: {reason}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
