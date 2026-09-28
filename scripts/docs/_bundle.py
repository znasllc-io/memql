#!/usr/bin/env python3
"""Assemble the docs bundle tree + manifest.json from docs/public.

Invoked by build-docs-bundle.sh. Reads env: PUBLIC_DIR, OUT, VERSION (already
checked by the caller to equal the VERSION file, which equals the release tag).
Selects markdown files whose front-matter is
`audience: public`, copies them (plus any non-markdown generated assets,
e.g. the architecture model JSON) into OUT preserving the path relative to
docs/public, and writes OUT/manifest.json describing the nav tree by area.

HTML COMMENTS ARE STRIPPED FROM THE BUNDLED MARKDOWN. They are gate markers
read from the source tree (`<!-- corpus: -->`, `<!-- proving: -->`,
`<!-- retired-vocabulary-ok: -->`, `<!-- BEGIN GENERATED -->`), never prose,
and the site renders markdown without raw HTML, so a comment that reached the
bundle printed as literal text in the middle of a page. Fenced code blocks and
code spans are left alone: a comment there is content.
"""
import json
import os
import re
import shutil

PUBLIC = os.environ["PUBLIC_DIR"]
OUT = os.environ["OUT"]
VERSION = os.environ["VERSION"]

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
        # Best-effort by contract (see docstring): a file we cannot read has
        # no readable front-matter, so it returns {} -- which fails the
        # `audience: public` check in main() and keeps the file OUT of the
        # bundle. Failing open here could never publish a non-public doc.
        pass
    return fm


# A fence opens with three or more backticks or tildes. CommonMark allows at
# most three spaces of indent, but a fence inside a list item sits deeper, so
# any indent is accepted: reading a line as a fence can only leave a comment
# in place, never strip content. It closes on a line of the same character, at
# least as long, with nothing after it but whitespace.
FENCE = re.compile(r"^[ \t]*(`{3,}|~{3,})")
BACKTICKS = re.compile(r"`+")


def _strip_line(line, in_comment):
    """Remove the HTML comments on one line outside its code spans.

    Returns (text, removed, in_comment): the line without its comments, whether
    anything was removed, and whether a comment is still open at its end.
    """
    out = []
    removed = False
    i = 0
    while i < len(line):
        if in_comment:
            end = line.find("-->", i)
            removed = True
            if end < 0:
                return "".join(out), removed, True
            i = end + 3
            in_comment = False
            continue
        opener = line.find("<!--", i)
        tick = BACKTICKS.search(line, i)
        if tick is not None and (opener < 0 or tick.start() < opener):
            # A code span runs to the next backtick run of the same length;
            # an unmatched run is literal backticks and is copied as such.
            run = tick.group(0)
            close = re.compile(r"(?<!`)" + run + r"(?!`)").search(line, tick.end())
            stop = close.end() if close is not None else tick.end()
            out.append(line[i:stop])
            i = stop
            continue
        if opener < 0:
            out.append(line[i:])
            break
        out.append(line[i:opener])
        i = opener + 4
        in_comment = True
    return "".join(out), removed, in_comment


def strip_html_comments(text):
    """Return markdown text with its HTML comments removed.

    A comment on a line of its own leaves one blank line, which keeps the block
    boundary it made (CommonMark reads it as an HTML block). A trailing comment
    leaves its line's text, with the whitespace before the comment trimmed so
    it cannot become a hard line break.
    """
    out = []
    fence = None
    in_comment = False
    for line in text.splitlines(keepends=True):
        body = line.rstrip("\r\n")
        newline = line[len(body):]
        if not in_comment:
            m = FENCE.match(body)
            if fence is not None:
                if m and m.group(1)[0] == fence[0] and len(m.group(1)) >= len(fence) \
                        and body[m.end():].strip() == "":
                    fence = None
                out.append(line)
                continue
            if m is not None:
                fence = m.group(1)
                out.append(line)
                continue
        continued = in_comment
        text_out, removed, in_comment = _strip_line(body, in_comment)
        if not removed:
            out.append(line)
            continue
        if in_comment or body.rstrip().endswith("-->"):
            text_out = text_out.rstrip(" \t")
        if text_out.strip() == "":
            if not continued:
                out.append(newline or "\n")
            continue
        out.append(text_out + newline)
    return "".join(out)


def copy_markdown(src, dest):
    """Copy one markdown file with its HTML comments stripped. newline="" keeps
    the line endings as written, so a file with no comments is copied byte for
    byte."""
    with open(src, encoding="utf-8", newline="") as fh:
        text = fh.read()
    with open(dest, "w", encoding="utf-8", newline="") as fh:
        fh.write(strip_html_comments(text))
    shutil.copystat(src, dest)


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
                copy_markdown(src, dest)
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
