import type { ReactNode } from "react";

import {
  MARK_EDGES,
  MARK_NODES,
  MARK_NODE_R,
  MARK_STROKE_W,
  MARK_VIEWBOX,
} from "../ui/markGeometry";

// The MemQL mark: the 9-node graph polyhedron from memql.io.
//
// The GEOMETRY moved to ui/markGeometry.ts in memql#4651, when the
// Constellation became a second renderer of it -- a traced polyhedron
// re-eyeballed per renderer is two marks. The trace itself is unchanged and
// is still shared verbatim with the VS Code extension's activity-bar glyph
// (editors/vscode/icons/memql-activity.svg), which a Go test in cmd/memql-lsp
// validates against the source artwork.
//
// Rendered inline (not an <img>) and in currentColor, for two reasons:
//   1. it takes the surrounding text colour, so the same component is the
//      accent-coloured brand mark, a muted empty-state glyph, or the dimmed
//      disconnected indicator with no prop plumbing;
//   2. the reactive layer (memql#4180) animates INDIVIDUAL nodes -- each
//      circle carries a stable class (mm-node mm-n1..mm-n9) and the edge
//      group mm-edges, which CSS keyframes target. An <img> is a bitmap to
//      CSS; an inline SVG is a DOM.
//
// The title prop makes the mark meaningful to assistive tech where it IS the
// information (the rail's connection indicator); without it the mark is
// decoration next to text and hides itself.

export function MemqlMark({
  size = 24,
  className,
  title,
}: {
  size?: number;
  className?: string;
  title?: string;
}): ReactNode {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox={`0 0 ${MARK_VIEWBOX} ${MARK_VIEWBOX}`}
      width={size}
      height={size}
      {...(className === undefined ? {} : { className })}
      {...(title === undefined
        ? { "aria-hidden": true }
        : { role: "img", "aria-label": title })}
    >
      {title === undefined ? null : <title>{title}</title>}
      <g
        className="mm-edges"
        fill="none"
        stroke="currentColor"
        strokeWidth={MARK_STROKE_W}
        strokeLinecap="round"
      >
        {MARK_EDGES.map((edge) => (
          <line
            key={`${edge.x1},${edge.y1}-${edge.x2},${edge.y2}`}
            x1={edge.x1}
            y1={edge.y1}
            x2={edge.x2}
            y2={edge.y2}
          />
        ))}
      </g>
      <g fill="currentColor">
        {MARK_NODES.map((node, index) => (
          <circle
            key={`${node.cx},${node.cy}`}
            className={`mm-node mm-n${index + 1}`}
            cx={node.cx}
            cy={node.cy}
            r={MARK_NODE_R}
          />
        ))}
      </g>
    </svg>
  );
}
