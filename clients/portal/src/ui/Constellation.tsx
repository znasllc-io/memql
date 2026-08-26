import type { ReactNode } from "react";

import {
  MARK_EDGES,
  MARK_NODES,
  MARK_VIEWBOX,
} from "./markGeometry";
import { useReducedMotion } from "./motion";

// The Constellation: the product's ONE expressive element (decision D4).
//
// ===========================================================================
// WHY ONE, AND WHY THIS ONE
// ===========================================================================
// The portal was "visually anonymous" -- correct, careful, and indistinguish-
// able from any other admin console. The fix is not more decoration
// everywhere; it is ONE motif, used in few enough places that it reads as a
// signature rather than as wallpaper. This is the mark the product already
// owns, drawn large enough to be a picture instead of an icon, assembling
// itself once.
//
// IT APPEARS IN EXACTLY FOUR PLACES and the list is closed:
//
//   * EmptyState, opt-in, for a FIRST-RUN empty ("No machines yet") -- not
//     for a filter that matched nothing, which is a different sentence.
//   * the PageGuide dialog's video placeholder poster.
//   * SignInPage.
//   * NotFoundPage.
//
// A fifth use is a design change, not a component change. The moment it is
// on six screens it is a texture and the product is anonymous again in a new
// way.
//
// ===========================================================================
// THE ANIMATION RUNS ONCE, AND THEN THE SCREEN IS STILL
// ===========================================================================
// Nodes scale in staggered, the edges draw once, and that is the end of it --
// no loop, no breathing. Breathing is reserved in this product for things
// that are genuinely live (the connection mark, online dots, streaming
// states), and spending it on decoration is what would make those states stop
// meaning anything.
//
// The assemble is CSS, keyed off `data-animate` (src/styles/index.css), for
// the same reason the reactive mark is: one media query flattens it, and the
// RESTING state of every element is already the final frame. So `animation:
// none` -- which is what reduced motion applies -- is not a special case that
// needs its own drawing code; it is the drawing with the movement removed.
//
// REDUCED MOTION IS READ TWICE, deliberately. The stylesheet has the media
// query, and this component ALSO reads the preference and stops emitting the
// animate flag. The stylesheet is what actually stops the movement; the flag
// is what a unit test can see, because jsdom applies no stylesheet at all and
// a component that only "obeyed" through CSS would have no assertable
// behaviour (see [[jsdom-cannot-see-webgl-or-css-tokens]] -- the portal has
// already shipped 518 green tests over a scene that drew nothing).

export type ConstellationSize = "sm" | "md" | "lg";

// Per size: the rendered box, and the two WEIGHTS that are not part of the
// shared trace.
//
// The node positions are the mark's, verbatim and never adjusted. The radius
// and the stroke are not positions -- they are how heavy the drawing is at
// this size, and the mark's own 6.4%-of-width dot is a dot at 24px and a blob
// at 240. Scaling them down as the box grows is what turns an icon into a
// constellation.
const SIZES: Record<ConstellationSize, { px: number; r: number; stroke: number }> = {
  sm: { px: 96, r: 1.35, stroke: 0.62 },
  md: { px: 160, r: 1.15, stroke: 0.45 },
  lg: { px: 240, r: 1.0, stroke: 0.34 },
};

export function Constellation({
  size = "md",
  animate = true,
  className,
}: {
  size?: ConstellationSize;
  // Once on mount, by default. Passed false where the element is already
  // on screen when the page arrives and a second assemble would read as a
  // glitch -- a dialog re-opening on the same page, for instance.
  animate?: boolean;
  className?: string;
}): ReactNode {
  const reduced = useReducedMotion();
  const { px, r, stroke } = SIZES[size];
  const running = animate && !reduced;

  return (
    <span
      className={"constellation inline-block" + (className === undefined ? "" : ` ${className}`)}
      data-animate={running ? "true" : "false"}
      data-size={size}
    >
      <svg
        xmlns="http://www.w3.org/2000/svg"
        viewBox={`0 0 ${MARK_VIEWBOX} ${MARK_VIEWBOX}`}
        width={px}
        height={px}
        // Decoration in all four of its homes: the heading or the sentence
        // beside it carries the meaning, and a mark that also announced
        // itself would read the page's subject twice.
        aria-hidden="true"
        className="max-w-full"
      >
        <g
          className="cn-edges"
          fill="none"
          stroke="currentColor"
          strokeWidth={stroke}
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
              className={`cn-node cn-n${index + 1}`}
              cx={node.cx}
              cy={node.cy}
              r={r}
            />
          ))}
        </g>
      </svg>
    </span>
  );
}
