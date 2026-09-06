import { Suspense, lazy, type ReactNode } from "react";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

import { Skeleton } from "../ui";

// The SCENE registry (epic memql#4661, task memql#4672).
//
// ===========================================================================
// A SCENE IS AN ELEMENT
// ===========================================================================
// The map was a page. It was a good page, and being a page is exactly what
// made it unreusable: a three-dimensional reading of a row set is a way of
// SHOWING data, which is what an element is, and there was no way to put one
// in a view.
//
// The scene element kind fixes that. An arrangement names a scene by id;
// sanitizeArrangement drops an id this build does not carry; any layout can
// host one, and the natural home is a focus layout's hero slot.
//
// ===========================================================================
// THE REGISTRY IS CLOSED, LIKE THE WIDGET REGISTRY AND FOR THE SAME REASON
// ===========================================================================
// Scenes are predefined, data-bound modules. An arrangement PLACES one and can
// never define one, so a regeneration cannot invent a visualisation and a
// stored arrangement from a release that had a scene this build lacks is
// repaired rather than crashing.
//
// ===========================================================================
// THE LAZY-CHUNK RULE, EXTENDED
// ===========================================================================
// three.js, react-three-fiber and drei are the portal's largest dependency and
// no other page uses them. The rule is "only ConceptGraphCanvas.tsx may import
// them", enforced by scenes.test.ts over the whole portal tree.
//
// The rule used to name NexusCanvas.tsx as well; the Nexus pages are deleted
// (work spine A1, decision D7) and the goal map went with them, because the
// rows it drew are being replaced and MemQL OS -- where Nexus is rebuilt --
// carries no WebGL at all.
//
// Every entry below is therefore a `lazy()` boundary. That is not an
// optimisation to revisit: a static import here would put the entire WebGL
// stack in the main bundle, because this registry is reachable from every
// arranged page in the console.

export interface SceneProps {
  concept: Concept;
  rows: readonly Row[];
  selectedRowId: string;
  onSelect: (rowId: string) => void;
}

export interface SceneDefinition {
  readonly id: string;
  readonly title: string;
  // One sentence for a composer's picker and for the prompt that offers this
  // to a model, in the words a person would use.
  readonly summary: string;
  // What this scene needs of a concept to be worth placing. Prose, shown
  // beside the offer -- a scene that cannot say what it needs is a scene
  // somebody places once and never again.
  readonly needs: string;
  readonly render: (props: SceneProps) => ReactNode;
}

const ConceptGraphScene = lazy(async () => ({
  default: (await import("./ConceptGraphScene")).ConceptGraphScene,
}));

export const SCENES: readonly SceneDefinition[] = [
  {
    id: "conceptGraph",
    title: "Constellation",
    summary: "This concept's rows as points, with an edge for every declared relationship.",
    needs: "Rows. Relationships make it a graph rather than a cloud.",
    render: (props) => (
      <ConceptGraphScene
        concept={props.concept}
        rows={props.rows}
        selectedRowId={props.selectedRowId}
        onSelect={props.onSelect}
      />
    ),
  },
];

export const SCENE_IDS: readonly string[] = SCENES.map((s) => s.id);

export function sceneById(id: string): SceneDefinition | undefined {
  return SCENES.find((scene) => scene.id === id);
}

// renderScene is the one call site. The Suspense boundary is per scene rather
// than per page: a page whose data has loaded should not be blank because a
// six-megabyte WebGL chunk is still arriving.
export function renderScene(id: string, props: SceneProps): ReactNode {
  const scene = sceneById(id);
  if (scene === undefined) return null;
  return (
    <Suspense fallback={<Skeleton variant="rows" rows={6} />}>{scene.render(props)}</Suspense>
  );
}
