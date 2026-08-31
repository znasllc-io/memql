import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { LiveState } from "@znasllc-io/memql-sdk-core/client";

import { Caption } from "../../../kit";
import type { ArrivalKind, ArrivalTick } from "../../../live/arrival";
import type { SiteRow } from "../rows";
import { EMPTY_LAYOUT, layout, nodeCentre, type MapLayout, type MapNode } from "./layout";
import {
  IDENTITY,
  KEY_PAN_STEP,
  KEY_ZOOM_STEP,
  fitTo,
  panBy,
  transformOf,
  zoomAt,
  type Viewport,
} from "./viewport";

// The deploy map: what serves where, as a shape.
//
// ===========================================================================
// PLAIN SVG. NO three.js, AND THAT IS A RULE RATHER THAN A PREFERENCE
// ===========================================================================
// The portal's Nexus is the platform's ONE 3D surface, and it pays for that
// with a lazy chunk and a guard test holding three.js behind a dynamic import.
// This map answers a flat question -- which host, which site, which bundle --
// and a WebGL renderer would buy it nothing while making the OS bundle carry
// the largest dependency the portal has. `test/deployables/map.test.tsx`
// asserts the module graph stays clean, with a reachable-positive half so its
// silence is evidence rather than a statement about the regex.
//
// Every colour is a token, so a theme restyles the map with no code. Every
// position comes from `layout.ts`, so the map lays out identically in a test
// and on a screen.

/**
 * Total pointer travel, in screen pixels, past which a press is a DRAG.
 *
 * Manhattan distance rather than Euclidean: it is the cheaper arithmetic and it
 * over-counts slightly, which errs toward treating a wobble as a drag -- and
 * failing to open a deployable is recoverable in a way that opening one
 * somebody did not ask for, mid-drag, is not.
 */
const DRAG_THRESHOLD_PX = 5;

const GLYPHS: Record<string, string> = {
  spa: "{ }",
  static: "</>",
  shopify_storefront: "$",
};

export function DeployMap({
  sites,
  ticks,
  state,
  selectedNodeId,
  onSelect,
}: {
  sites: readonly SiteRow[];
  /** Arrival cues by SITE id -- the same ones the list beside this shows. */
  ticks: Map<string, ArrivalTick>;
  state: LiveState;
  selectedNodeId: string;
  onSelect: (node: MapNode) => void;
}) {
  const model: MapLayout = useMemo(() => (sites.length === 0 ? EMPTY_LAYOUT : layout(sites)), [sites]);
  const [view, setView] = useState<Viewport>(IDENTITY);
  const frame = useRef<HTMLDivElement | null>(null);

  // A DEGRADED FEED MUST NOT READ AS A HEALTHY FLEET. A map is a picture of
  // now; when the subscription is behind, the picture is of some earlier now,
  // and the only honest thing to do is say so and dim it.
  const behind = state === "degraded" || state === "disconnected";

  const nodeById = useMemo(() => {
    const m = new Map<string, MapNode>();
    for (const n of model.nodes) m.set(n.id, n);
    return m;
  }, [model]);

  const selected = selectedNodeId === "" ? null : (nodeById.get(selectedNodeId) ?? null);
  const selectedSites = selected?.siteIds ?? [];

  // ---- pan and zoom -------------------------------------------------------
  //
  // Pointer events cover mouse, pen and touch with one code path, which is what
  // makes "works with pointer and touch" a property of the implementation
  // rather than of two implementations that have to agree. Two live pointers
  // are a PINCH; one is a drag.
  const pointers = useRef(new Map<number, { x: number; y: number }>());
  const pinch = useRef<{ distance: number; midX: number; midY: number } | null>(null);
  /**
   * How far the pointer has travelled since it went down.
   *
   * A node lives INSIDE the canvas, so a drag that begins on one bubbles to the
   * pan handler and then ends in a `click` on that node -- which would open a
   * deployable somebody was only steering past. Anything past the threshold is
   * a drag and swallows the click; anything under it is a click with a shaky
   * hand, which is most clicks.
   */
  const travelled = useRef(0);

  const localPoint = useCallback((clientX: number, clientY: number) => {
    const box = frame.current?.getBoundingClientRect();
    return { x: clientX - (box?.left ?? 0), y: clientY - (box?.top ?? 0) };
  }, []);

  const onPointerDown = useCallback((e: React.PointerEvent<SVGSVGElement>) => {
    // A PRIMARY pointer down starts a fresh gesture, so anything still in the
    // map is stale -- a pointerup that landed outside the element in a browser
    // with no pointer capture. Without this, the next press would see two live
    // pointers and read a single-finger drag as a pinch, which scales the map
    // by whatever the ghost happened to be sitting at. The second finger of a
    // real pinch is NOT primary, so it never clears the first.
    if (e.isPrimary) pointers.current.clear();
    pointers.current.set(e.pointerId, { x: e.clientX, y: e.clientY });
    pinch.current = null;
    travelled.current = 0;
    // Capture so a drag that leaves the frame keeps steering. jsdom has no
    // implementation, and a map that threw on mousedown under test would be a
    // map nothing could test -- hence the optional call rather than a branch.
    try {
      (e.currentTarget as unknown as Element).setPointerCapture?.(e.pointerId);
    } catch {
      // A browser that refuses capture still pans; it just stops at the edge.
    }
  }, []);

  const onPointerMove = useCallback(
    (e: React.PointerEvent<SVGSVGElement>) => {
      const held = pointers.current.get(e.pointerId);
      if (!held) return;
      const previous = { ...held };
      pointers.current.set(e.pointerId, { x: e.clientX, y: e.clientY });

      const live = [...pointers.current.values()];
      const a = live[0];
      const b = live[1];
      if (a && b) {
        const distance = Math.hypot(a.x - b.x, a.y - b.y);
        const mid = localPoint((a.x + b.x) / 2, (a.y + b.y) / 2);
        const last = pinch.current;
        pinch.current = { distance, midX: mid.x, midY: mid.y };
        // The FIRST move after a second finger lands only establishes the
        // baseline: a ratio against a distance nobody measured yet would jump
        // the scale by whatever the fingers happened to be apart.
        if (last && last.distance > 0 && distance > 0) {
          setView((v) => zoomAt(v, distance / last.distance, mid.x, mid.y));
        }
        return;
      }

      const dx = e.clientX - previous.x;
      const dy = e.clientY - previous.y;
      travelled.current += Math.abs(dx) + Math.abs(dy);
      setView((v) => panBy(v, dx, dy));
    },
    [localPoint],
  );

  /**
   * Open a node -- unless the pointer was steering rather than choosing.
   *
   * The keyboard path does not come through here, so Enter on a node always
   * opens it however far the map was last dragged.
   */
  const activate = useCallback(
    (node: MapNode) => {
      if (travelled.current > DRAG_THRESHOLD_PX) return;
      onSelect(node);
    },
    [onSelect],
  );

  const endPointer = useCallback((e: React.PointerEvent<SVGSVGElement>) => {
    pointers.current.delete(e.pointerId);
    if (pointers.current.size < 2) pinch.current = null;
  }, []);

  const onWheel = useCallback(
    (e: React.WheelEvent<SVGSVGElement>) => {
      // deltaY < 0 is a scroll UP, which is a zoom IN everywhere else in
      // every map anybody has used.
      const factor = e.deltaY < 0 ? KEY_ZOOM_STEP : 1 / KEY_ZOOM_STEP;
      const at = localPoint(e.clientX, e.clientY);
      setView((v) => zoomAt(v, factor, at.x, at.y));
    },
    [localPoint],
  );

  const onKeyDown = useCallback((e: React.KeyboardEvent<SVGSVGElement>) => {
    // The keyboard steers the same viewport the pointer does. It is not a
    // fallback: a map you can only reach with a mouse is a map somebody cannot
    // read at all, and this is the app's signature surface.
    const centre = () => {
      const box = frame.current?.getBoundingClientRect();
      return { x: (box?.width ?? 0) / 2, y: (box?.height ?? 0) / 2 };
    };
    switch (e.key) {
      case "ArrowLeft":
        setView((v) => panBy(v, KEY_PAN_STEP, 0));
        break;
      case "ArrowRight":
        setView((v) => panBy(v, -KEY_PAN_STEP, 0));
        break;
      case "ArrowUp":
        setView((v) => panBy(v, 0, KEY_PAN_STEP));
        break;
      case "ArrowDown":
        setView((v) => panBy(v, 0, -KEY_PAN_STEP));
        break;
      case "+":
      case "=": {
        const c = centre();
        setView((v) => zoomAt(v, KEY_ZOOM_STEP, c.x, c.y));
        break;
      }
      case "-":
      case "_": {
        const c = centre();
        setView((v) => zoomAt(v, 1 / KEY_ZOOM_STEP, c.x, c.y));
        break;
      }
      case "0":
        setView(IDENTITY);
        break;
      default:
        return;
    }
    e.preventDefault();
  }, []);

  // ==========================================================================
  // FIT ONCE, THEN NEVER AGAIN
  // ==========================================================================
  // A map that opens clipped is a map somebody has to discover they can pan
  // before they can read it, and a cluster with a dozen deployables is taller
  // than any window this draws in. So the first paint frames the whole thing.
  //
  // ONCE is the whole rule. A row set that changes shape must NOT re-fit:
  // somebody who panned into a corner to read a bundle reference would be
  // thrown back to the origin because a publish landed somewhere else on the
  // map -- which is the same class of rudeness as a list that scrolls itself.
  // An empty map resets instead, so the next one that opens starts framed.
  const fitted = useRef(false);
  useEffect(() => {
    if (model.nodes.length === 0) {
      fitted.current = false;
      setView(IDENTITY);
      return;
    }
    if (fitted.current) return;
    const box = frame.current?.getBoundingClientRect();
    // An unmeasured frame -- a window mid-open, a hidden desk, jsdom -- is not
    // a fit of zero; it is no answer yet, so the attempt is left for a later
    // render rather than being spent on a viewport nobody has laid out.
    if (!box || box.width <= 0 || box.height <= 0) return;
    fitted.current = true;
    setView(fitTo(model.width, model.height, box.width, box.height));
  }, [model]);

  if (model.nodes.length === 0) {
    return (
      <div className="os-deploy-map" data-empty>
        <Caption>
          {state === "seeding"
            ? "Loading from the cluster"
            : behind
              ? "Not connected to the cluster, so there is nothing to draw."
              : "No deployables to map yet. Create one from the Actions section."}
        </Caption>
      </div>
    );
  }

  return (
    <div className="os-deploy-map" ref={frame} data-behind={behind || undefined}>
      <svg
        className="os-deploy-map-canvas"
        role="application"
        aria-label="Deploy map"
        tabIndex={0}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={endPointer}
        onPointerCancel={endPointer}
        onWheel={onWheel}
        onKeyDown={onKeyDown}
      >
        <g data-os-map-view transform={transformOf(view)}>
          {model.groups.map((group) => (
            <g key={group.id} className="os-deploy-group">
              <rect
                className="os-deploy-group-box"
                x={group.x - 8}
                y={group.y}
                width={group.w + 16}
                height={group.h}
                rx={10}
              />
              <text className="os-deploy-group-label" x={group.x} y={group.y + 18}>
                {group.label}
              </text>
            </g>
          ))}

          {model.edges.map((edge) => {
            const from = nodeById.get(edge.from);
            const to = nodeById.get(edge.to);
            if (!from || !to) return null;
            const a = nodeCentre(from);
            const b = nodeCentre(to);
            return (
              <path
                key={edge.id}
                className="os-deploy-edge"
                data-selected={selectedSites.includes(edge.siteId) || undefined}
                d={`M ${from.x + from.w} ${a.y} C ${from.x + from.w + 24} ${a.y}, ${to.x - 24} ${b.y}, ${to.x} ${b.y}`}
              />
            );
          })}

          {model.nodes.map((node) => (
            <MapNodeShape
              key={node.id}
              node={node}
              tick={tickFor(node, ticks)}
              selected={selectedNodeId === node.id}
              inSelectedCluster={node.siteIds.some((id) => selectedSites.includes(id))}
              onActivate={() => activate(node)}
              onKeyActivate={() => onSelect(node)}
            />
          ))}
        </g>
      </svg>
      <Caption>
        {behind
          ? "Live updates are behind -- this is the last shape the cluster sent, not the shape it has."
          : "Drag to pan, scroll to zoom. Arrow keys pan, + and - zoom, 0 resets; Tab walks the map."}
      </Caption>
    </div>
  );
}

/**
 * The cue a node shows.
 *
 * Keyed on the SITE, because that is what changed -- a bundle node has no row
 * of its own, so a publish announces itself on the site node and on the bundle
 * the site now points at. A shared bundle takes the cue if ANY of its sites
 * moved, which is the reading somebody watching wants: something about this
 * bundle's world just changed.
 */
function tickFor(node: MapNode, ticks: Map<string, ArrivalTick>): ArrivalKind | null {
  let best: ArrivalKind | null = null;
  for (const siteId of node.siteIds) {
    const kind = ticks.get(siteId)?.kind;
    if (kind === "added") return "added";
    if (kind === "updated") best = "updated";
  }
  return best;
}

function MapNodeShape({
  node,
  tick,
  selected,
  inSelectedCluster,
  onActivate,
  onKeyActivate,
}: {
  node: MapNode;
  tick: ArrivalKind | null;
  selected: boolean;
  inSelectedCluster: boolean;
  /** The pointer path, which a drag suppresses. */
  onActivate: () => void;
  /** The keyboard path, which nothing suppresses. */
  onKeyActivate: () => void;
}) {
  const glyph = node.kind === "site" ? (GLYPHS[node.siteKind] ?? "?") : null;
  return (
    <g
      className="os-deploy-node"
      data-kind={node.kind}
      data-arrival={tick ?? undefined}
      data-selected={selected || undefined}
      data-in-cluster={inSelectedCluster || undefined}
      transform={`translate(${node.x} ${node.y})`}
      role="button"
      tabIndex={0}
      aria-label={ariaLabelFor(node)}
      aria-pressed={selected}
      onClick={onActivate}
      onKeyDown={(e) => {
        if (e.key !== "Enter" && e.key !== " ") return;
        // Stop here: the map's own key handler pans on space-adjacent keys and
        // would otherwise scroll the view out from under the thing just opened.
        e.preventDefault();
        e.stopPropagation();
        onKeyActivate();
      }}
    >
      {/* The untruncated value, for a pointer. `aria-label` carries it for a
          screen reader; this is the same fact for everyone else. */}
      <title>{node.full}</title>
      <rect className="os-deploy-node-box" width={node.w} height={node.h} rx={8} />
      {node.kind === "site" ? (
        <circle className="os-deploy-node-dot" data-status={node.status || "draft"} cx={14} cy={14} r={4} />
      ) : null}
      {glyph ? (
        <text className="os-deploy-node-glyph" x={node.w - 10} y={18} textAnchor="end">
          {glyph}
        </text>
      ) : null}
      <text className="os-deploy-node-label" x={node.kind === "site" ? 26 : 10} y={18}>
        {node.label}
      </text>
      {node.sublabel === "" ? null : (
        <text className="os-deploy-node-sub" x={10} y={34}>
          {node.sublabel}
        </text>
      )}
    </g>
  );
}

/**
 * The node, read out.
 *
 * SVG text is not a label -- a screen reader walking a `<g>` gets whatever the
 * author gave it -- so every node states its kind, its value and, for a site,
 * its status. A shared bundle says how many deployables it serves, because that
 * is the fact the picture carries and the words otherwise would not.
 */
function ariaLabelFor(node: MapNode): string {
  // EVERY READING USES `full`, never the fitted `label`. A screen reader told
  // "blog.memql.exa..." has been given less than nothing, and the whole reason
  // the truncation is a display concern is that the fact underneath it is not.
  switch (node.kind) {
    case "host":
      return `Host ${node.full}`;
    case "site":
      return `Deployable ${node.full}, ${node.status || "status unknown"}${node.siteKind ? `, ${node.siteKind}` : ""}`;
    case "bundle":
      return `Bundle ${node.label}${node.siteIds.length > 1 ? `, serving ${node.siteIds.length} deployables` : ""}: ${node.full}`;
    default:
      return `Library artifact ${node.full}`;
  }
}
