import { useMemo, useState } from "react";

import { LiveList } from "../../../live/LiveList";
import { useLiveView } from "../liveView";
import { formatFreshness, formatMoment } from "../format";
import { RELEASE_REASON_BLURB, workspacesByNode, type WorkspaceRow } from "../rows";
import { Button, Fact, Facts, FleetError, Panel, SectionHead } from "../ui";
import { useNow } from "../useNow";
import { useWorkbenches } from "./useWorkbenches";

// Workbenches: the replicas that host per-plan working directories, and the
// directories on them.
//
// A workspace is a FILESYSTEM on ONE replica's disk (memql#4354), which is
// why the replica is the organising fact rather than a column. When a replica
// leaves the mesh its directories go with it -- they are not migrated -- and
// the plan is given a fresh workspace elsewhere. That is a design decision
// rather than a recovery failure, and this screen is where an operator finds
// it out.

export function WorkbenchesSection() {
  const state = useWorkbenches();
  const [showReleased, setShowReleased] = useState(false);
  const now = useNow(30_000);

  // Ordered by replica, then newest first inside each -- so the flat list the
  // LiveList renders reads as the grouping the headers draw. Sorting in the
  // VIEW rather than at render is what lets one LiveList own the whole list:
  // its state caption, its arrival cues and its empty text all describe the
  // same feed, which two lists could not do.
  const view = useLiveView<WorkspaceRow>(
    state.source,
    `released:${showReleased}`,
    (rows) => {
      const visible = showReleased ? [...rows] : rows.filter((one) => one.status !== "released");
      return workspacesByNode(visible).flatMap((group) => group.workspaces);
    },
  );

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
  const emptyReplicas = state.nodes.filter((node) => !occupied.has(node.id));

  return (
    <div className="os-fleet">
      <SectionHead title="Workbenches">
        <label className="os-fleet-check">
          <input
            type="checkbox"
            checked={showReleased}
            onChange={(e) => setShowReleased(e.target.checked)}
          />
          <span>Show released</span>
        </label>
        <Button onClick={state.reseedWorkspaces}>Re-read</Button>
      </SectionHead>

      <p className="os-caption">
        A workspace is a sandboxed working directory for one plan, on one workbench replica's
        disk. Nothing on your own computer is touched.
      </p>

      <ReplicaPanel state={state} emptyReplicas={emptyReplicas} now={now} />

      {state.workspaceError ? (
        <FleetError
          sentence="The workspaces feed reported an error."
          detail={state.workspaceError}
        />
      ) : null}

      {/* Keyed on the toggle so revealing released rows re-baselines the
          arrival cues: without it they would flash "new" on the next
          workspace event, claiming the cluster just sent rows this browser
          already had. */}
      <LiveList<WorkspaceRow>
        key={`workspaces:${showReleased}`}
        source={view}
        rowId={(w) => w.id}
        fingerprint={(w) => `${w.status}|${w.releasedAt}|${w.releasedReason}|${w.lastUsedAt}`}
        label="Your workspaces"
        emptyText={
          showReleased
            ? "No workspaces. One is created the first time a plan of yours uses the workbench."
            : "No live workspaces. Released ones are hidden -- turn on Show released to see directories that have been torn down."
        }
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
  );
}

function ReplicaPanel({
  state,
  emptyReplicas,
  now,
}: {
  state: ReturnType<typeof useWorkbenches>;
  emptyReplicas: ReturnType<typeof useWorkbenches>["nodes"];
  now: Date;
}) {
  return (
    <Panel label="Workbench replicas">
      <div className="os-fleet-head">
        <h4 className="os-fleet-subhead">Replicas</h4>
        <div className="os-fleet-head-actions">
          <Button onClick={state.refreshNodes} busy={state.nodesLoading} busyLabel="Reading...">
            Refresh
          </Button>
        </div>
      </div>

      {state.nodesError ? (
        <FleetError
          sentence="The workbench replicas could not be read."
          next={
            state.nodes.length > 0
              ? "The replicas below are from the last successful read."
              : "Nothing was loaded."
          }
          detail={state.nodesError}
        />
      ) : null}

      {/* Three DIFFERENT answers, and this surface must not collapse them:
          "no replicas are deployed", "the read has not happened yet", and
          "the read failed". The workspaces LiveList below says the fourth --
          whether its own feed is live -- in its state caption. */}
      {state.nodes.length === 0 && !state.nodesLoading && state.nodesError === "" ? (
        <p className="os-caption">
          {state.nodesReadAt === null
            ? "The replica list has not been read yet."
            : "No workbench replicas are running in this cluster. Nothing can provision a workspace until one is."}
        </p>
      ) : null}

      <ul className="os-fleet-replicas">
        {state.nodes.map((node) => (
          <li key={node.id} className="os-fleet-replica">
            <span className="os-mono">{node.id}</span>
            <span className="os-caption">{node.health || "health not reported"}</span>
            <span className="os-caption">{formatFreshness(node.lastSeen, now)}</span>
            {emptyReplicas.some((one) => one.id === node.id) ? (
              <span className="os-caption">no workspaces</span>
            ) : null}
          </li>
        ))}
      </ul>

      {state.nodesReadAt === null ? null : (
        <p className="os-caption">
          Read at {formatMoment(state.nodesReadAt.toISOString())}. Cluster node rows are not
          broadcast to browsers, so this list refreshes on request rather than on its own -- the
          workspaces below are live.
        </p>
      )}
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
      <div className="os-fleet-workspace-body">
        <Facts>
          <Fact label="Plan" value={workspace.planId} mono />
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
        {released && blurb ? <p className="os-fleet-note">{blurb}</p> : null}
        {released && !blurb && reason !== "" ? (
          <p className="os-caption">
            Released for a reason this build does not have copy for: {reason}.
          </p>
        ) : null}
        {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
      </div>
    </div>
  );
}
