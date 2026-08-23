import { useMemo, type ReactNode } from "react";

import type { ConstructRow, DependencyEdgeRow } from "../scene/world";

// The bundle's dependency graph, in two dimensions.
//
// ===========================================================================
// SVG, NOT WEBGL, AND THAT IS NOT A SHORTCUT
// ===========================================================================
// The map is 3D because it is about TIME and ACTIVITY -- a thing arriving, a
// thing pulsing, a thing filling up. A dependency graph is about STRUCTURE:
// what this query needs, what breaks if that concept changes. Structure reads
// better flat, it is selectable and searchable as DOM, it needs no GPU, and
// it costs the page nothing. Drawing it in the same canvas would have been
// consistency at the expense of every one of those.
//
// It is drawn from THE SAME ROWS the map draws (design 5), which is why this
// takes constructs and edges rather than a graph someone assembled: two
// representations of one bundle would be two chances to disagree about what
// depends on what.
//
// ===========================================================================
// LAYERED BY DEPENDENCY DEPTH
// ===========================================================================
// A construct sits one column to the right of the deepest thing it depends
// on, so dependencies flow right-to-left and a reader can find the base of
// the bundle by looking at the left edge. Deterministic, like everything else
// in this feature: the same bundle draws the same picture twice.
//
// A dependency the bundle does NOT contain -- `toSource` of `core` or
// `catalog` -- is drawn as an outline node rather than omitted. Omitting it
// would make a query that depends on a core concept look like it depends on
// nothing, which is the opposite of true.

const NODE_W = 168;
const NODE_H = 44;
const COL_GAP = 76;
const ROW_GAP = 18;
const PAD = 12;

interface GraphNode {
  key: string;
  name: string;
  kind: string;
  // Present for a construct this bundle authored; absent for a core or
  // catalog dependency, which is what the outline styling says.
  status: string;
  external: boolean;
  x: number;
  y: number;
}

// depthOf resolves each node's column. Iterative rather than recursive
// because a bundle CAN carry a cycle -- the authoring path does not forbid
// one, and a recursive walk would blow the stack rather than draw the mess.
// A cycle settles at whatever depth the iteration cap leaves it, which draws
// as a graph with edges pointing backwards: visibly wrong, which is the
// correct rendering of a bundle that is wrong.
function depths(names: readonly string[], edges: readonly DependencyEdgeRow[]): Map<string, number> {
  const depth = new Map<string, number>(names.map((name) => [name, 0]));
  const dependsOn = new Map<string, string[]>();
  for (const edge of edges) {
    const list = dependsOn.get(edge.fromConstruct);
    if (list === undefined) dependsOn.set(edge.fromConstruct, [edge.toName]);
    else list.push(edge.toName);
  }
  for (let pass = 0; pass < names.length + 1; pass += 1) {
    let moved = false;
    for (const name of names) {
      let want = 0;
      for (const dep of dependsOn.get(name) ?? []) {
        want = Math.max(want, (depth.get(dep) ?? 0) + 1);
      }
      if (want !== (depth.get(name) ?? 0)) {
        depth.set(name, want);
        moved = true;
      }
    }
    if (!moved) break;
  }
  return depth;
}

export function DependencyGraph({
  constructs,
  edges,
}: {
  constructs: readonly ConstructRow[];
  edges: readonly DependencyEdgeRow[];
}): ReactNode {
  const { nodes, width, height, byKey } = useMemo(() => {
    const external = new Map<string, string>();
    for (const edge of edges) {
      if (edge.toSource === "bundle") continue;
      if (constructs.some((construct) => construct.name === edge.toName)) continue;
      external.set(edge.toName, edge.toKind);
    }

    const names = [
      ...constructs.map((construct) => construct.name),
      ...external.keys(),
    ].sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
    const depth = depths(names, edges);

    const columns = new Map<number, string[]>();
    for (const name of names) {
      const d = depth.get(name) ?? 0;
      const list = columns.get(d);
      if (list === undefined) columns.set(d, [name]);
      else list.push(name);
    }

    const placed: GraphNode[] = [];
    let maxRows = 0;
    for (const [column, list] of [...columns.entries()].sort((a, b) => a[0] - b[0])) {
      maxRows = Math.max(maxRows, list.length);
      list.forEach((name, index) => {
        const construct = constructs.find((c) => c.name === name);
        placed.push({
          key: name,
          name,
          kind: construct?.kind ?? external.get(name) ?? "",
          status: construct?.status ?? "",
          external: construct === undefined,
          x: PAD + column * (NODE_W + COL_GAP),
          y: PAD + index * (NODE_H + ROW_GAP),
        });
      });
    }

    const cols = columns.size === 0 ? 1 : columns.size;
    return {
      nodes: placed,
      byKey: new Map(placed.map((node) => [node.key, node])),
      width: PAD * 2 + cols * NODE_W + (cols - 1) * COL_GAP,
      height: PAD * 2 + Math.max(1, maxRows) * NODE_H + Math.max(0, maxRows - 1) * ROW_GAP,
    };
  }, [constructs, edges]);

  if (nodes.length === 0) {
    return <p className="text-sm text-muted">This bundle declares no dependencies.</p>;
  }

  return (
    // The wide-content rule: the graph scrolls inside its own box rather than
    // widening the page.
    <div className="overflow-x-auto">
      <svg
        width={width}
        height={height}
        viewBox={`0 0 ${width} ${height}`}
        role="img"
        aria-label={`Dependency graph: ${nodes.length} constructs, ${edges.length} edges`}
        className="text-fg"
      >
        <g>
          {edges.map((edge) => {
            const from = byKey.get(edge.fromConstruct);
            const to = byKey.get(edge.toName);
            if (from === undefined || to === undefined) return null;
            return (
              <line
                key={edge.id === "" ? `${edge.fromConstruct}->${edge.toName}` : edge.id}
                x1={from.x}
                y1={from.y + NODE_H / 2}
                x2={to.x + NODE_W}
                y2={to.y + NODE_H / 2}
                stroke="currentColor"
                strokeOpacity={0.35}
                strokeWidth={1}
              />
            );
          })}
        </g>
        {nodes.map((node) => (
          <g key={node.key}>
            <rect
              x={node.x}
              y={node.y}
              width={NODE_W}
              height={NODE_H}
              rx={6}
              fill="currentColor"
              fillOpacity={node.external ? 0 : 0.06}
              stroke="currentColor"
              strokeOpacity={0.45}
              strokeDasharray={node.external ? "4 3" : undefined}
            />
            <text x={node.x + 10} y={node.y + 18} fontSize={11} fill="currentColor" fillOpacity={0.6}>
              {node.kind}
              {node.external ? " (outside this bundle)" : ""}
            </text>
            <text x={node.x + 10} y={node.y + 33} fontSize={12} fill="currentColor">
              {node.name.length > 22 ? `${node.name.slice(0, 21)}…` : node.name}
            </text>
          </g>
        ))}
      </svg>
    </div>
  );
}
