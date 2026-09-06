import { useCallback, useMemo } from "react";
import type { LiveState } from "@znasllc-io/memql-sdk-core/client";

import { Caption } from "../../kit";
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
  });

  // A DEGRADED FEED MUST NOT READ AS A LIVE RUN. A map is a picture of now;
  // when the subscription is behind, the picture is of some earlier now, and
  // the only honest thing to do is say so and dim it.
  const behind = state === "degraded" || state === "disconnected";

  const px = useCallback((x: number) => (x - model.bounds.minX) * UNIT + PAD, [model.bounds.minX]);
  const py = useCallback((y: number) => (y - model.bounds.minY) * UNIT + PAD, [model.bounds.minY]);

  const activate = useCallback(
    (node: LayoutNode) => {
      if (steering()) return;
      if (node.kind === "cluster") return onToggleColumn(node.depth);
      if (node.kind === "fold") return onToggleFold(node.depth);
      if (node.kind === "approval") return onOpenApproval(node.rowId);
      if (node.stepKey !== "") return onSelectStep(node.stepKey);
    },
    [onOpenApproval, onSelectStep, onToggleColumn, onToggleFold, steering],
  );

  if (world.goal === null) {
    return (
      <div className="os-nexus-map" data-empty>
        <Caption>
          {state === "seeding" ? "Loading from the cluster" : "Nothing to draw yet."}
        </Caption>
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
            return (
              <line
                key={`road-${index}`}
                className="os-nexus-road"
                data-thought={point.thought || undefined}
                data-done={point.done || undefined}
                x1={px(from.x)}
                y1={py(from.y)}
                x2={px(point.x)}
                y2={py(point.y)}
                strokeWidth={point.thought ? THICK : THIN}
              />
            );
          })}

          {/* Real dependency edges, under the nodes. */}
          {model.edges.map((edge, index) => {
            const a = model.nodes.get(edge.from);
            const b = model.nodes.get(edge.to);
            if (a === undefined || b === undefined) return null;
            return (
              <line
                key={`edge-${index}`}
                className="os-nexus-edge"
                data-kind={edge.kind}
                x1={px(a.x)}
                y1={py(a.y)}
                x2={px(b.x)}
                y2={py(b.y)}
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
            ? `As it stood at ${at}.`
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
      <text className="os-nexus-beacon-label" x={cx} y={cy + BEACON_R + 18} textAnchor="middle">
        {truncate(node.label, 34)}
      </text>
      <text className="os-nexus-beacon-count" x={cx} y={cy + BEACON_R + 32} textAnchor="middle">
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
      <text className="os-nexus-node-label" x={cx} y={cy - STEP_R - 7} textAnchor="middle">
        {truncate(node.label, node.kind === "template" ? 26 : 18)}
      </text>
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
          x={cx - 9}
          y={cy - 9}
          width={18}
          height={18}
          rx={5}
        />
      );
    case "fold":
      return (
        <rect
          className="os-nexus-fold"
          x={cx - 26}
          y={cy - 9}
          width={52}
          height={18}
          rx={9}
        />
      );
    case "cluster":
      return <circle className="os-nexus-cluster" cx={cx} cy={cy} r={STEP_R + 5} />;
    case "binding":
      return (
        <rect
          className="os-nexus-binding"
          x={cx - 5}
          y={cy - 5}
          width={10}
          height={10}
          rx={2}
        />
      );
    case "approval":
      return (
        <path
          className="os-nexus-approval"
          d={`M ${cx} ${cy - 7} L ${cx + 7} ${cy} L ${cx} ${cy + 7} L ${cx - 7} ${cy} Z`}
        />
      );
    default:
      return <circle className="os-nexus-step" cx={cx} cy={cy} r={STEP_R} />;
  }
}

function describe(node: LayoutNode): string {
  switch (node.kind) {
    case "you":
      return "You, where the goal was set";
    case "template":
      return `Automation ${node.label}, ${node.status}`;
    case "fold":
      return `${node.standsFor} finished steps, folded. Open to see them.`;
    case "cluster":
      return `${node.standsFor} steps running together. Open to see them.`;
    case "binding":
      return `Ran on ${node.label}`;
    case "approval":
      return node.status === "waiting"
        ? `Waiting on you: ${node.label}`
        : `${node.label}, ${node.status}`;
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
  const when = at === "" ? "" : `, as it stood at ${at}`;
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
