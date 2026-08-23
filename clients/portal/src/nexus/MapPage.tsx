import { useMemo, useState, type ReactNode } from "react";
import { useNavigate, useParams } from "react-router-dom";

import { EmptyState } from "../ui";
import { MapSurface } from "./map/MapSurface";
import { NodeDetail } from "./NodeDetail";
import { RecentGoals } from "./GoalPicker";
import { useGoalContext } from "./GoalLayout";
import { useGoals } from "./useGoals";
import { layout } from "./scene/layout";
import { nexusPath, nodePath } from "./urls";

// /nexus/:planId -- the Map, and /node/:nodeId over it.
//
// ===========================================================================
// TWO LAYOUTS, ONE WORLD, AND THAT IS DELIBERATE
// ===========================================================================
// The DRAWN scene collapses a phase over the density threshold to one cluster
// node, so the tasks inside it are not in that layout at all. A deep link to
// one of those tasks would then resolve to nothing -- a dialog that refuses
// to open on a URL somebody sent, for a reason that has nothing to do with
// the row.
//
// A node's IDENTITY does not depend on whether its phase is drawn collapsed,
// so the resolution index is a second layout with the threshold lifted. It is
// a pure function over a few hundred rows, computed only when the world
// changes; the alternative is threading collapse state into the URL, which
// would make a link mean something different depending on how the sender's
// map happened to be arranged.

export function MapPage(): ReactNode {
  const { world, planId } = useGoalContext();
  const { nodeId = "" } = useParams();
  const navigate = useNavigate();
  const { goals } = useGoals();
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set());

  const scene = useMemo(() => layout(world, { expanded }), [world, expanded]);
  // The resolution index: every node, whatever the drawing does. See above.
  const index = useMemo(
    () => layout(world, { clusterThreshold: Number.POSITIVE_INFINITY }),
    [world],
  );

  if (world.plan === null) {
    return (
      <EmptyState statement="This goal has not been read yet." />
    );
  }

  return (
    <div className="flex flex-col gap-5">
      <MapSurface
        world={world}
        scene={scene}
        selectedNodeId={nodeId}
        onSelect={(id) => navigate(nodePath(planId, id))}
        onExpandPhase={(phase) => setExpanded((current) => new Set([...current, phase]))}
      />
      <RecentGoals goals={goals} currentId={planId} />
      {nodeId === "" ? null : (
        <NodeDetail scene={index} nodeId={nodeId} onClose={() => navigate(nexusPath(planId))} />
      )}
    </div>
  );
}
