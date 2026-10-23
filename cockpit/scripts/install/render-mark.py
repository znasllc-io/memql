#!/usr/bin/env python3
"""Render the canonical SVG into terminal cells; no runtime image dependency.

The cells are braille (2 × 4 square dots in a half-width terminal cell), so the
mark keeps the SVG's proportions. Run --check in CI or --write after artwork
changes. This is generated artwork, not a separately drawn terminal logo.
"""
import argparse
import math
from pathlib import Path
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[2]
SOURCE = ROOT / "native/macos/mark.svg"
TARGET = ROOT / "scripts/install/lib.sh"
START = "# BEGIN GENERATED MEMQL MARK\n"
END = "# END GENERATED MEMQL MARK"


def render():
    root = ET.parse(SOURCE).getroot()
    ns = {"s": "http://www.w3.org/2000/svg"}
    edges = root.find("s:g[@class='mm-edges']", ns)
    stroke_radius = float(edges.attrib["stroke-width"]) / 2
    extent = float(root.attrib["viewBox"].split()[2])
    lines = [[float(e.attrib[k]) for k in ("x1", "y1", "x2", "y2")] for e in edges.findall("s:line", ns)]
    nodes = [[float(e.attrib[k]) for k in ("cx", "cy", "r")] for e in root.findall(".//s:circle", ns)]
    assert len(nodes) == 9 and len(lines) == 19, "Unexpected canonical mark geometry"
    size = 40
    pixels = set()
    for y in range(size):
        for x in range(size):
            px, py = (x + .5) * extent / size, (y + .5) * extent / size
            inside = any(math.hypot(px - cx, py - cy) <= r for cx, cy, r in nodes)
            for x1, y1, x2, y2 in lines:
                dx, dy = x2 - x1, y2 - y1
                t = max(0, min(1, ((px - x1) * dx + (py - y1) * dy) / (dx * dx + dy * dy)))
                inside |= math.hypot(px - x1 - t * dx, py - y1 - t * dy) <= stroke_radius
            if inside:
                pixels.add((x, y))
    bits = ((0, 0, 0), (0, 1, 1), (0, 2, 2), (1, 0, 3), (1, 1, 4), (1, 2, 5), (0, 3, 6), (1, 3, 7))
    return "\n".join("".join(chr(0x2800 + sum(1 << b for dx, dy, b in bits if (x + dx, y + dy) in pixels)) for x in range(0, size, 2)) for y in range(0, size, 4))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--write", action="store_true")
    mode.add_argument("--check", action="store_true")
    args = parser.parse_args()
    block = START + "    cat <<'MEMQL_MARK'\n" + render() + "\nMEMQL_MARK\n" + END
    text = TARGET.read_text()
    start, end = text.index(START), text.index(END) + len(END)
    updated = text[:start] + block + text[end:]
    if args.write:
        TARGET.write_text(updated)
    elif args.check:
        if updated != text:
            parser.error("Terminal mark is stale; run scripts/install/render-mark.py --write")
    else:
        print(render())


if __name__ == "__main__":
    main()
