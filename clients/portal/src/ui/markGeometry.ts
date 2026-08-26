// The MemQL mark's geometry: nine nodes and the nineteen edges between them.
//
// ONE SET OF COORDINATES, because there are now three renderers of it in this
// app alone -- the rail's connection indicator (components/MemqlMark), the
// Constellation (ui/Constellation), and brand/mark.svg, which the VS Code
// extension's activity-bar glyph is validated against by a Go test in
// cmd/memql-lsp. A traced polyhedron re-eyeballed per renderer is three marks.
//
// The trace itself is from memql.io's 9-node graph polyhedron (memql-mark.png)
// on a 24x24 viewBox. Do not adjust a coordinate here to make one renderer
// look better at one size: the shared trace is the point, and a size problem
// is a stroke-width or a radius problem.

export const MARK_VIEWBOX = 24;
export const MARK_NODE_R = 1.53;
export const MARK_STROKE_W = 0.78;

export interface MarkNode {
  readonly cx: number;
  readonly cy: number;
}

// In the order the mark is read: top, then left to right down the solid. The
// INDEX is load-bearing -- the reactive stylesheet targets `.mm-n1`..`.mm-n9`
// by it (styles/index.css), and so does the Constellation's assemble stagger.
export const MARK_NODES: readonly MarkNode[] = [
  { cx: 13.09, cy: 2.52 },
  { cx: 4.3, cy: 5.32 },
  { cx: 9.45, cy: 6.83 },
  { cx: 19.7, cy: 8.76 },
  { cx: 2.52, cy: 14.24 },
  { cx: 13.58, cy: 16.07 },
  { cx: 17.92, cy: 17.85 },
  { cx: 9.4, cy: 20.7 },
  { cx: 21.52, cy: 21.48 },
];

export interface MarkEdge {
  readonly x1: number;
  readonly y1: number;
  readonly x2: number;
  readonly y2: number;
}

// Stated as coordinates rather than as node-index pairs, so this file is a
// literal transcription of brand/mark.svg and a diff against it is a plain
// text comparison.
export const MARK_EDGES: readonly MarkEdge[] = [
  { x1: 13.09, y1: 2.52, x2: 4.3, y2: 5.32 },
  { x1: 13.09, y1: 2.52, x2: 9.45, y2: 6.83 },
  { x1: 13.09, y1: 2.52, x2: 19.7, y2: 8.76 },
  { x1: 13.09, y1: 2.52, x2: 2.52, y2: 14.24 },
  { x1: 4.3, y1: 5.32, x2: 9.45, y2: 6.83 },
  { x1: 4.3, y1: 5.32, x2: 19.7, y2: 8.76 },
  { x1: 4.3, y1: 5.32, x2: 2.52, y2: 14.24 },
  { x1: 9.45, y1: 6.83, x2: 19.7, y2: 8.76 },
  { x1: 9.45, y1: 6.83, x2: 2.52, y2: 14.24 },
  { x1: 9.45, y1: 6.83, x2: 13.58, y2: 16.07 },
  { x1: 19.7, y1: 8.76, x2: 13.58, y2: 16.07 },
  { x1: 19.7, y1: 8.76, x2: 17.92, y2: 17.85 },
  { x1: 19.7, y1: 8.76, x2: 9.4, y2: 20.7 },
  { x1: 2.52, y1: 14.24, x2: 13.58, y2: 16.07 },
  { x1: 2.52, y1: 14.24, x2: 9.4, y2: 20.7 },
  { x1: 13.58, y1: 16.07, x2: 17.92, y2: 17.85 },
  { x1: 13.58, y1: 16.07, x2: 9.4, y2: 20.7 },
  { x1: 17.92, y1: 17.85, x2: 9.4, y2: 20.7 },
  { x1: 17.92, y1: 17.85, x2: 21.52, y2: 21.48 },
];
