import { useMemo, useState, type ReactNode } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { EmptyState } from "../../ui";
import { MapSurface } from "../map/MapSurface";
import { useGoalWorld } from "../feed/useGoalWorld";
import { layout } from "./layout";

// The goal map, AS A SCENE ELEMENT (epic memql#4661, task memql#4673).
//
// ===========================================================================
// EVERY SPECIAL BEHAVIOUR IS PRESERVED, BECAUSE NONE OF THEM MOVED
// ===========================================================================
// The map's materialization, its activity pulse, its hover and
// click-to-detail, its demand frame loop, its reduced-motion behaviour, its
// accessibility event list and its no-WebGL fallback all live in MapSurface
// and NexusCanvas, and this file does not touch any of them. What it adds is
// the seam: a scene element hands it ROWS, and it turns one of those rows into
// the goal to read.
//
// ===========================================================================
// ONE GOAL AT A TIME, AND WHOSE
// ===========================================================================
// A scene element is placed on a section over a POPULATION -- v1:planner:plan,
// which for the Nexus page is the caller's own goals. The map reads one goal,
// so the scene picks the selected row, else the first. That is the same
// one-goal-at-a-time rule the Nexus page has always had, expressed through the
// selection the arrangement already carries rather than through a route
// parameter.
//
// The requestedBy filter is NOT re-implemented here. It belongs to the read
// (see the Nexus notes on memql#4366: v1:planner:plan is undeclared, so
// planById answers for any id and the page filters client-side), and a second
// copy of a security-shaped filter is the copy that goes stale.
export function GoalMapScene({
  rows,
  selectedRowId,
  onSelect,
}: {
  rows: readonly Row[];
  selectedRowId: string;
  onSelect: (rowId: string) => void;
}): ReactNode {
  const planId = useMemo(() => {
    if (selectedRowId !== "") return selectedRowId;
    const first = rows[0];
    return typeof first?.id === "string" ? first.id : "";
  }, [rows, selectedRowId]);

  const { world } = useGoalWorld(planId);
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set());
  const scene = useMemo(() => layout(world, { expanded }), [world, expanded]);

  if (planId === "") {
    return <EmptyState statement="No goal to map yet." />;
  }
  if (world.plan === null) {
    return <EmptyState statement="This goal has not been read yet." />;
  }

  return (
    <MapSurface
      world={world}
      scene={scene}
      selectedNodeId=""
      onSelect={onSelect}
      onExpandPhase={(phase) => setExpanded((current) => new Set([...current, phase]))}
    />
  );
}
