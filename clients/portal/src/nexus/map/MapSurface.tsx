import { Suspense, lazy, useMemo, useState, type ReactNode } from "react";

import { Badge, Panel, Skeleton } from "../../ui";
import type { LayoutResult } from "../scene/layout";
import { goalProgress } from "../scene/scene";
import { receipt } from "../scene/receipt";
import type { GoalWorld } from "../scene/world";
import { CompletionCard } from "./CompletionCard";
import { toneForTask } from "./palette";
import { probeWebGL } from "./webgl";
import { useReducedMotion } from "./useReducedMotion";

// The map, with everything that is not three.js.
//
// ===========================================================================
// THE SCENE IS A LAZY CHUNK
// ===========================================================================
// three.js, react-three-fiber and drei are the portal's largest dependency by
// a wide margin, and every other page in the console has no use for them. The
// import below is the only reference to that module, so the bundler puts the
// whole graph behind it in its own chunk and nothing pays for it until
// somebody opens a goal.
//
// ===========================================================================
// A REAL FALLBACK, NOT A TEST STUB
// ===========================================================================
// WebGL is unavailable more often than it looks: hardware acceleration off, a
// locked-down enterprise profile, a crashed GPU process, a headless browser.
// The map's answer is not a blank canvas -- it is the same information without
// the picture, which this surface already has in the phase summary below and
// which Replay's event list carries in full.
//
// That the portal's own tests run in jsdom, which has no WebGL, is a
// consequence of this rather than a reason for it: because the fallback is
// real, the route and state tests exercise the page a person on a locked-down
// laptop actually sees, and the canvas is tested where it belongs -- in a
// browser.

const NexusCanvas = lazy(() => import("./NexusCanvas"));

export interface MapSurfaceProps {
  world: GoalWorld;
  // The laid-out scene. Passed IN rather than computed here because the page
  // needs the same one to resolve /node/:nodeId, and two layouts of one world
  // would be two answers to where a node is.
  scene: LayoutResult;
  // Which node's detail is open, from the route. "" when none is.
  selectedNodeId: string;
  onSelect: (nodeId: string) => void;
  // Clicking a cluster expands the phase it stands in for (design 4.2). The
  // expanded set lives on the page beside the layout it feeds.
  onExpandPhase: (phase: string) => void;
  // Bumped by Replay when the scrub jumps backwards, so arrivals replay.
  arrivalEpoch?: number;
  // Replay renders its own controls around the canvas and does not want the
  // receipt duplicated above them.
  showReceipt?: boolean;
}

export function MapSurface({
  world,
  scene,
  selectedNodeId,
  onSelect,
  onExpandPhase,
  arrivalEpoch = 0,
  showReceipt = true,
}: MapSurfaceProps): ReactNode {
  const reducedMotion = useReducedMotion();
  // Probed once per mount rather than per render: see webgl.ts on why it is
  // not a module-level constant either.
  const [webgl] = useState(probeWebGL);
  const [hovered, setHovered] = useState("");
  const [dismissed, setDismissed] = useState(false);

  const progress = useMemo(() => goalProgress(world), [world]);
  const card = useMemo(() => receipt(world), [world]);

  function select(nodeId: string): void {
    // A cluster node is not a row, so it has no detail to open -- clicking it
    // EXPANDS the phase it stands for instead (design 4.2, memql#4376).
    const node = scene.nodes.get(nodeId);
    if (node?.kind === "cluster") {
      onExpandPhase(node.phase);
      return;
    }
    onSelect(nodeId);
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="relative overflow-hidden rounded-lg border border-line bg-surface" style={{ height: "62vh", minHeight: "420px" }}>
        {webgl ? (
          <Suspense fallback={<div className="p-6"><Skeleton variant="rows" rows={6} /></div>}>
            <NexusCanvas
              world={world}
              scene={scene}
              progress={progress}
              hoveredNodeId={hovered}
              selectedNodeId={selectedNodeId}
              onHover={setHovered}
              onSelect={select}
              reducedMotion={reducedMotion}
              arrivalEpoch={arrivalEpoch}
            />
          </Suspense>
        ) : (
          <div className="flex h-full flex-col justify-center gap-3 p-6">
            <h2 className="text-sm font-semibold">This browser cannot draw the map</h2>
            <p className="max-w-2xl text-sm text-muted">
              The scene needs WebGL, which this browser has not made available -- hardware
              acceleration may be switched off, or the profile may block it. Everything the
              map shows is also on this page as text, and Replay's event list is a complete
              index of every node and every moment.
            </p>
          </div>
        )}
      </div>

      {/* The phase summary. Present whether or not the canvas rendered: it is
          the map's reading in text, and it is what makes the fallback above a
          fallback rather than an apology. */}
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs font-semibold tracking-wide text-muted uppercase">Phases</span>
        {scene.phases.length === 0 ? (
          <span className="text-sm text-muted">No tasks yet.</span>
        ) : (
          scene.phases.map((phase) => (
            <Badge key={phase.name} tone="neutral">
              {phase.name === "" ? "unnamed" : phase.name}
              <span className="ml-1.5 text-muted">{phase.count}</span>
              {phase.collapsed ? <span className="ml-1.5 text-subtle">collapsed</span> : null}
            </Badge>
          ))
        )}
        <span className="ml-auto text-sm text-muted">
          {progress.completed} of {progress.total} tasks complete
        </span>
      </div>

      {showReceipt && card !== null && !dismissed ? (
        <CompletionCard receipt={card} onDismiss={() => setDismissed(true)} />
      ) : null}

      {hovered === "" ? null : (
        <Panel>
          <HoverReading scene={scene} nodeId={hovered} world={world} />
        </Panel>
      )}
    </div>
  );
}

// What the hovered node is, in words. The canvas lights the path; this says
// the name -- and it is here rather than in a canvas tooltip so it is
// selectable text on the page.
function HoverReading({
  scene,
  nodeId,
  world,
}: {
  scene: LayoutResult;
  nodeId: string;
  world: GoalWorld;
}): ReactNode {
  const node = scene.nodes.get(nodeId);
  if (node === undefined) return null;
  const task = world.tasks.find((t) => t.id === node.rowId);
  return (
    <div className="flex flex-wrap items-center gap-3 text-sm">
      <span className="text-xs tracking-wide text-muted uppercase">{node.kind}</span>
      <span className="font-medium">{node.label}</span>
      {task === undefined ? null : <Badge tone={toneForTask(task.status)}>{task.status}</Badge>}
      {node.toolInvocations === 0 ? null : (
        <span className="text-muted">
          {node.toolInvocations} tool {node.toolInvocations === 1 ? "call" : "calls"}
        </span>
      )}
      {node.clusterCount === 0 ? null : (
        <span className="text-muted">{node.clusterCount} tasks -- click to expand</span>
      )}
    </div>
  );
}
