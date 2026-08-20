import type { ReactNode } from "react";

// The MemQL mark: the 9-node graph polyhedron from memql.io, traced from
// memql-mark.png. The geometry is shared verbatim with the VS Code
// extension's activity-bar glyph (editors/vscode/icons/memql-activity.svg),
// which a Go test in cmd/memql-lsp validates against the source artwork --
// so this component inherits a checked trace rather than a re-eyeballed one.
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
      viewBox="0 0 24 24"
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
        strokeWidth="0.78"
        strokeLinecap="round"
      >
        <line x1="13.09" y1="2.52" x2="4.30" y2="5.32" />
        <line x1="13.09" y1="2.52" x2="9.45" y2="6.83" />
        <line x1="13.09" y1="2.52" x2="19.70" y2="8.76" />
        <line x1="13.09" y1="2.52" x2="2.52" y2="14.24" />
        <line x1="4.30" y1="5.32" x2="9.45" y2="6.83" />
        <line x1="4.30" y1="5.32" x2="19.70" y2="8.76" />
        <line x1="4.30" y1="5.32" x2="2.52" y2="14.24" />
        <line x1="9.45" y1="6.83" x2="19.70" y2="8.76" />
        <line x1="9.45" y1="6.83" x2="2.52" y2="14.24" />
        <line x1="9.45" y1="6.83" x2="13.58" y2="16.07" />
        <line x1="19.70" y1="8.76" x2="13.58" y2="16.07" />
        <line x1="19.70" y1="8.76" x2="17.92" y2="17.85" />
        <line x1="19.70" y1="8.76" x2="9.40" y2="20.70" />
        <line x1="2.52" y1="14.24" x2="13.58" y2="16.07" />
        <line x1="2.52" y1="14.24" x2="9.40" y2="20.70" />
        <line x1="13.58" y1="16.07" x2="17.92" y2="17.85" />
        <line x1="13.58" y1="16.07" x2="9.40" y2="20.70" />
        <line x1="17.92" y1="17.85" x2="9.40" y2="20.70" />
        <line x1="17.92" y1="17.85" x2="21.52" y2="21.48" />
      </g>
      <g fill="currentColor">
        <circle className="mm-node mm-n1" cx="13.09" cy="2.52" r="1.53" />
        <circle className="mm-node mm-n2" cx="4.30" cy="5.32" r="1.53" />
        <circle className="mm-node mm-n3" cx="9.45" cy="6.83" r="1.53" />
        <circle className="mm-node mm-n4" cx="19.70" cy="8.76" r="1.53" />
        <circle className="mm-node mm-n5" cx="2.52" cy="14.24" r="1.53" />
        <circle className="mm-node mm-n6" cx="13.58" cy="16.07" r="1.53" />
        <circle className="mm-node mm-n7" cx="17.92" cy="17.85" r="1.53" />
        <circle className="mm-node mm-n8" cx="9.40" cy="20.70" r="1.53" />
        <circle className="mm-node mm-n9" cx="21.52" cy="21.48" r="1.53" />
      </g>
    </svg>
  );
}
