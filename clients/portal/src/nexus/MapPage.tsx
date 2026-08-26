import { useCallback, type ReactNode } from "react";
import { useNavigate, useParams } from "react-router-dom";

import { ArrangedPage } from "../pages/ArrangedPage";
import { RecentGoals } from "./GoalPicker";
import { useGoalContext } from "./GoalLayout";
import { useGoals } from "./useGoals";
import { GOAL_PAGE, GOAL_PAGE_ID } from "./goalPage";
import { nexusPath, nodePath } from "./urls";
import { NodeDetail } from "./NodeDetail";
import { layout } from "./scene/layout";

// The goal page (epic memql#4661, task memql#4673).
//
// It used to lay itself out: a MapSurface, a recent-goals strip and a node
// detail, stacked by hand. It is now an ARRANGEMENT -- a focus layout whose
// hero is the `goalMap` scene -- rendered by the same component that renders
// the five predefined views, the composed ones and the converged fleet pages.
//
// That is the proof spec D6 asks for. The richest page in the console, the one
// with a WebGL scene and hover and click-to-detail and a demand frame loop,
// speaks the same grammar as a table of users. And because it does, it is
// regenerable and versioned like every other page for free.
//
// WHAT STAYS HERE is what is genuinely about this ROUTE rather than about the
// page: the node detail the /node/:nodeId address opens, and the recent-goals
// strip, which is navigation to a different goal rather than a reading of this
// one.
export function MapPage(): ReactNode {
  const { world, planId } = useGoalContext();
  const { nodeId = "" } = useParams();
  const navigate = useNavigate();
  const { goals } = useGoals();

  // The FULL layout, uncollapsed: /node/:nodeId must resolve a node the map
  // may currently be showing as part of a cluster, so the index the detail
  // reads is computed with clustering off.
  const index = layout(world, { clusterThreshold: Number.POSITIVE_INFINITY });

  const onSelect = useCallback(
    (id: string) => navigate(nodePath(planId, id)),
    [navigate, planId],
  );

  return (
    <>
      <ArrangedPage
        manifest={GOAL_PAGE}
        pageId={GOAL_PAGE_ID}
        selectedRowId={planId}
        onSelect={onSelect}
      />
      <RecentGoals goals={goals} currentId={planId} />
      {nodeId === "" ? null : (
        <NodeDetail scene={index} nodeId={nodeId} onClose={() => navigate(nexusPath(planId))} />
      )}
    </>
  );
}
