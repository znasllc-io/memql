#!/usr/bin/env python3
# Phase G.2 spec migration: add `@shape("name")` annotation above
# each `spec NAME { ... }` declaration.
#
# Strategy: classify each spec by the field paths in its body, map to
# the shape that covers those fields. The mapping is hand-curated
# below; the migration script applies it mechanically.
from __future__ import annotations

import os
import re
import sys

root = os.environ.get("ROOT")
if not root:
    sys.stderr.write("ROOT env var is required\n")
    sys.exit(1)

specs_dir = os.path.join(root, "dsl", "v1", "specs")
if not os.path.isdir(specs_dir):
    sys.stderr.write(f"missing {specs_dir}\n")
    sys.exit(1)

# Per-spec shape binding. Maps the spec name to the shape it should
# bind to.
SPEC_TO_SHAPE = {
    # Concept-bound specs.
    "specIsHumanParticipant": "participantFull",
    "specIsSIParticipant":    "participantFull",
    # Caller-bound spec.
    "requiresAdmin":          "callerActor",
    # Cross-concept trait specs.
    "specIsActiveRecord":     "activeRowTrait",
    "specIsCancelled":        "statusRowTrait",
    "specIsCompleted":        "statusRowTrait",
    "specIsInProgress":       "statusRowTrait",
    "specIsOpen":             "statusRowTrait",
    "specIsScheduled":        "statusRowTrait",
    "specStatusIsActive":     "statusRowTrait",
    "specStatusIsArchived":   "archivedRowTrait",
    "specStatusIsSaved":      "savedRowTrait",
    "specIsNotArchived":      "archivedRowTrait",
    "specIsNotDeleted":       "deletedRowTrait",
    "specIsChecked":          "validationRowTrait",
    "specIsConfirmed":        "validationRowTrait",
    "specIsDraft":            "validationRowTrait",
    "specIsUsable":           "validationRowTrait",
}

# Match the spec header line.
SPEC_HEADER_RE = re.compile(
    r"(?m)^([ \t]*)spec[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{"
)

# Already-migrated marker.
SHAPE_ANNOT_RE = re.compile(r"(?m)^[ \t]*@shape\s*\(")


def migrate(text: str) -> tuple[str, bool, str]:
    m = SPEC_HEADER_RE.search(text)
    if not m:
        return text, False, "no spec header"
    indent, name = m.group(1), m.group(2)
    if SHAPE_ANNOT_RE.search(text):
        return text, False, "already has @shape(...)"
    shape_name = SPEC_TO_SHAPE.get(name)
    if not shape_name:
        return text, False, f"no mapping for spec {name!r}"
    # Insert `@shape("...")` on its own line immediately above the
    # `spec NAME {` header. Preserve indentation.
    line_start = text.rfind("\n", 0, m.start()) + 1
    insert_text = f'{indent}@shape("{shape_name}")\n'
    new_text = text[:line_start] + insert_text + text[line_start:]
    return new_text, True, "migrated"


def main() -> int:
    migrated = 0
    unchanged = 0
    skipped = 0
    skipped_reasons: dict[str, int] = {}

    for cur_root, _, files in os.walk(specs_dir):
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
