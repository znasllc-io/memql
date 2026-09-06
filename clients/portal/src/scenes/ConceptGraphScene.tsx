import { Suspense, lazy, useMemo, useState, type ReactNode } from "react";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

import { Panel, Skeleton } from "../ui";
import { useReducedMotion } from "./useReducedMotion";
import { probeWebGL } from "./webgl";
import { buildConceptGraph } from "./conceptGraph";

// The Constellation, made live and generic (epic memql#4661, task memql#4672).
//
// ===========================================================================
// WHAT IT SHOWS
// ===========================================================================
// Any concept's rows as points, with an edge for every declared relationship
// whose two ends are both loaded. Before ConceptInfo carried relationships
// (memql#4662) this was not possible: a foreign key is a string like any
// other string, so there was no way to know that `ownerAgentId` points at an
// agent and `goal` does not.
//
// ===========================================================================
// THE CANVAS IS A LAZY CHUNK AND THIS FILE IS NOT
// ===========================================================================
// The lazy-chunk discipline, and the reason for it: three.js, fiber and
// drei are the portal's largest dependency, this registry is reachable from
// every arranged page, and a static import here would put the whole WebGL
// stack in the main bundle. So the three.js lives one dynamic import away, and
// everything a person sees when WebGL is unavailable lives here.
//
// ===========================================================================
// A REAL FALLBACK
// ===========================================================================
// WebGL is unavailable more often than it looks: hardware acceleration off, a
// locked-down profile, a crashed GPU process, a headless browser. The answer
// is the same information without the picture -- which for a graph is the node
// count, the edge count and what the edges MEAN, because "these rows are
// related by `respondsAs`" is most of what the picture says.

const ConceptGraphCanvas = lazy(() => import("./ConceptGraphCanvas"));

export function ConceptGraphScene({
  concept,
  rows,
  selectedRowId,
  onSelect,
}: {
  concept: Concept;
  rows: readonly Row[];
  selectedRowId: string;
  onSelect: (rowId: string) => void;
}): ReactNode {
  const graph = useMemo(() => buildConceptGraph(concept, rows), [concept, rows]);
  const reducedMotion = useReducedMotion();
  // Probed once per mount rather than per render, for the reason webgl.ts
  // states: the probe allocates a context.
  const [webgl] = useState(probeWebGL);

  if (graph.nodes.length === 0) {
    return <p className="text-sm text-subtle">No rows to draw for {concept.entity}.</p>;
  }

  const reading = (
    <p className="text-sm text-muted">
      {graph.nodes.length} {graph.nodes.length === 1 ? "row" : "rows"}
      {graph.omitted > 0 ? ` of ${graph.nodes.length + graph.omitted}` : ""}
      {graph.edges.length > 0
        ? `, ${graph.edges.length} ${graph.edges.length === 1 ? "link" : "links"} (${edgeLabels(graph.edges).join(", ")})`
        : ", no declared relationship links these rows to each other"}
      {graph.omitted > 0 ? ` — the newest ${graph.nodes.length} are drawn` : ""}
    </p>
  );

  if (!webgl) {
    return (
      <Panel>
        <p className="text-sm text-fg">
          This browser cannot draw the constellation, so here is what it says.
        </p>
        <div className="mt-2">{reading}</div>
      </Panel>
    );
  }

  return (
    <div className="flex flex-col gap-2">
      <Suspense fallback={<Skeleton variant="rows" rows={6} />}>
        <ConceptGraphCanvas
          graph={graph}
          selectedRowId={selectedRowId}
          onSelect={onSelect}
          reducedMotion={reducedMotion}
        />
      </Suspense>
      {reading}
    </div>
  );
}

// The distinct edge meanings, in first-seen order. What an edge MEANS is the
// `as` label; the count alone would not tell a reader what the lines are.
function edgeLabels(edges: readonly { readonly label: string }[]): readonly string[] {
  const seen: string[] = [];
  for (const edge of edges) if (!seen.includes(edge.label)) seen.push(edge.label);
  return seen;
}
