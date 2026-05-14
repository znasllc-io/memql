#!/usr/bin/env bash
# refactor-shape-struct.sh — converts every legacy shape file under
# dsl/v1/shapes/ from the receiver-function form to the canonical
# struct-style form. Idempotent: files already on the new form are
# unchanged.
#
# Before:
#
#   @description("...")
#   @concepts("...")
#   func (Shape) participantFull {
#     @template({
#       node("id"),
#       node("payload.spaceID"),
#       ...
#       node("createdAt")
#     })
#   }
#
# After:
#
#   @description("...")
#   @concepts("...")
#   shape participantFull {
#     id
#     payload.spaceID
#     ...
#     createdAt
#   }
#
# The transformation is done in Python because the multi-line block
# extraction + per-line path rewrite is awkward in pure sed/perl.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SHAPES="$ROOT/dsl/v1/shapes"

if [[ ! -d "$SHAPES" ]]; then
  echo "missing $SHAPES — wrong working tree?"
  exit 1
fi

python3 - <<'PY'
import os
import re
import sys

shapes_dir = os.path.join(os.environ.get("ROOT", "."), "dsl", "v1", "shapes")
if not os.path.isdir(shapes_dir):
    # Fallback: derive from cwd if env-passed ROOT didn't work
    here = os.path.abspath(os.path.dirname(__file__))
    shapes_dir = os.path.join(here, "..", "..", "dsl", "v1", "shapes")
shapes_dir = os.path.abspath(shapes_dir)

# Pattern: capture pre-amble (annotations + use, etc.), shape name,
# and the body containing @template({ ... }) entries.
HEADER_RE = re.compile(
    r"^(?P<head>.*?)^func \(Shape\) (?P<name>[A-Za-z0-9_]+)\s*\{\s*\n"
    r"\s*@template\(\{\s*\n(?P<body>.*?)\n\s*\}\)\s*\n\}\s*\n?\s*$",
    re.DOTALL | re.MULTILINE,
)

# Capture every node("path") entry inside the template body.
NODE_RE = re.compile(r'node\("([^"]+)"\)')

migrated = 0
skipped = 0
unchanged = 0

for root, _, files in os.walk(shapes_dir):
    for fname in files:
        if not fname.endswith(".memql"):
            continue
        if fname.startswith("_"):
            continue
        path = os.path.join(root, fname)
        with open(path, "r", encoding="utf-8") as f:
            content = f.read()
        if re.search(r"^\s*shape\s+[A-Za-z0-9_]+\s*\{", content, re.MULTILINE):
            unchanged += 1
            continue
        m = HEADER_RE.search(content)
        if not m:
            skipped += 1
            print(f"  SKIP (no match): {path}")
            continue
        head = m.group("head").rstrip() + "\n"
        name = m.group("name")
        body = m.group("body")
        paths = NODE_RE.findall(body)
        if not paths:
            skipped += 1
            print(f"  SKIP (no node() entries): {path}")
            continue
        lines = ["  " + p for p in paths]
        new_content = f"{head}shape {name} {{\n" + "\n".join(lines) + "\n}\n"
        with open(path, "w", encoding="utf-8") as f:
            f.write(new_content)
        migrated += 1

print(f"migrated: {migrated}  unchanged: {unchanged}  skipped: {skipped}")
PY
