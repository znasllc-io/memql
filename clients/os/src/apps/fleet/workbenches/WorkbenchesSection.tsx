import { useMemo, useState } from "react";
import { FleetTabs, RefreshButton, useFleetScroll } from "../FleetControls";
import { InfoDetail } from "../../../kit/InfoDetail";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { LiveList } from "../../../live/LiveList";
import { feedIsBehind } from "../../../live/useLiveCollection";
import { useLiveView } from "../../../live/liveView";
import { formatFreshness, formatMoment } from "../../../kit/format";
import {
  nodeFromRow,
  RELEASE_REASON_BLURB,
  workspaceFromRow,
  workspacesByNode,
  type WorkbenchNodeRow,
  type WorkspaceRow,
} from "../rows";
import { Button, EmptyState, Switch, Fact, Facts, Head, Notice, Panel } from "../../../kit";
import { useNow } from "../../../kit/useNow";
import { useWorkbenches } from "./useWorkbenches";

// Workbenches: the replicas that host per-plan working directories, and the
// directories on them.
//
// A workspace is a FILESYSTEM on ONE replica's disk (memql#4354), which is
// why the replica is the organizing fact rather than a column. When a replica
// leaves the mesh its directories go with it -- they are not migrated -- and
// the plan is given a fresh workspace elsewhere. That is a design decision
// rather than a recovery failure, and this screen is where an operator finds
// it out.

export function WorkbenchesSection() {
  const state = useWorkbenches();
  const [tab, setTab] = useState<"workspaces" | "replicas">("workspaces");
  const root = useFleetScroll(tab);
  const [showReleased, setShowReleased] = useState(false);
  const now = useNow(30_000);

  // Ordered by replica, then newest first inside each -- so the flat list the
  // LiveList renders reads as the grouping the headers draw. Sorting in the
  // VIEW rather than at render is what lets one LiveList own the whole list:
  // its state caption, its arrival cues and its empty text all describe the
  // same feed, which two lists could not do.
  // PROJECT, then narrow, then order -- one pass over the raw rows the
  // collection holds (see useWorkbenches: the fold stores what the wire sent).
  const view = useLiveView<Row, WorkspaceRow>(
    state.source,
    `released:${showReleased}`,
    (rows) => {
      const all = rows.map(workspaceFromRow).filter((one) => one.id !== "");
      const visible = showReleased ? all : all.filter((one) => one.status !== "released");
      return workspacesByNode(visible).flatMap((group) => group.workspaces);
    },
  );

  // Sorted by id so the replica list reads the same way on every render; the
  // collection preserves arrival order, which for a set of replicas is
  // whatever order the mesh happened to answer in.
  const nodeView = useLiveView<Row, WorkbenchNodeRow>(state.nodeSource, "nodes", (rows) =>
    rows
      .map(nodeFromRow)
      .filter((node) => node.id !== "")
      .sort((a, b) => a.id.localeCompare(b.id)),
  );
  const nodes = nodeView?.snapshot.rows ?? [];

  // The group boundaries, computed from the same rows the list renders.
  const firstOfGroup = useMemo(() => {
    const rows = view?.snapshot.rows ?? [];
    const seen = new Set<string>();
    const first = new Set<string>();
    for (const one of rows) {
      if (seen.has(one.nodeId)) continue;
      seen.add(one.nodeId);
      first.add(one.id);
    }
    return first;
  }, [view?.snapshot.rows]);

  const occupied = useMemo(
    () => new Set((view?.snapshot.rows ?? []).map((one) => one.nodeId)),
    [view?.snapshot.rows],
  );
  const emptyReplicas = nodes.filter((node) => !occupied.has(node.id));

  return (
    <div ref={root} className="os-fleet">
      <div className="fleet-section-header">
      <Head title="Cluster workspaces">
        {/* OFFERED ONLY WHEN THE FEED IS BEHIND. Both feeds on this screen
            are live, and a refresh control standing next to a live list
            quietly contradicts it -- it says "this may be stale" about rows
            that arrive on their own. When the caption says the feed IS
            behind, the same control is exactly the right one, and its
            appearance is itself the signal. */}
        {feedIsBehind(state.workspaceState) ? (
          <RefreshButton label="Reconnect workspaces" onClick={state.reseedWorkspaces} />
        ) : null}
      </Head>

      <FleetTabs label="Cluster workspace views" value={tab} onChange={setTab} options={[["workspaces", "Workspaces"], ["replicas", "Replicas"]]} />
      </div>
      <div className="fleet-section-context"><p>Temporary working folders for tasks running in the cluster.</p><InfoDetail title="Cluster workspaces"><p className="os-caption">
        A workspace is a sandboxed working directory for one plan, on one workbench replica's
        disk. Nothing on your own computer is touched.
      </p></InfoDetail></div>

      <div hidden={tab !== "replicas"}><ReplicaPanel
        state={state}
        source={nodeView}
        nodes={nodes}
        emptyReplicas={emptyReplicas}
        now={now}
      /></div>
      <div hidden={tab !== "workspaces"}>

      {state.workspaceError ? (
        <Notice
          tone="error"
          sentence="Workspaces could not be updated."
          detail={state.workspaceError}
        />
      ) : null}

      {/* The section's two halves are parallel: replicas are supporting
          context and sit in a panel, workspaces are the subject and run full
          width. Both carry a heading, or the per-replica group bars below
          read as belonging to nothing. */}
      <div className="fleet-list-options"><Switch checked={showReleased} onChange={setShowReleased}>Show released</Switch><span className="os-caption">Include folders removed after a run.</span></div>

      {/* Keyed on the toggle so revealing released rows re-baselines the
          arrival cues: without it they would flash "new" on the next
          workspace event, claiming the cluster just sent rows this browser
          already had. */}
      <LiveList<WorkspaceRow>
        key={`workspaces:${showReleased}`}
        source={view}
        rowId={(w) => w.id}
        // `lastUsedAt` is deliberately absent: a workspace being touched
        // again is activity, not news, and every workbench call would
        // otherwise flash the row it ran in. Being RELEASED is the news.
        fingerprint={(w) => `${w.status}|${w.releasedAt}|${w.releasedReason}`}
        label="Your workspaces"
        emptyText="No workspaces yet."
        emptyContent={<EmptyState title={showReleased ? "No workspaces yet" : "No active workspaces"} action={!showReleased ? <Button onClick={() => setShowReleased(true)}>View released workspaces</Button> : <Button onClick={() => setTab("replicas")}>Check replicas</Button>}>MemQL creates a temporary working folder when one of your plans needs the workbench. {showReleased ? "Your workspace history will appear here." : "Finished runs release their folders; you can include that history."}</EmptyState>}
        renderRow={(w, tick) => (
          <WorkspaceLine
            workspace={w}
            tick={tick}
            now={now}
            groupHead={firstOfGroup.has(w.id)}
          />
        )}
      />
      </div>
    </div>
  );
}

function ReplicaPanel({
  state,
  source,
  nodes,
  emptyReplicas,
  now,
}: {
  state: ReturnType<typeof useWorkbenches>;
  source: ReturnType<typeof useLiveView<Row, WorkbenchNodeRow>>;
  nodes: readonly WorkbenchNodeRow[];
  emptyReplicas: readonly WorkbenchNodeRow[];
  now: Date;
}) {
  return (
    <Panel label="Workbench replicas">
      <div className="os-head">
        <span className="os-caption">Cluster workbench capacity</span>
        <div className="os-head-actions">
          {feedIsBehind(state.nodeState) ? (
            <RefreshButton label="Reconnect replicas" onClick={state.reseedNodes} />
          ) : null}
        </div>
      </div>

      {state.nodeError ? (
        <Notice
          tone="error"
          sentence="Replicas could not be updated."
          next={
            nodes.length > 0
              ? "Showing the last available replicas."
              : "Nothing was loaded."
          }
          detail={state.nodeError}
        />
      ) : null}

      {/* Through LiveList like every other live surface in the OS, which is
          what makes the three states this panel has to tell apart legible
          without a hand-rolled caption for each: "no workbench replicas are
          deployed" is the empty text, and "the feed is seeding / degraded /
          disconnected" is the LiveState line underneath it. */}
      <LiveList<WorkbenchNodeRow>
        source={source}
        rowId={(n) => n.id}
        // `lastSeen` absent for the reason a machine's `lastSeenAt` is: a
        // replica heartbeating is its normal condition, and announcing it
        // would make the panel strobe. A replica CHANGING HEALTH is news.
        fingerprint={(n) => `${n.health}|${n.address}`}
        label="Workbench replicas"
        emptyText="No workbench replicas are running in this cluster."
        emptyContent={<EmptyState title="No workbench replicas">A workbench replica provides space for tasks to run. Ask your cluster administrator to start one before running work that needs a workspace.</EmptyState>}
        renderRow={(node, tick) => (
          <details className="fleet-record fleet-replica-record">
            <summary><span className="fleet-record-identity"><strong>{node.id}</strong><small>Workbench replica</small></span><span className="fleet-record-status" data-health={node.health}>{node.health || "Health unknown"}</span><span className="fleet-record-meta">Seen {formatFreshness(node.lastSeen, now)}</span></summary>
            <div className="fleet-record-detail"><Facts><Fact label="Address" value={node.address || "Not reported"} mono /><Fact label="First seen" value={formatMoment(node.createdAt)} /><Fact label="Last seen" value={formatMoment(node.lastSeen)} /><Fact label="Your workspaces" value={emptyReplicas.some(one => one.id === node.id) ? "None in this view" : "Listed in Workspaces"} /></Facts></div>
            {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
          </details>
        )}
      />
    </Panel>
  );
}

function WorkspaceLine({
  workspace,
  tick,
  now,
  groupHead,
}: {
  workspace: WorkspaceRow;
  tick: "added" | "updated" | null;
  now: Date;
  groupHead: boolean;
}) {
  const released = workspace.status === "released";
  const reason = workspace.releasedReason;
  const blurb = RELEASE_REASON_BLURB[reason];

  return (
    <div className="os-fleet-workspace" data-released={released || undefined}>
      {groupHead ? (
        <p className="os-fleet-groupbar os-mono">
          {workspace.nodeId === "" ? "replica not recorded" : workspace.nodeId}
        </p>
      ) : null}
      <details className="os-fleet-workspace-body"><summary className="fleet-workspace-summary"><span className="fleet-record-identity"><strong>{workspace.runId || workspace.id}</strong><small>Run workspace</small></span><span>{workspace.status}</span><span className="os-caption">{formatFreshness(workspace.lastUsedAt, now)}</span></summary>
        <Facts>
          <Fact label="Run" value={workspace.runId} mono />
          <Fact label="Status" value={workspace.status} mono />
          <Fact label="Directory" value={workspace.storageRoot} mono />
          <Fact label="Provisioned" value={formatMoment(workspace.createdAt)} />
          <Fact
            label="Last used"
            value={formatFreshness(workspace.lastUsedAt, now)}
            title={workspace.lastUsedAt || undefined}
          />
          {released ? <Fact label="Released" value={formatMoment(workspace.releasedAt)} /> : null}
          {released ? (
            <Fact label="Reason" value={reason === "" ? "not recorded" : reason} mono />
          ) : null}
        </Facts>
        {/* node_lost gets the explicit copy: the files are gone with the
            replica, they were NOT migrated, and the plan was given a fresh
            workspace elsewhere. An operator reading "node_lost" alone would
            reasonably go looking for the directory. */}
        {released && blurb ? (
          <Notice tone={reason === "node_lost" ? "warn" : "info"}>
            <p className="os-notice-line">{blurb}</p>
          </Notice>
        ) : null}
        {released && !blurb && reason !== "" ? (
          <p className="os-caption">
            Release reason: {reason}.
          </p>
        ) : null}
        {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
      </details>
    </div>
  );
}
