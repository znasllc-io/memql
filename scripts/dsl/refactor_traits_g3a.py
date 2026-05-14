#!/usr/bin/env python3
# Phase G.3.a migration:
#
# 1. Convert 15 cross-concept specs + 1 caller-bound spec into traits.
#    Specs that bound to trait shapes (`@shape("activeRowTrait")` etc.)
#    OR to the caller-actor shape become traits, since traits are the
#    home for concept-agnostic atomic predicates.
#
# 2. Rename canonical names: specXxx -> traitXxx.
#    - File contents: `spec specXxx { ... }` -> `trait traitXxx { ... }`
#    - All cross-construct references (queries' filter clauses, etc.)
#      that called specXxx now call traitXxx.
#
# 3. Drop the `@shape(...)` annotation -- traits have no shape binding.
#
# 4. Delete the 6 trait shape files (now redundant).
from __future__ import annotations

import os
import re
import sys

root = os.environ.get("ROOT")
if not root:
    sys.stderr.write("ROOT env var is required\n")
    sys.exit(1)

dsl_dir = os.path.join(root, "dsl", "v1")
if not os.path.isdir(dsl_dir):
    sys.stderr.write(f"missing {dsl_dir}\n")
    sys.exit(1)

# Specs that become traits (concept-agnostic OR caller-only). Maps
# old spec name -> new trait name.
SPEC_TO_TRAIT = {
    "specIsActiveRecord":     "traitIsActiveRecord",
    "specIsCancelled":        "traitIsCancelled",
    "specIsCompleted":        "traitIsCompleted",
    "specIsInProgress":       "traitIsInProgress",
    "specIsOpen":             "traitIsOpen",
    "specIsScheduled":        "traitIsScheduled",
    "specStatusIsActive":     "traitStatusIsActive",
    "specStatusIsArchived":   "traitStatusIsArchived",
    "specStatusIsSaved":      "traitStatusIsSaved",
    "specIsNotArchived":      "traitIsNotArchived",
    "specIsNotDeleted":       "traitIsNotDeleted",
    "specIsChecked":          "traitIsChecked",
    "specIsConfirmed":        "traitIsConfirmed",
    "specIsDraft":            "traitIsDraft",
    "specIsUsable":           "traitIsUsable",
    "requiresAdmin":          "traitRequiresAdmin",
}

# Trait-shape names that lose their callers (the cross-concept specs
# migrated above were the only consumers). Delete these files.
TRAIT_SHAPES_TO_DELETE = {
    "activeRowTrait",
    "statusRowTrait",
    "deletedRowTrait",
    "archivedRowTrait",
    "savedRowTrait",
    "validationRowTrait",
}

# Caller-actor shape was created for `requiresAdmin`; since that
# migrates to a trait (with no shape binding), the shape becomes
# orphaned and is deleted too.
CALLER_SHAPES_TO_DELETE = {
    "callerActor",
}


def transform_spec_to_trait(text: str, old_name: str, new_name: str) -> str:
    """Rewrite a spec file as a trait file:
       - Drop the `@shape("...")` annotation line.
       - Change `spec <oldName> { ... }` to `trait <newName> { ... }`.
    """
    # Drop @shape("...") annotation lines.
    text = re.sub(r"(?m)^[ \t]*@shape\s*\([^)]*\)\s*\n", "", text)
    # Swap the spec header for a trait header.
    text = re.sub(
        rf"(?m)^([ \t]*)spec[ \t]+{re.escape(old_name)}[ \t]*\{{",
        rf"\1trait {new_name} {{",
        text,
    )
    return text


def update_references(text: str) -> tuple[str, int]:
    """Rename specXxx -> traitXxx in the rest of the DSL tree (call
    sites in queries, mutations, automations, prompts, etc.).
    Returns (new_text, replacements_made)."""
    count = 0
    for old, new in SPEC_TO_TRAIT.items():
        # Match the name as a whole identifier (no broader matches).
        pattern = re.compile(rf"\b{re.escape(old)}\b")
        new_text, n = pattern.subn(new, text)
        count += n
        text = new_text
    return text, count


def main() -> int:
    # Step 1: rewrite spec files into trait files.
    specs_renamed = 0
    for cur_root, _, files in os.walk(os.path.join(dsl_dir, "specs")):
        for fname in files:
            if not fname.endswith(".memql"):
                continue
            if fname.startswith("_"):
                continue
            path = os.path.join(cur_root, fname)
            with open(path, "r", encoding="utf-8") as f:
                content = f.read()
            # Find the spec header to identify the file's spec.
            m = re.search(r"(?m)^[ \t]*spec[ \t]+([A-Za-z_][A-Za-z0-9_]*)\b", content)
            if not m:
                continue
            old_name = m.group(1)
            if old_name not in SPEC_TO_TRAIT:
                continue
            new_name = SPEC_TO_TRAIT[old_name]
            new_content = transform_spec_to_trait(content, old_name, new_name)
            with open(path, "w", encoding="utf-8") as f:
                f.write(new_content)
            specs_renamed += 1
            print(f"  trait: {old_name} -> {new_name}  ({path})")

    print(f"specs converted to traits: {specs_renamed}")

    # Step 2: rename references across the whole DSL tree.
    refs_updated = 0
    files_touched = 0
    for cur_root, _, files in os.walk(dsl_dir):
        for fname in files:
            if not fname.endswith(".memql"):
                continue
            path = os.path.join(cur_root, fname)
            with open(path, "r", encoding="utf-8") as f:
                content = f.read()
            new_content, n = update_references(content)
            if n > 0 and new_content != content:
                with open(path, "w", encoding="utf-8") as f:
                    f.write(new_content)
                refs_updated += n
                files_touched += 1

    print(f"references renamed: {refs_updated} across {files_touched} files")

    # Step 3: delete trait shape files.
    deleted = 0
    for cur_root, _, files in os.walk(os.path.join(dsl_dir, "shapes")):
        for fname in files:
            if not fname.endswith(".memql"):
                continue
            stem = fname[:-len(".memql")]
            if stem in TRAIT_SHAPES_TO_DELETE or stem in CALLER_SHAPES_TO_DELETE:
                path = os.path.join(cur_root, fname)
                os.remove(path)
                deleted += 1
                print(f"  deleted: {path}")
    print(f"shape files deleted: {deleted}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
