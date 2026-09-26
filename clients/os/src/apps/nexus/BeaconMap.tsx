import { ContentSkeleton } from "../../kit/ContentSkeleton";
import { useCallback, useMemo } from "react";
import type { LiveState } from "@znasllc-io/memql-sdk-core/client";

import { Caption, formatMoment } from "../../kit";
import { transformOf } from "../../kit/viewport";
import { usePanZoom } from "../../kit/usePanZoom";
import { layout, type LayoutNode, type LayoutResult } from "../../nexus/scene/layout";
import { goalProgress } from "../../nexus/scene/scene";
import type { GoalWorld } from "../../nexus/scene/world";

// THE BEACON MAP: a goal, drawn as the place the work arrives at.
//
// ===========================================================================
// PLAIN SVG. NO WebGL, AND THAT IS A RULE RATHER THAN A PREFERENCE
// ===========================================================================
// The portal's Nexus was the platform's one 3D surface and it was deleted with
// the portal's pages. MemQL OS carries NO WebGL by owner requirement (epic
// memql#4785): three.js is the largest dependency the platform had, and a
// window in a desktop shell cannot afford it to draw a road with stops on it.
// `test/nexus/map.test.tsx` scans the module graph AND the package manifest,
// because a static import is only one of the two ways one gets in, and both
// halves carry a reachable positive so an empty offender list is evidence
// about the tree rather than a statement about the regex.
//
// ===========================================================================
// EVERY POSITION COMES FROM THE PURE LIBRARY
// ===========================================================================
// Nothing here decides where anything goes. `src/nexus/scene/layout.ts` is a
// function from rows to positioned nodes, fixture-tested with no DOM and no
// GPU, and this file turns its answer into shapes. That is what lets the
// layout be asserted at all -- a rule about where a node sits, asserted
// through a rendered SVG, is asserted through React, the DOM and a transform
// string.
//
// ===========================================================================
// THE ROAD'S WEIGHT IS THE RAIL'S INK
// ===========================================================================
// The step rail beneath this map already says the product's claim in one
// glance: hollow node on a hairline where the machine did not have to think,
// filled node on thick ink where it did. The road says the SAME sentence with
// the SAME marks, so a person learns the language once. Scanning a long run,
// the eye finds where the thinking happened before a word has been read.
//
// ===========================================================================
// COLOUR IS NEVER THE ONLY CARRIER
// ===========================================================================
// Status is a word in every accessible name and a SHAPE on the canvas -- a
// failed step carries a cross, a waiting one a pause bar, a finished one a
// tick -- so the picture survives greyscale and reads the same under every
// theme pack. Every colour is a token, so a theme restyles the map with no
// code.

/** Pixels per scene unit. The layout's units are abstract; this is the dial. */
const UNIT = 22;
/** Room around the extent so nothing sits against the edge of the canvas. */
const PAD = 40;

const STEP_R = 7;
const BEACON_R = 22;
const THIN = 1.25;
const THICK = 4;

/* The horizontal half-extent of every shape on the canvas.
 *
 * NAMED RATHER THAN INLINE because two things now need them: `shapeFor`, which
 * draws the glyph, and `standoff`, which stops the road short of it. Written
 * twice they drift, and the drift is invisible -- a rect that grew by two
 * pixels just starts touching the line again. */
const TEMPLATE_HALF = 9;
const FOLD_HALF = 30;
const CLUSTER_R = STEP_R + 5;
const BINDING_HALF = 5;
const APPROVAL_HALF = 7;

/* The clear space between a glyph's edge and the connector arriving at it.
 *
 * WHY THE ROAD IS TRIMMED AT ALL. Every shape on the road is `fill: none` -- an
 * open ring is the map saying "a position", not "a thing" -- so a line drawn
 * centre to centre is VISIBLE INSIDE the glyph. Across the beacon, 44px wide
 * and the one element this surface is allowed to be loud with, it reads as a
 * line crossing the goal out rather than a road arriving at it.
 *
 * FILLING THE SHAPES WOULD ALSO HAVE HIDDEN THE LINE and was the wrong fix
 * twice over: it only works while the canvas behind is flat, and it spends the
 * one thing the open shapes are there to say.
 *
 * Sized against the widest stroke it has to clear (the beacon ring's 3px, so
 * 1.5px past the nominal radius) plus the road's own round cap (THICK / 2),
 * leaving a gap that still reads as deliberate at the default zoom. */
const ARRIVAL_GAP = 6;

/** How far back from a node's centre a connector must stop. */
function standoff(kind: LayoutNode["kind"]): number {
  switch (kind) {
    case "goal":
      return BEACON_R + ARRIVAL_GAP;
    case "fold":
      return FOLD_HALF + ARRIVAL_GAP;
    case "cluster":
      return CLUSTER_R + ARRIVAL_GAP;
    case "template":
      return TEMPLATE_HALF + ARRIVAL_GAP;
    case "binding":
      return BINDING_HALF + ARRIVAL_GAP;
    case "approval":
      return APPROVAL_HALF + ARRIVAL_GAP;
    default:
      return STEP_R + ARRIVAL_GAP;
  }
}

/** Road points and node positions are the same floats; compare them as one. */
function roadKey(x: number): number {
  return Math.round(x * 100);
}

/**
 * A segment pulled back at both ends so it ARRIVES at its nodes.
 *
 * Returns null when the two standoffs would meet or cross: at that point the
 * honest drawing is no line at all, not a backwards one. Nothing in the current
 * layout comes close -- the shortest stretch is 110px against a 28px trim -- but
 * a zoom, a fold beside a beacon, or a future spacing change all reach it, and
 * an inverted segment is the kind of artefact nobody reads as a bug.
 */
function arrive(
  ax: number,
  ay: number,
  bx: number,
  by: number,
  backA: number,
  backB: number,
): { x1: number; y1: number; x2: number; y2: number } | null {
  const dx = bx - ax;
  const dy = by - ay;
  const len = Math.hypot(dx, dy);
  if (len === 0 || backA + backB >= len) return null;
  const ux = dx / len;
  const uy = dy / len;
  return { x1: ax + ux * backA, y1: ay + uy * backA, x2: bx - ux * backB, y2: by - uy * backB };
}

export interface BeaconMapProps {
  world: GoalWorld;
  state: LiveState;
  /** The step key the rail has open, so the two surfaces share one selection. */
  selectedStepKey: string;
  onSelectStep: (stepKey: string) => void;
  onOpenApproval: (approvalId: string) => void;
  expandedColumns: ReadonlySet<number>;
  expandedFolds: ReadonlySet<number>;
  onToggleColumn: (depth: number) => void;
  onToggleFold: (depth: number) => void;
  /** Set while the goal view is rewound, for the "as it stood" caption. */
  at: string;
}

export function BeaconMap({
  world,
  state,
  selectedStepKey,
  onSelectStep,
  onOpenApproval,
  expandedColumns,
  expandedFolds,
  onToggleColumn,
  onToggleFold,
  at,
}: BeaconMapProps) {
  const model: LayoutResult = useMemo(
    () => layout(world, { expandedColumns, expandedFolds }),
    [world, expandedColumns, expandedFolds],
  );
  const progress = useMemo(() => goalProgress(world), [world]);

  const width = (model.bounds.maxX - model.bounds.minX) * UNIT + PAD * 2;
  const height = (model.bounds.maxY - model.bounds.minY) * UNIT + PAD * 2;

  const { view, frameRef, handlers, steering } = usePanZoom({
    width,
    height,
    ready: model.nodes.size > 1,
    // FILLS THE CANVAS RATHER THAN SITTING IN THE MIDDLE OF IT. A short run is
    // a thin road with four stops, and at scale 1 it drew as a small band with
    // empty canvas all round -- which reads as a picture that failed to load.
    // Capped, so a one-step run does not become a diagram of one circle.
    maxFit: 1.8,
  });

  // A DEGRADED FEED MUST NOT READ AS A LIVE RUN. A map is a picture of now;
  // when the subscription is behind, the picture is of some earlier now, and
  // the only honest thing to do is say so and dim it.
  const behind = state === "degraded" || state === "disconnected";

  const px = useCallback((x: number) => (x - model.bounds.minX) * UNIT + PAD, [model.bounds.minX]);
  const py = useCallback((y: number) => (y - model.bounds.minY) * UNIT + PAD, [model.bounds.minY]);

  /* What stands at each point on the road, so a stretch can stop short of it.
   *
   * Keyed on the ROAD-LANE node only: a column can hold several nodes at
   * different lanes, and the one the road runs through is the one the road has
   * to clear. */
  const standoffAt = useMemo(() => {
    const byX = new Map<number, number>();
    for (const node of model.nodes.values()) {
      if (node.lane !== "road") continue;
      const key = roadKey(node.x);
      byX.set(key, Math.max(byX.get(key) ?? 0, standoff(node.kind)));
    }
    return byX;
  }, [model.nodes]);

  const activate = useCallback(
    (node: LayoutNode) => {
      if (steering()) return;
      if (node.kind === "cluster") return onToggleColumn(node.depth);
      if (node.kind === "fold") return onToggleFold(node.depth);
      if (node.kind === "approval") return onOpenApproval(node.rowId);
      // AN ARTIFACT IS A LABEL, NOT A DOOR, and that is a finding rather than
      // a preference. Opening one belongs in Files, and the shell's handoff
      // (`openApp("files", ...)`) would reach an intent handler that reads
      // `place` and `folderId` and CONSUMES anything else -- so wiring the
      // click would open the Files window on whatever it was last showing and
      // select nothing, which is a click that looks like it worked. Making it
      // a door wants Files to accept an artifact id, which is a change to
      // Files.
      if (node.stepKey !== "") return onSelectStep(node.stepKey);
    },
    [onOpenApproval, onSelectStep, onToggleColumn, onToggleFold, steering],
  );

  if (world.goal === null) {
    return (
      <div className="os-nexus-map" data-empty>
        {state === "seeding" ? <ContentSkeleton kind="map" label="Loading from the cluster" /> : <Caption>{"Nothing to draw yet."}</Caption>}
      </div>
    );
  }

  const nodes = [...model.nodes.values()];
  const road = model.road;

  return (
    <div className="os-nexus-map" ref={frameRef} data-behind={behind || undefined}>
      <svg
        className="os-nexus-map-canvas"
        role="application"
        aria-label={mapDescription(world, progress, at)}
        tabIndex={0}
        {...handlers}
      >
        <g data-os-map-view transform={transformOf(view)}>
          {/* THE ROAD, drawn segment by segment so each carries its own
              weight and its own brightness. One path with a dash pattern
              could not: the weight changes along it, and a stretch that has
              landed is a fact about that stretch rather than a length. */}
          {road.slice(1).map((point, index) => {
            const from = road[index]!;
            // A road point with no node standing on it is a bend, not an
            // arrival, and the polyline has to stay continuous through it --
            // hence 0 rather than a bare gap.
            const seg = arrive(
              px(from.x),
              py(from.y),
              px(point.x),
              py(point.y),
              standoffAt.get(roadKey(from.x)) ?? 0,
              standoffAt.get(roadKey(point.x)) ?? 0,
            );
            if (seg === null) return null;
            return (
              <line
                key={`road-${index}`}
                className="os-nexus-road"
                data-thought={point.thought || undefined}
                data-done={point.done || undefined}
                x1={seg.x1}
                y1={seg.y1}
                x2={seg.x2}
                y2={seg.y2}
                strokeWidth={point.thought ? THICK : THIN}
              />
            );
          })}

          {/* Real dependency edges. Trimmed for the same reason the road is:
              they run into the same unfilled glyphs, and fixing only the road
              would leave the identical artefact a few pixels away. Here both
              endpoints are nodes, so the standoff is read straight off them. */}
          {model.edges.map((edge, index) => {
            const a = model.nodes.get(edge.from);
            const b = model.nodes.get(edge.to);
            if (a === undefined || b === undefined) return null;
            const seg = arrive(
              px(a.x),
              py(a.y),
              px(b.x),
              py(b.y),
              standoff(a.kind),
              standoff(b.kind),
            );
            if (seg === null) return null;
            return (
              <line
                key={`edge-${index}`}
                className="os-nexus-edge"
                data-kind={edge.kind}
                x1={seg.x1}
                y1={seg.y1}
                x2={seg.x2}
                y2={seg.y2}
              />
            );
          })}

          {nodes.map((node) =>
            node.kind === "goal" ? (
              <Beacon
                key={node.id}
                node={node}
                cx={px(node.x)}
                cy={py(node.y)}
                fraction={progress.fraction}
                lit={progress.lit}
                compiling={progress.compiling}
                completed={progress.completed}
                total={progress.total}
              />
            ) : (
              <MapNode
                key={node.id}
                node={node}
                cx={px(node.x)}
                cy={py(node.y)}
                selected={node.stepKey !== "" && node.stepKey === selectedStepKey}
                onActivate={() => activate(node)}
              />
            ),
          )}
        </g>
      </svg>

      {/* The caption is the map's own sentence about itself, and it is the
          only place a rewound map says so -- a picture of an earlier moment
          that does not say which moment is a picture that lies. */}
      <p className="os-nexus-map-caption os-caption">
        {behind
          ? "Not connected to the cluster, so this is the last picture it sent."
          : at !== ""
            ? // THE PERSON'S FORMAT, not the wire's. The scrubber a few lines
              // below prints the same instant through `formatMoment`, and two
              // renderings of one moment on one screen read as two moments.
              `As it stood at ${formatMoment(at)}.`
            : legend(progress)}
      </p>
    </div>
  );
}

/**
 * The beacon: the goal, filling.
 *
 * TWO CIRCLES AND AN ARC. The outer ring is the whole of the work and the arc
 * is the part that has landed, drawn from twelve o'clock clockwise because
 * that is the direction every progress ring anybody has read goes.
 *
 * A COMPILING RUN DRAWS AN EMPTY RING AND SAYS SO, rather than an arc at zero.
 * There is no denominator yet -- the steps do not exist -- and "0%" reads as a
 * run that has failed to do anything, which is the opposite of the truth.
 */
function Beacon({
  node,
  cx,
  cy,
  fraction,
  lit,
  compiling,
  completed,
  total,
}: {
  node: LayoutNode;
  cx: number;
  cy: number;
  fraction: number;
  lit: boolean;
  compiling: boolean;
  completed: number;
  total: number;
}) {
  const circumference = 2 * Math.PI * BEACON_R;
  const filled = circumference * Math.max(0, Math.min(1, fraction));
  const spoken = compiling
    ? `Goal: ${node.label}. Working out how to do it.`
    : total === 0
      ? `Goal: ${node.label}. No steps yet.`
      : `Goal: ${node.label}. ${completed} of ${total} steps done${lit ? ", reached" : ""}.`;

  return (
    <g className="os-nexus-beacon" data-lit={lit || undefined} data-compiling={compiling || undefined}>
      <title>{spoken}</title>
      <circle className="os-nexus-beacon-ring" cx={cx} cy={cy} r={BEACON_R} />
      {compiling ? null : (
        <circle
          className="os-nexus-beacon-fill"
          cx={cx}
          cy={cy}
          r={BEACON_R}
          strokeDasharray={`${filled} ${circumference - filled}`}
          transform={`rotate(-90 ${cx} ${cy})`}
        />
      )}
      <circle className="os-nexus-beacon-core" cx={cx} cy={cy} r={BEACON_R / 3} />
      {/* NO STATEMENT UNDER THE RING. The page's own title is that sentence,
          in full and untruncated, four lines above -- and a second, elided copy
          of it under the beacon is the same thing said twice, worse. The
          beacon's <title> above still carries it for a reader who cannot see
          which page they are on. */}
      <text className="os-nexus-beacon-count" x={cx} y={cy + BEACON_R + 20} textAnchor="middle">
        {compiling ? "working it out" : total === 0 ? "" : `${completed} of ${total}`}
      </text>
    </g>
  );
}

function MapNode({
  node,
  cx,
  cy,
  selected,
  onActivate,
}: {
  node: LayoutNode;
  cx: number;
  cy: number;
  selected: boolean;
  onActivate: () => void;
}) {
  const spoken = describe(node);
  return (
    <g
      className="os-nexus-node"
      data-kind={node.kind}
      data-status={node.status}
      data-lane={node.lane}
      data-selected={selected || undefined}
      role="button"
      tabIndex={0}
      aria-label={spoken}
      onClick={onActivate}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          e.stopPropagation();
          onActivate();
        }
      }}
    >
      <title>{spoken}</title>
      {shapeFor(node, cx, cy)}
      {node.kind === "fold" ? null : (
        <text className="os-nexus-node-label" x={cx} y={cy - STEP_R - 7} textAnchor="middle">
          {truncate(node.label, node.kind === "template" ? 26 : 18)}
        </text>
      )}
    </g>
  );
}

/**
 * The shape a node takes.
 *
 * The shape carries the KIND and the fill carries the status, so neither
 * depends on colour alone. `you` is an open ring because it is a position
 * rather than a thing; a fold is a capsule because it stands for a stretch.
 */
function shapeFor(node: LayoutNode, cx: number, cy: number) {
  switch (node.kind) {
    case "you":
      return <circle className="os-nexus-you" cx={cx} cy={cy} r={STEP_R} />;
    case "template":
      return (
        <rect
          className="os-nexus-template"
          x={cx - TEMPLATE_HALF}
          y={cy - TEMPLATE_HALF}
          width={TEMPLATE_HALF * 2}
          height={TEMPLATE_HALF * 2}
          rx={5}
        />
      );
    case "fold":
      // THE COUNT SITS INSIDE THE PILL. Drawn outside like every other node's
      // label it left an empty capsule on the road with a number floating
      // above it, which reads as a gap rather than as a stretch.
      return (
        <>
          <rect
            className="os-nexus-fold"
            x={cx - FOLD_HALF}
            y={cy - 10}
            width={FOLD_HALF * 2}
            height={20}
            rx={10}
          />
          <text className="os-nexus-fold-count" x={cx} y={cy + 4} textAnchor="middle">
            {node.label}
          </text>
        </>
      );
    case "cluster":
      return <circle className="os-nexus-cluster" cx={cx} cy={cy} r={CLUSTER_R} />;
    case "binding":
      return (
        <rect
          className="os-nexus-binding"
          x={cx - BINDING_HALF}
          y={cy - BINDING_HALF}
          width={BINDING_HALF * 2}
          height={BINDING_HALF * 2}
          rx={2}
        />
      );
    case "approval":
      return (
        <path
          className="os-nexus-approval"
          d={`M ${cx} ${cy - APPROVAL_HALF} L ${cx + APPROVAL_HALF} ${cy} L ${cx} ${cy + APPROVAL_HALF} L ${cx - APPROVAL_HALF} ${cy} Z`}
        />
      );
    // A PAGE WITH ITS CORNER TURNED. Every other glyph here is a primitive --
    // circle, square, diamond -- because every other node stands for a moment
    // in the machine. This one stands for a thing a person can open and read,
    // and it is the only node on the map that leaves the run behind, so it is
    // the one place a literal shape earns its keep.
    case "artifact":
      return (
        <path
          className="os-nexus-artifact"
          data-archived={node.status === "archived" ? "" : undefined}
          d={`M ${cx - 5} ${cy - 7} L ${cx + 2} ${cy - 7} L ${cx + 5} ${cy - 4} L ${cx + 5} ${cy + 7} L ${cx - 5} ${cy + 7} Z M ${cx + 2} ${cy - 7} L ${cx + 2} ${cy - 4} L ${cx + 5} ${cy - 4}`}
        />
      );
    default:
      return <circle className="os-nexus-step" cx={cx} cy={cy} r={STEP_R} />;
  }
}

function describe(node: LayoutNode): string {
  switch (node.kind) {
    case "you":
      return "Start, where the goal was set";
    case "template":
      return `Automation ${node.label}, ${node.status}`;
    case "fold": {
      const thought = node.thoughtful ?? 0;
      const cost =
        thought === 0
          ? ""
          : ` ${thought} of them ${thought === 1 ? "is a step" : "are steps"} the machine had to think about.`;
      return `${node.standsFor} finished steps, folded.${cost} Open to see them.`;
    }
    case "cluster":
      return `${node.standsFor} steps running together. Open to see them.`;
    case "binding":
      return `Ran on ${node.label}`;
    case "approval":
      return node.status === "waiting"
        ? `Waiting on you: ${node.label}`
        : `${node.label}, ${node.status}`;
    case "artifact":
      // "Produced" rather than "created": the map is about what the run did,
      // and a person reading this wants to know the run made it rather than
      // when the row was written.
      return node.status === "archived"
        ? `${node.label}, produced here and since archived`
        : `Produced: ${node.label}`;
    default:
      return `Step ${node.label}, ${node.status}`;
  }
}

function legend(progress: { completed: number; total: number; compiling: boolean }): string {
  if (progress.compiling) return "Working out how to do this. The map fills in as steps appear.";
  if (progress.total === 0) return "No steps yet.";
  return "Thick road is where the machine had to think. Drag to pan, scroll to zoom.";
}

function mapDescription(
  world: GoalWorld,
  progress: { completed: number; total: number; compiling: boolean },
  at: string,
): string {
  const statement = world.goal?.statement ?? "a goal";
  // THE PERSON'S FORMAT HERE TOO. This string is read aloud, and an RFC3339
  // stamp spoken out is a worse answer than no answer.
  const when = at === "" ? "" : `, as it stood at ${formatMoment(at)}`;
  if (progress.compiling) return `Map of ${statement}: working out how to do it${when}`;
  return `Map of ${statement}: ${progress.completed} of ${progress.total} steps done${when}`;
}

function truncate(text: string, max: number): string {
  const trimmed = text.trim();
  if (trimmed.length <= max) return trimmed;
  // A hard cut at a character budget rather than a measured width: the layout
  // is pure and takes no DOM reads, and a label that measured itself would
  // make the map lay out differently in a test and on a screen.
  return `${trimmed.slice(0, max - 1)}…`;
}
