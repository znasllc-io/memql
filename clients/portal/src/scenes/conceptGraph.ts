import type { ConceptRelationshipLike } from "@znasllc-io/memql-view-kit";
import { resolveDisplayCard } from "@znasllc-io/memql-view-kit";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

// The conceptGraph scene's arithmetic (epic memql#4661, task memql#4672).
//
// PURE, AND IMPORTS NO three.js. Everything the graph can be wrong about that
// does not need a GPU is here and is tested without one -- the same split the
// registry already makes between scenes/ and scenes/ConceptGraphCanvas.tsx,
// for the same reason: a scene that renders nothing at all does so silently,
// with a live WebGL context and a full scene graph behind it, so the parts
// that CAN be asserted must be assertable.

export interface GraphNode {
  readonly id: string;
  // What a person reads on the node, from the concept's own display card.
  readonly label: string;
  // The status member, when the concept declares a status slot. Drives colour.
  readonly status: string;
  // Position on the unit sphere, in [-1, 1]. Scaled by the canvas.
  readonly x: number;
  readonly y: number;
  readonly z: number;
}

export interface GraphEdge {
  readonly from: string;
  readonly to: string;
  // The DOMAIN label (`as`), which is what an edge MEANS. Falls back to the
  // pointer field's name -- never to the engine's structural `type`, which
  // would present "references" to a person as though somebody chose it as a
  // word (memql#3652).
  readonly label: string;
}

export interface ConceptGraph {
  readonly nodes: readonly GraphNode[];
  readonly edges: readonly GraphEdge[];
  // Rows beyond the cap, which the canvas reports rather than silently
  // dropping. A graph that quietly showed 300 of 4,000 rows would be read as
  // the whole population.
  readonly omitted: number;
}

// The node budget. Above this the layout stops adding, for the same reason the
// the retired goal map clustered: a sphere of ten thousand points is not a reading of
// anything, and the frame cost is real.
export const GRAPH_NODE_CAP = 300;

// buildConceptGraph turns a row set plus the concept's DECLARED relationships
// into nodes and edges.
//
// EDGES COME FROM THE DECLARATION, NOT FROM THE VALUES. A foreign key is a
// string like any other string, so before the concept wire carried
// relationships (memql#4662) this scene was not possible: there was no way to
// know that `ownerAgentId` points at an agent and `goal` does not.
//
// SELF-CONTAINED EDGES ONLY, and that is a deliberate limit rather than a
// simplification. An edge is drawn when BOTH ends are in this row set -- a
// pointer to a row that is not loaded has no position to draw to, and drawing
// it to a stub would invent a node the reader would take for real. Cross-
// concept edges belong to a graph over several walks, which is a different
// feature (and would need the lookup resolver from memql#4671).
export function buildConceptGraph(
  concept: Concept,
  rows: readonly Row[],
  relationships: readonly ConceptRelationshipLike[] = concept.relationships ?? [],
): ConceptGraph {
  const capped = rows.slice(0, GRAPH_NODE_CAP);
  const card = resolveDisplayCard(
    { id: concept.id, entity: concept.entity, ...(concept.displayCard ? { displayCard: concept.displayCard } : {}) },
    capped as unknown as readonly Record<string, unknown>[],
  );

  const nodes: GraphNode[] = capped.map((row, index) => {
    const id = typeof row["id"] === "string" ? row["id"] : `row-${index}`;
    return {
      id,
      label: text(row, card.primary) || id,
      status: text(row, card.status),
      ...spherePoint(index, capped.length),
    };
  });

  const present = new Set(nodes.map((n) => n.id));
  const edges: GraphEdge[] = [];
  for (const rel of relationships) {
    // Only edges whose pointer is a top-level field of this concept. A dotted
    // path into a nested block is a real declaration and simply has no cell
    // here to read from.
    if (rel.field === "" || rel.field.includes(".")) continue;
    const label = rel.as !== undefined && rel.as !== "" ? rel.as : rel.field;
    for (const row of capped) {
      const from = typeof row["id"] === "string" ? row["id"] : "";
      const to = text(row, rel.field);
      if (from === "" || to === "" || from === to) continue;
      if (!present.has(to)) continue;
      edges.push({ from, to, label });
    }
  }

  return { nodes, edges, omitted: Math.max(rows.length - capped.length, 0) };
}

function text(row: Row, field: string | undefined): string {
  if (field === undefined || field === "") return "";
  const v = (row as Record<string, unknown>)[field];
  return typeof v === "string" ? v : typeof v === "number" || typeof v === "boolean" ? String(v) : "";
}

// spherePoint distributes n points evenly over a sphere by the Fibonacci
// lattice.
//
// DETERMINISTIC BY INDEX, which is the property that matters more than the
// distribution: a random layout would move every node whenever a row arrived,
// and a graph that reshuffles on every CDC event is unreadable. The same row
// set always produces the same picture, and one more row nudges the picture
// rather than replacing it.
export function spherePoint(
  index: number,
  count: number,
): { readonly x: number; readonly y: number; readonly z: number } {
  if (count <= 1) return { x: 0, y: 0, z: 0 };
  // The golden angle. Two decimals of it is plenty for a layout, and writing
  // it out keeps this readable next to the derivation.
  const golden = Math.PI * (3 - Math.sqrt(5));
  const y = 1 - (index / (count - 1)) * 2;
  const radius = Math.sqrt(Math.max(1 - y * y, 0));
  const theta = golden * index;
  return { x: Math.cos(theta) * radius, y, z: Math.sin(theta) * radius };
}
