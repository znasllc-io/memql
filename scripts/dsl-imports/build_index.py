#!/usr/bin/env python3
"""Walk dsl/*.memql and emit a JSON name -> module map.

Used by the migration tool (migrate.py) to translate `@useConcept(X)` /
`@useShape(X)` / `@useQuery(X)` / `@useMutation(X)` / `@useLogic(X)` /
`@useBuiltin(X)` annotations into file-top `use module.{ X }` imports.

A construct is a top-level named declaration:
    concept | query | mutation | automation | logic |
    shape   | spec  | trait    | tool       | prompt |
    provider | builtin | policy

Module path = dotted, rooted at dsl/. E.g. dsl/common/traits.memql ->
"common.traits".

Output shape:
    {
      "traitIsActiveRecord": [
        {"kind": "trait", "module": "common.traits",
         "path": "dsl/common/traits.memql"}
      ],
      ...
    }

Collisions (one name in multiple modules) are reported with all
matches; the migrator decides per-call which to pick.
"""
import json
import re
import sys
from pathlib import Path

KIND_KEYWORDS = (
    "concept", "query", "mutation", "automation", "logic",
    "shape", "spec", "trait", "tool", "prompt",
    "provider", "builtin", "policy",
)

DECL_RE = re.compile(
    r"^(?P<kw>" + "|".join(KIND_KEYWORDS) + r")\s+(?P<name>[A-Za-z_][A-Za-z0-9_]*)\s*[\{({]"
)


def path_to_module(p: Path, root: Path) -> str:
    rel = p.relative_to(root).with_suffix("")
    parts = rel.parts
    if parts and parts[0] == "dsl":
        parts = parts[1:]
    return ".".join(parts)


def main():
    root = Path(sys.argv[1] if len(sys.argv) > 1 else ".").resolve()
    dsl_root = root / "dsl"
    if not dsl_root.is_dir():
        print(f"no dsl/ directory under {root}", file=sys.stderr)
        sys.exit(2)
    index: dict[str, list[dict[str, str]]] = {}
    for path in sorted(dsl_root.rglob("*.memql")):
        if path.name.startswith("_"):
            continue
        module = path_to_module(path, root)
        text = path.read_text()
        for line in text.splitlines():
            m = DECL_RE.match(line)
            if not m:
                continue
            kind = m.group("kw")
            name = m.group("name")
            index.setdefault(name, []).append({
                "kind": kind,
                "module": module,
                "path": str(path.relative_to(root)),
            })
    json.dump(index, sys.stdout, indent=2, sort_keys=True)
    print()


if __name__ == "__main__":
    main()
