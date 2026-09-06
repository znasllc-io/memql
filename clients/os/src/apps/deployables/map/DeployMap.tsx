import { useCallback, useMemo } from "react";
import type { LiveState } from "@znasllc-io/memql-sdk-core/client";

import { Caption } from "../../../kit";
import type { ArrivalKind, ArrivalTick } from "../../../live/arrival";
import type { SiteRow } from "../rows";
import { EMPTY_LAYOUT, layout, nodeCentre, type MapLayout, type MapNode } from "./layout";
import { transformOf } from "../../../kit/viewport";
import { usePanZoom } from "../../../kit/usePanZoom";

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
  // The gesture is `kit/usePanZoom`, shared with the Nexus beacon map since
  // epic memql#4785: pointer, touch, wheel and keyboard over one viewport,
  // with the pointer-capture, pinch-baseline and drag-threshold rules that
  // each took a bug to get right. Nothing about it is specific to this map.
  const { view, frameRef, handlers, steering } = usePanZoom({
    width: model.width,
    height: model.height,
    ready: model.nodes.length > 0,
  });

  /**
   * Open a node -- unless the pointer was steering rather than choosing.
   *
   * The keyboard path does not come through here, so Enter on a node always
   * opens it however far the map was last dragged.
   */
  const activate = useCallback(
    (node: MapNode) => {
      if (steering()) return;
      onSelect(node);
    },
    [onSelect, steering],
  );


  if (model.nodes.length === 0) {
    return (
      <div className="os-deploy-map" data-empty>
        <Caption>
          {state === "seeding"
            ? "Loading from the cluster"
            : behind
              ? "Not connected to the cluster, so there is nothing to draw."
              : "No deployables to map yet. New deployable, on the Deployables section, is where one starts."}
        </Caption>
      </div>
    );
  }

  return (
    <div className="os-deploy-map" ref={frameRef} data-behind={behind || undefined}>
      <svg
        className="os-deploy-map-canvas"
        role="application"
        aria-label="Deploy map"
        tabIndex={0}
        {...handlers}
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
