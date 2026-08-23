import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Canvas, useFrame, useThree } from "@react-three/fiber";
import { Html, OrbitControls } from "@react-three/drei";
import * as THREE from "three";

import type { GoalProgress } from "../scene/scene";
import type { LayoutNode, LayoutResult } from "../scene/layout";
import type { GoalWorld, NodeKind } from "../scene/world";
import { latestAttempts } from "../scene/world";
import { colourForTask, readPalette, type ScenePalette } from "./palette";
import { easeOutBack, timingsFor } from "./motion";

// The scene.
//
// ===========================================================================
// THIS MODULE IS THE ONLY ONE THAT IMPORTS three.js
// ===========================================================================
// Everything the map can be wrong about that does not need a GPU lives in
// ../scene/ and is tested without one. This file draws what those functions
// return, and it is loaded as a LAZY CHUNK (see MapSurface.tsx) so the rest of
// the portal's bundle does not pay for three.js, react-three-fiber and drei on
// a page that never opens the map.
//
// ===========================================================================
// FRAME LOOP ON DEMAND
// ===========================================================================
// `frameloop="demand"` means React Three Fiber renders when something asks it
// to and otherwise does nothing at all -- no requestAnimationFrame, no GPU
// work, no wakeups. That is what keeps an operations console with a map open
// in a background tab from costing a core.
//
// So the loop below is inverted from the usual one: instead of running every
// frame and animating, it decides whether anything is STILL animating and
// invalidates only then. Arrivals in flight, a task pulsing because it is
// running, the goal filling -- each keeps the loop awake; when the last of
// them settles the loop stops on its own. Under reduced motion the pulse and
// the drift do not exist, so it settles sooner and stays settled.
//
// ===========================================================================
// INSTANCING, AND WHERE IT IS NOT USED
// ===========================================================================
// The four kinds whose count is unbounded -- tasks, specialists, artifacts,
// constructs -- draw through one instanced mesh each, so a 300-task goal is
// four draw calls rather than three hundred.
//
// The singletons are NOT instanced, and that is a decision rather than an
// omission: you, the goal, the planner and the bundle are one node each, and
// an instanced mesh for a set of one costs an instance buffer, a matrix
// upload and a second code path to keep correct, in exchange for removing a
// draw call that was never the problem. The cluster node is in the same
// position -- there is at most one per phase, and a phase count is small by
// construction.

const LABEL_DISTANCE = 26;
// The particle pool. One geometry, one draw call, sliced between whichever
// nodes are currently condensing -- so a burst of arrivals costs no
// allocation and a scene at rest costs nothing at all.
const PARTICLE_POOL = 480;
const PARTICLES_PER_ARRIVAL = 24;

export interface NexusCanvasProps {
  world: GoalWorld;
  scene: LayoutResult;
  progress: GoalProgress;
  hoveredNodeId: string;
  selectedNodeId: string;
  onHover: (nodeId: string) => void;
  onSelect: (nodeId: string) => void;
  reducedMotion: boolean;
  // Bumped by Replay when the scrub position jumps backwards, so nodes that
  // "un-arrive" and arrive again replay their animation instead of being
  // remembered as already-arrived from the previous pass.
  arrivalEpoch: number;
}

// The four instanced kinds, with the geometry each draws.
const INSTANCED: readonly NodeKind[] = ["task", "specialist", "artifact", "construct"];

function geometryFor(kind: NodeKind): THREE.BufferGeometry {
  switch (kind) {
    case "you":
      return new THREE.IcosahedronGeometry(0.62, 0);
    case "goal":
      return new THREE.OctahedronGeometry(1.15, 0);
    case "planner":
      return new THREE.DodecahedronGeometry(0.72, 0);
    case "specialist":
      return new THREE.SphereGeometry(0.42, 16, 12);
    case "task":
      return new THREE.BoxGeometry(0.62, 0.62, 0.62);
    case "cluster":
      return new THREE.BoxGeometry(1.5, 1.5, 1.5);
    case "construct":
      return new THREE.TetrahedronGeometry(0.5, 0);
    case "artifact":
      // A disc: a cylinder with no height reads as a plate from every angle
      // the camera reaches, where a CircleGeometry disappears edge-on.
      return new THREE.CylinderGeometry(0.42, 0.42, 0.09, 18);
    case "bundle":
      return new THREE.TorusGeometry(0.62, 0.12, 10, 28);
  }
}

// nodeColour is the one place a node's tone is decided, for every kind.
function nodeColour(node: LayoutNode, statuses: Map<string, string>, palette: ScenePalette): string {
  switch (node.kind) {
    case "you":
      return palette.you;
    case "goal":
      return palette.goal;
    case "planner":
    case "specialist":
      return palette.agent;
    case "task":
    case "cluster":
      return colourForTask(statuses.get(node.id) ?? "queued", palette);
    case "artifact":
      return palette.artifact;
    case "construct":
      return palette.construct;
    case "bundle":
      return palette.bundle;
  }
}

// ---------------------------------------------------------------------------

// arrivalClock tracks when each node first appeared, which is what the
// arrival animation is a function of. Held in a ref rather than in state: it
// changes on every frame a node arrives, and a setState per frame would
// re-render the whole scene to animate one cube.
interface Arrival {
  at: number;
  particleSlot: number;
}

function useArrivals(
  nodeIds: readonly string[],
  epoch: number,
): { arrivals: Map<string, Arrival>; anyAnimating: (now: number, condenseMs: number, scaleMs: number) => boolean } {
  const arrivals = useRef(new Map<string, Arrival>());
  const nextSlot = useRef(0);
  const lastEpoch = useRef(epoch);

  if (lastEpoch.current !== epoch) {
    arrivals.current.clear();
    nextSlot.current = 0;
    lastEpoch.current = epoch;
  }

  const now = typeof performance === "undefined" ? 0 : performance.now();
  const present = new Set(nodeIds);
  for (const id of nodeIds) {
    if (arrivals.current.has(id)) continue;
    arrivals.current.set(id, { at: now, particleSlot: nextSlot.current });
    nextSlot.current = (nextSlot.current + PARTICLES_PER_ARRIVAL) % PARTICLE_POOL;
  }
  // A node that left (a scrub backwards, a row that was dropped) forgets its
  // arrival, so coming back is a fresh arrival rather than an instant pop.
  for (const id of [...arrivals.current.keys()]) {
    if (!present.has(id)) arrivals.current.delete(id);
  }

  return {
    arrivals: arrivals.current,
    anyAnimating: (at: number, condenseMs: number, scaleMs: number) => {
      const window = Math.max(condenseMs, scaleMs);
      for (const arrival of arrivals.current.values()) {
        if (at - arrival.at < window) return true;
      }
      return false;
    },
  };
}

// ---------------------------------------------------------------------------

function InstancedKind({
  kind,
  nodes,
  statuses,
  palette,
  arrivals,
  reducedMotion,
  hoveredNodeId,
  selectedNodeId,
  onHover,
  onSelect,
}: {
  kind: NodeKind;
  nodes: readonly LayoutNode[];
  statuses: Map<string, string>;
  palette: ScenePalette;
  arrivals: Map<string, Arrival>;
  reducedMotion: boolean;
  hoveredNodeId: string;
  selectedNodeId: string;
  onHover: (nodeId: string) => void;
  onSelect: (nodeId: string) => void;
}): ReactNode {
  const meshRef = useRef<THREE.InstancedMesh>(null);
  const geometry = useMemo(() => geometryFor(kind), [kind]);
  const timings = timingsFor(reducedMotion);
  const dummy = useMemo(() => new THREE.Object3D(), []);
  const colour = useMemo(() => new THREE.Color(), []);

  // Dispose the geometry when the kind changes or the scene unmounts. Three
  // does not garbage-collect GPU buffers, and a map that is opened and closed
  // twenty times in a session leaks twenty geometries without this.
  useEffect(() => () => geometry.dispose(), [geometry]);

  useFrame(() => {
    const mesh = meshRef.current;
    if (mesh === null) return;
    const now = performance.now();
    nodes.forEach((node, index) => {
      const arrival = arrivals.get(node.id);
      const age = arrival === undefined ? timings.scaleInMs : now - arrival.at;
      const t = timings.scaleInMs === 0 ? 1 : Math.min(1, age / timings.scaleInMs);
      let scale = easeOutBack(t, timings.overshoot);
      // A running task breathes. Not decoration: it is the one thing on the
      // map that says work is happening right now, and it is exactly what
      // reduced motion switches off (timings.breathAmplitude is 0 there).
      if (statuses.get(node.id) === "running" && timings.breathAmplitude > 0) {
        scale *= 1 + Math.sin(now / 260) * timings.breathAmplitude;
      }
      if (node.id === hoveredNodeId || node.id === selectedNodeId) scale *= 1.25;
      dummy.position.set(node.x, node.y, node.z);
      dummy.scale.setScalar(Math.max(0.001, scale));
      dummy.updateMatrix();
      mesh.setMatrixAt(index, dummy.matrix);
      colour.set(nodeColour(node, statuses, palette));
      mesh.setColorAt(index, colour);
    });
    mesh.instanceMatrix.needsUpdate = true;
    if (mesh.instanceColor !== null) mesh.instanceColor.needsUpdate = true;
  });

  if (nodes.length === 0) return null;

  return (
    <instancedMesh
      ref={meshRef}
      args={[geometry, undefined, nodes.length]}
      // `key` on the COUNT: an instanced mesh's buffers are sized at
      // construction, so growing the node set has to remount rather than
      // resize -- otherwise the new nodes render as garbage matrices left
      // over from whatever was in the buffer.
      key={`${kind}-${nodes.length}`}
      onPointerOver={(event) => {
        event.stopPropagation();
        const node = nodes[event.instanceId ?? -1];
        if (node !== undefined) onHover(node.id);
      }}
      onPointerOut={() => onHover("")}
      onClick={(event) => {
        event.stopPropagation();
        const node = nodes[event.instanceId ?? -1];
        if (node !== undefined) onSelect(node.id);
      }}
    >
      <meshStandardMaterial roughness={0.45} metalness={0.15} />
    </instancedMesh>
  );
}

function SingleGlyph({
  node,
  statuses,
  palette,
  arrivals,
  progress,
  reducedMotion,
  hoveredNodeId,
  selectedNodeId,
  onHover,
  onSelect,
}: {
  node: LayoutNode;
  statuses: Map<string, string>;
  palette: ScenePalette;
  arrivals: Map<string, Arrival>;
  progress: GoalProgress;
  reducedMotion: boolean;
  hoveredNodeId: string;
  selectedNodeId: string;
  onHover: (nodeId: string) => void;
  onSelect: (nodeId: string) => void;
}): ReactNode {
  const ref = useRef<THREE.Mesh>(null);
  const geometry = useMemo(() => geometryFor(node.kind), [node.kind]);
  const timings = timingsFor(reducedMotion);
  useEffect(() => () => geometry.dispose(), [geometry]);

  useFrame(() => {
    const mesh = ref.current;
    if (mesh === null) return;
    const now = performance.now();
    const arrival = arrivals.get(node.id);
    const age = arrival === undefined ? timings.scaleInMs : now - arrival.at;
    const t = timings.scaleInMs === 0 ? 1 : Math.min(1, age / timings.scaleInMs);
    let scale = easeOutBack(t, timings.overshoot);
    if (node.id === hoveredNodeId || node.id === selectedNodeId) scale *= 1.2;
    mesh.scale.setScalar(Math.max(0.001, scale));

    const material = mesh.material as THREE.MeshStandardMaterial;
    if (node.kind === "goal") {
      // The beacon FILLS with progress and LIGHTS when the plan succeeds.
      // Emissive intensity rather than colour, so a half-done goal is a
      // dimmer version of the finished one rather than a different object.
      material.emissiveIntensity = progress.lit ? 1.6 : 0.12 + progress.fraction * 0.9;
    } else if (node.kind === "planner" && timings.breathAmplitude > 0) {
      mesh.scale.multiplyScalar(1 + Math.sin(now / 900) * timings.breathAmplitude);
    }
  });

  const colour = nodeColour(node, statuses, palette);

  return (
    <mesh
      ref={ref}
      geometry={geometry}
      position={[node.x, node.y, node.z]}
      onPointerOver={(event) => {
        event.stopPropagation();
        onHover(node.id);
      }}
      onPointerOut={() => onHover("")}
      onClick={(event) => {
        event.stopPropagation();
        onSelect(node.id);
      }}
    >
      <meshStandardMaterial
        color={colour}
        emissive={colour}
        emissiveIntensity={node.kind === "goal" ? 0.12 : 0.05}
        wireframe={node.kind === "goal"}
        roughness={0.4}
        metalness={0.2}
      />
    </mesh>
  );
}

// Edges as one line-segment geometry rather than a line per edge: an edge is
// two vertices, and a hundred separate <line> objects is a hundred draw calls
// to draw what one buffer can.
function Edges({
  scene,
  palette,
  progress,
  hoveredPath,
}: {
  scene: LayoutResult;
  palette: ScenePalette;
  progress: GoalProgress;
  hoveredPath: readonly string[];
}): ReactNode {
  const geometry = useMemo(() => {
    const points: number[] = [];
    for (const edge of scene.edges) {
      const from = scene.nodes.get(edge.from);
      const to = scene.nodes.get(edge.to);
      if (from === undefined || to === undefined) continue;
      points.push(from.x, from.y, from.z, to.x, to.y, to.z);
    }
    const g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.Float32BufferAttribute(points, 3));
    return g;
  }, [scene]);
  useEffect(() => () => geometry.dispose(), [geometry]);

  // The hovered path: you -> the node, drawn brighter over the top. Design
  // 4.4 -- hovering a node lights the path from you to it, which is how an
  // operator reads "where did this come from" without clicking.
  const pathGeometry = useMemo(() => {
    const points: number[] = [];
    for (const id of hoveredPath) {
      const node = scene.nodes.get(id);
      if (node === undefined) continue;
      points.push(node.x, node.y, node.z);
    }
    const g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.Float32BufferAttribute(points, 3));
    return g;
  }, [scene, hoveredPath]);
  useEffect(() => () => pathGeometry.dispose(), [pathGeometry]);

  const roadGeometry = useMemo(() => {
    const g = new THREE.BufferGeometry();
    const you = scene.nodes.get("you");
    const goal = scene.nodes.get("goal");
    g.setAttribute(
      "position",
      new THREE.Float32BufferAttribute(
        you === undefined || goal === undefined ? [] : [you.x, you.y, you.z, goal.x, goal.y, goal.z],
        3,
      ),
    );
    return g;
  }, [scene]);
  useEffect(() => () => roadGeometry.dispose(), [roadGeometry]);

  return (
    <>
      <lineSegments geometry={geometry}>
        <lineBasicMaterial color={palette.grid} transparent opacity={0.55} />
      </lineSegments>
      {hoveredPath.length > 1 ? (
        // lineSegments rather than line: React's JSX types already own `line`
        // (SVG's), so the three.js element of that name is unreachable from
        // TSX. The path is always exactly two points -- you and the node --
        // so a segment list and a polyline draw the same thing here.
        <lineSegments geometry={pathGeometry}>
          <lineBasicMaterial color={palette.agent} transparent opacity={0.9} />
        </lineSegments>
      ) : null}
      {/* The road, brightening with progress. Drawn separately from the edge
          buffer because its opacity is a function of the goal's fill and the
          rest of the edges' is not. */}
      <lineSegments geometry={roadGeometry}>
        <lineBasicMaterial
          color={palette.you}
          transparent
          opacity={0.15 + progress.fraction * 0.5}
        />
      </lineSegments>
    </>
  );
}

// Particles, pooled. One geometry sized once; each arrival claims a slice for
// the length of its condense and the points travel inward to the node.
function Particles({
  nodes,
  arrivals,
  palette,
  reducedMotion,
}: {
  nodes: readonly LayoutNode[];
  arrivals: Map<string, Arrival>;
  palette: ScenePalette;
  reducedMotion: boolean;
}): ReactNode {
  const ref = useRef<THREE.Points>(null);
  const timings = timingsFor(reducedMotion);
  const positions = useMemo(() => new Float32Array(PARTICLE_POOL * 3), []);
  const geometry = useMemo(() => {
    const g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.BufferAttribute(positions, 3));
    return g;
  }, [positions]);
  useEffect(() => () => geometry.dispose(), [geometry]);

  useFrame(() => {
    const points = ref.current;
    if (points === null) return;
    const now = performance.now();
    // Everything parks at the origin and is scaled to nothing unless it is
    // claimed this frame; a stale particle left at its last position is a
    // speck hanging in the scene forever.
    positions.fill(0);
    for (const node of nodes) {
      const arrival = arrivals.get(node.id);
      if (arrival === undefined) continue;
      const age = now - arrival.at;
      if (age > timings.condenseMs) continue;
      const t = timings.condenseMs === 0 ? 1 : age / timings.condenseMs;
      for (let i = 0; i < PARTICLES_PER_ARRIVAL; i += 1) {
        const slot = (arrival.particleSlot + i) % PARTICLE_POOL;
        // A deterministic ring around the node, collapsing inward. Derived
        // from the index rather than Math.random so a replay of the same
        // arrival looks the same twice -- the determinism rule the scene
        // library follows applies to the pixels too.
        const angle = (i / PARTICLES_PER_ARRIVAL) * Math.PI * 2;
        const radius = (1 - t) * 2.4;
        positions[slot * 3] = node.x + Math.cos(angle) * radius;
        positions[slot * 3 + 1] = node.y + Math.sin(angle * 1.7) * radius * 0.6;
        positions[slot * 3 + 2] = node.z + Math.sin(angle) * radius;
      }
    }
    geometry.attributes["position"]!.needsUpdate = true;
  });

  if (timings.condenseMs === 0) return null;

  return (
    <points ref={ref} geometry={geometry}>
      <pointsMaterial color={palette.agent} size={0.09} transparent opacity={0.8} sizeAttenuation />
    </points>
  );
}

// The label layer. CSS2D through drei's <Html>, so labels are the portal's
// own type and tokens rather than a texture atlas that would need its own
// font pipeline and would not respect the theme.
//
// CULLED BY DISTANCE, because they are DOM elements: three hundred of them is
// three hundred absolutely-positioned divs the browser lays out every frame
// the camera moves, which is a cost the picture does not repay. The hovered
// and selected nodes are always labelled however far away they are -- that is
// the one label the operator is actually reading.
function Labels({
  nodes,
  hoveredNodeId,
  selectedNodeId,
}: {
  nodes: readonly LayoutNode[];
  hoveredNodeId: string;
  selectedNodeId: string;
}): ReactNode {
  const { camera } = useThree();
  const [visible, setVisible] = useState<readonly string[]>([]);

  useFrame(() => {
    const near: string[] = [];
    for (const node of nodes) {
      const d = Math.hypot(camera.position.x - node.x, camera.position.y - node.y, camera.position.z - node.z);
      if (d < LABEL_DISTANCE || node.kind === "goal" || node.kind === "you") near.push(node.id);
    }
    // Compared as a joined string so an unchanged set does not re-render the
    // label layer every frame the camera drifts.
    const key = near.join("|");
    setVisible((current) => (current.join("|") === key ? current : near));
  });

  const shown = new Set(visible);
  if (hoveredNodeId !== "") shown.add(hoveredNodeId);
  if (selectedNodeId !== "") shown.add(selectedNodeId);

  return (
    <>
      {nodes
        .filter((node) => shown.has(node.id) && node.label !== "")
        .map((node) => (
          <Html
            key={node.id}
            position={[node.x, node.y + 0.9, node.z]}
            center
            distanceFactor={18}
            // The label must never intercept a click meant for the glyph
            // under it, and it must not be read twice by a screen reader --
            // the event list is the accessible index (design 4.4), this is
            // decoration over a canvas.
            style={{ pointerEvents: "none" }}
            aria-hidden="true"
          >
            <span className="rounded bg-surface/80 px-1.5 py-0.5 text-[10px] whitespace-nowrap text-fg">
              {node.label}
              {node.toolInvocations > 0 ? (
                <span className="ml-1 text-muted">{node.toolInvocations}</span>
              ) : null}
            </span>
          </Html>
        ))}
    </>
  );
}

// The demand-mode governor. See this file's header: it invalidates while
// anything is still moving and then stops, which is what makes an idle map
// cost nothing.
//
// The predicate is a FUNCTION evaluated every frame, not a boolean computed
// at render time, and the difference is the whole correctness of this
// component. A boolean captured at render never changes while the loop is
// running -- no re-render happens during an animation, because nothing in
// React state moves -- so `animating: true` would invalidate forever and
// `animating: false` would never wake up. Asking each frame is what lets the
// loop notice the last arrival finishing and stop on its own.
function Governor({ shouldAnimate }: { shouldAnimate: () => boolean }): ReactNode {
  const { invalidate } = useThree();
  useFrame(() => {
    if (shouldAnimate()) invalidate();
  });
  return null;
}

export default function NexusCanvas({
  world,
  scene,
  progress,
  hoveredNodeId,
  selectedNodeId,
  onHover,
  onSelect,
  reducedMotion,
  arrivalEpoch,
}: NexusCanvasProps): ReactNode {
  const palette = useMemo(() => readPalette(), []);
  const nodes = useMemo(() => [...scene.nodes.values()], [scene]);
  const nodeIds = useMemo(() => nodes.map((n) => n.id), [nodes]);
  const { arrivals, anyAnimating } = useArrivals(nodeIds, arrivalEpoch);
  const timings = timingsFor(reducedMotion);

  // Task status by NODE id, from the latest attempt of each step -- the same
  // collapse the layout performs, so the cube's colour and the cube's
  // position are reading the same row.
  const statuses = useMemo(() => {
    const out = new Map<string, string>();
    for (const [id, task] of latestAttempts(world.tasks.filter((t) => t.category !== "toolInvocation"))) {
      out.set(id, task.status);
    }
    return out;
  }, [world.tasks]);

  const runningRef = useRef(false);
  runningRef.current = [...statuses.values()].some((status) => status === "running");

  const byKind = useMemo(() => {
    const out = new Map<NodeKind, LayoutNode[]>();
    for (const node of nodes) {
      const list = out.get(node.kind);
      if (list === undefined) out.set(node.kind, [node]);
      else list.push(node);
    }
    return out;
  }, [nodes]);

  // The path from you to the hovered node, for the highlight. Straight rather
  // than routed through the graph: the goal is to say WHERE the node is
  // relative to you, and a routed path through five task nodes says something
  // else (that the work flowed that way, which is not what the edges mean).
  const hoveredPath = useMemo(
    () => (hoveredNodeId === "" || hoveredNodeId === "you" ? [] : ["you", hoveredNodeId]),
    [hoveredNodeId],
  );

  // Evaluated per frame by the Governor, never at render time -- see its own
  // comment for why a boolean here would either spin forever or never start.
  const shouldAnimate = (): boolean => {
    const now = typeof performance === "undefined" ? 0 : performance.now();
    if (anyAnimating(now, timings.condenseMs, timings.scaleInMs)) return true;
    // A running task breathes, so the loop stays awake while any work is in
    // flight. Under reduced motion breathAmplitude is 0 and this term
    // disappears entirely, which is why a reduced-motion map settles as soon
    // as its arrivals have faded in and then costs nothing at all.
    return timings.breathAmplitude > 0 && runningRef.current;
  };

  return (
    <Canvas
      frameloop="demand"
      dpr={[1, 2]}
      camera={{ position: [scene.goalX * 0.55, 14, 26], fov: 48, near: 0.1, far: 400 }}
      style={{ background: palette.ground }}
      // Named for the accessible tree. The canvas itself carries no readable
      // content -- the event list does (design 4.4) -- so it is labelled and
      // then left alone.
      aria-label="Goal map"
      role="img"
    >
      <ambientLight intensity={0.55} />
      <directionalLight position={[12, 22, 14]} intensity={1.1} />
      <gridHelper
        args={[220, 44, palette.grid, palette.grid]}
        position={[scene.goalX / 2, -11, 0]}
      />

      <Edges scene={scene} palette={palette} progress={progress} hoveredPath={hoveredPath} />

      {INSTANCED.map((kind) => (
        <InstancedKind
          key={kind}
          kind={kind}
          nodes={byKind.get(kind) ?? []}
          statuses={statuses}
          palette={palette}
          arrivals={arrivals}
          reducedMotion={reducedMotion}
          hoveredNodeId={hoveredNodeId}
          selectedNodeId={selectedNodeId}
          onHover={onHover}
          onSelect={onSelect}
        />
      ))}

      {nodes
        .filter((node) => !INSTANCED.includes(node.kind))
        .map((node) => (
          <SingleGlyph
            key={node.id}
            node={node}
            statuses={statuses}
            palette={palette}
            arrivals={arrivals}
            progress={progress}
            reducedMotion={reducedMotion}
            hoveredNodeId={hoveredNodeId}
            selectedNodeId={selectedNodeId}
            onHover={onHover}
            onSelect={onSelect}
          />
        ))}

      <Particles nodes={nodes} arrivals={arrivals} palette={palette} reducedMotion={reducedMotion} />
      <Labels nodes={nodes} hoveredNodeId={hoveredNodeId} selectedNodeId={selectedNodeId} />

      {/* The camera is the operator's, and it is never in the URL (design
          D5). Damping off under reduced motion: a camera that keeps gliding
          after the pointer stops is drift, which is exactly what the
          preference asks not to happen. */}
      <OrbitControls
        makeDefault
        enableDamping={!reducedMotion}
        target={[scene.goalX / 2, 0, 0]}
      />
      <Governor shouldAnimate={shouldAnimate} />
    </Canvas>
  );
}
