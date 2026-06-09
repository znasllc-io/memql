#!/usr/bin/env python3
"""Assemble the docs bundle tree + manifest.json from docs/public.

Invoked by build-docs-bundle.sh. Reads env: PUBLIC_DIR, OUT, VERSION,
ENGINE_VERSION. Selects markdown files whose front-matter is
`audience: public`, copies them (plus any non-markdown generated assets,
e.g. the architecture model JSON) into OUT preserving the path relative to
docs/public, and writes OUT/manifest.json describing the nav tree by area.
"""
import json
import os
import shutil

PUBLIC = os.environ["PUBLIC_DIR"]
OUT = os.environ["OUT"]
VERSION = os.environ["VERSION"]
ENGINE_VERSION = os.environ.get("ENGINE_VERSION", "")

AREAS = ["overview", "concepts", "language", "ai", "build", "operate", "cockpit"]


def front_matter(path):
    """Return the YAML front-matter as a flat dict (best-effort), or {}."""
    fm = {}
    try:
        with open(path, encoding="utf-8") as fh:
            first = fh.readline()
            if first.strip() != "---":
                return fm
            for line in fh:
                if line.strip() == "---":
                    break
                if ":" in line:
                    k, _, v = line.partition(":")
                    fm[k.strip()] = v.strip()
    except OSError:
        pass
    return fm


def main():
    entries = []  # (relpath, title, area, sinceVersion)
    for root, _dirs, files in os.walk(PUBLIC):
        for name in sorted(files):
            src = os.path.join(root, name)
            rel = os.path.relpath(src, PUBLIC)
            if name.endswith(".md"):
                fm = front_matter(src)
                if fm.get("audience") != "public":
                    continue
                dest = os.path.join(OUT, rel)
                os.makedirs(os.path.dirname(dest), exist_ok=True)
                shutil.copy2(src, dest)
                entries.append((rel, fm.get("title", name[:-3]),
                                fm.get("area", ""), fm.get("sinceVersion", "")))
            elif "/reference/_generated/" in src.replace(os.sep, "/") and name != ".gitkeep":
                # non-markdown generated assets (e.g. topology.model.json)
                dest = os.path.join(OUT, rel)
                os.makedirs(os.path.dirname(dest), exist_ok=True)
                shutil.copy2(src, dest)

    # nav tree grouped by area, in the canonical sidebar order
    nav = []
    for area in AREAS:
        pages = sorted(
            ({"path": rel, "title": title, "sinceVersion": since}
             for rel, title, a, since in entries if a == area),
            key=lambda p: p["path"],
        )
        if pages:
            nav.append({"area": area, "pages": pages})
    # any pages with an unrecognized/blank area land in an "other" group
    other = sorted(
        ({"path": rel, "title": title, "sinceVersion": since}
         for rel, title, a, since in entries if a not in AREAS),
        key=lambda p: p["path"],
    )
    if other:
        nav.append({"area": "other", "pages": other})

    manifest = {
        "version": VERSION,
        "engineVersion": ENGINE_VERSION,
        "pageCount": len(entries),
        "areas": AREAS,
        "nav": nav,
    }
    with open(os.path.join(OUT, "manifest.json"), "w", encoding="utf-8") as fh:
        json.dump(manifest, fh, indent=2)
        fh.write("\n")
    print(f"INFO: bundled {len(entries)} public docs into {OUT} (manifest.json written)")


if __name__ == "__main__":
    main()
