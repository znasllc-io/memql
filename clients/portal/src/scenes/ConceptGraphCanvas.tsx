import { useMemo, useRef, type ReactNode } from "react";
import { Canvas, useFrame } from "@react-three/fiber";
import { OrbitControls } from "@react-three/drei";
import * as THREE from "three";

import { readPalette } from "./palette";
import type { ConceptGraph, GraphNode } from "./conceptGraph";

// The constellation, drawn (epic memql#4661, task memql#4672).
//
// ===========================================================================
// THIS MODULE IMPORTS three.js, AND ITS SIBLING DOES NOT
// ===========================================================================
// The scene registry's rule: everything
// the graph can be wrong about that does not need a GPU lives in
// conceptGraph.ts and is tested there; this file draws what that returns and
// is reached only through a dynamic import. scenes.test.ts fails the build if
// any other module in the portal imports three, fiber or drei.
//
// ===========================================================================
// FRAME LOOP ON DEMAND
// ===========================================================================
// `frameloop="demand"` means fiber renders when something asks and otherwise
// does nothing -- no rAF, no GPU work, no wakeups. So the loop below decides
// whether anything is still moving and invalidates only then, rather than
// running every frame.
//
// THE PREDICATE IS EVALUATED PER FRAME, not captured at render. A boolean
// closed over at render time either spins forever or never wakes: this is the
// bug a captured-boolean governor produces, and it is the shape to avoid here.
//
// Under reduced motion there is no drift at all, so the loop settles on the
// first frame and stays settled.

export default function ConceptGraphCanvas({
  graph,
  selectedRowId,
  onSelect,
  reducedMotion,
}: {
  graph: ConceptGraph;
  selectedRowId: string;
  onSelect: (rowId: string) => void;
  reducedMotion: boolean;
}): ReactNode {
  const palette = useMemo(() => readPalette(), []);

  return (
    <div className="h-[22rem] w-full overflow-hidden rounded-lg border border-line">
      <Canvas
        frameloop="demand"
        // DPR-aware anti-aliasing: capped at 2 because the third device pixel
        // buys nothing visible and costs four times the fragments.
        dpr={[1, 2]}
        gl={{ antialias: true }}
        camera={{ position: [0, 0, 3.4], fov: 50 }}
      >
        <color attach="background" args={[palette.ground]} />
        {/* Ambient plus one key light: enough for the bevelled geometry to
            read as solid without modelling a studio. */}
        <ambientLight intensity={0.65} />
        <directionalLight position={[3, 4, 5]} intensity={1.1} />
        {/* The rim. Behind and below, so an edge catches on every node and the
            silhouette separates from the ground. */}
        <directionalLight position={[-4, -2, -5]} intensity={0.55} color={palette.agent} />

        <Nodes
          graph={graph}
          selectedRowId={selectedRowId}
          onSelect={onSelect}
          colour={palette.agent}
          selectedColour={palette.goal}
        />
        <Edges graph={graph} colour={palette.road} />

        <Drift reducedMotion={reducedMotion} />
        <OrbitControls
          enablePan={false}
          enableDamping={!reducedMotion}
          // Zoom and rotate, never pan: a graph on a sphere has no "away", and
          // a panned camera is one a person cannot get back from.
          minDistance={2}
          maxDistance={7}
        />
      </Canvas>
    </div>
  );
}

// One instanced mesh for every node.
//
// INSTANCED because the count is unbounded up to the cap: 300 nodes is one
// draw call rather than three hundred. The retired goal map made the same choice for
// the same reason, and does NOT instance its singletons -- there is no
// singleton here, so everything is instanced.
function Nodes({
  graph,
  selectedRowId,
  onSelect,
  colour,
  selectedColour,
}: {
  graph: ConceptGraph;
  selectedRowId: string;
  onSelect: (rowId: string) => void;
  colour: string;
  selectedColour: string;
}): ReactNode {
  const ref = useRef<THREE.InstancedMesh>(null);

  useMemo(() => {
    const mesh = ref.current;
    if (mesh === null) return;
    applyInstances(mesh, graph.nodes, selectedRowId, colour, selectedColour);
  }, [graph, selectedRowId, colour, selectedColour]);

  // A second pass on mount: the ref is null during the memo above on the very
  // first render, which is when there is most to place.
  useFrame(() => {
    const mesh = ref.current;
    if (mesh === null || mesh.userData["placed"] === true) return;
    applyInstances(mesh, graph.nodes, selectedRowId, colour, selectedColour);
    mesh.userData["placed"] = true;
  });

  return (
    <instancedMesh
      ref={ref}
      args={[undefined, undefined, Math.max(graph.nodes.length, 1)]}
      onClick={(event) => {
        const index = event.instanceId;
        if (index === undefined) return;
        const node = graph.nodes[index];
        if (node !== undefined) onSelect(node.id);
      }}
    >
      {/* BEVELLED, not a cube. An icosahedron at detail 1 has 80 faces and
          reads as a rounded solid under a key light, where a box reads as a
          placeholder -- which is the note the owner made about the map. */}
      <icosahedronGeometry args={[0.045, 1]} />
      <meshStandardMaterial roughness={0.35} metalness={0.1} vertexColors />
    </instancedMesh>
  );
}

function applyInstances(
  mesh: THREE.InstancedMesh,
  nodes: readonly GraphNode[],
  selectedRowId: string,
  colour: string,
  selectedColour: string,
): void {
  const matrix = new THREE.Matrix4();
  const base = new THREE.Color(colour);
  const chosen = new THREE.Color(selectedColour);
  nodes.forEach((node, index) => {
    matrix.makeTranslation(node.x, node.y, node.z);
    // The selected node is bigger as well as brighter: colour alone is not a
    // distinction somebody with a colour-vision deficiency can rely on.
    const scale = node.id === selectedRowId ? 1.9 : 1;
    matrix.scale(new THREE.Vector3(scale, scale, scale));
    mesh.setMatrixAt(index, matrix);
    mesh.setColorAt(index, node.id === selectedRowId ? chosen : base);
  });
  mesh.instanceMatrix.needsUpdate = true;
  if (mesh.instanceColor !== null) mesh.instanceColor.needsUpdate = true;
  mesh.count = nodes.length;
}

// The edges, as one line-segment geometry.
//
// ONE geometry rather than a line per edge, for the draw-call reason above.
// Lines are unlit on purpose: an edge is a relationship, not an object, and
// shading it would make it compete with the nodes it connects.
function Edges({ graph, colour }: { graph: ConceptGraph; colour: string }): ReactNode {
  const geometry = useMemo(() => {
    const byId = new Map(graph.nodes.map((n) => [n.id, n]));
    const points: number[] = [];
    for (const edge of graph.edges) {
      const from = byId.get(edge.from);
      const to = byId.get(edge.to);
      if (from === undefined || to === undefined) continue;
      points.push(from.x, from.y, from.z, to.x, to.y, to.z);
    }
    const g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.Float32BufferAttribute(points, 3));
    return g;
  }, [graph]);

  if (graph.edges.length === 0) return null;
  return (
    <lineSegments geometry={geometry}>
      <lineBasicMaterial color={colour} transparent opacity={0.45} />
    </lineSegments>
  );
}

// The slow rotation that makes a sphere read as a sphere.
//
// It is the only thing animating, so it is also the only thing keeping the
// demand loop awake -- which is why it stops entirely under reduced motion
// rather than merely slowing: a person who asked for less motion asked for
// none, and stopping it also stops the frame loop.
function Drift({ reducedMotion }: { reducedMotion: boolean }): ReactNode {
  useFrame((state, delta) => {
    // Per-frame, not captured: a boolean closed over at render either spins
    // forever or never wakes.
    if (reducedMotion) return;
    state.scene.rotation.y += delta * 0.08;
    state.invalidate();
  });
  return null;
}
