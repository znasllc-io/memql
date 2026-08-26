import { useState, type ReactNode } from "react";

import {
  Badge,
  Band,
  Button,
  Callout,
  ConfirmDialog,
  Container,
  DataText,
  EmptyState,
  ErrorNotice,
  Panel,
  Select,
  Skeleton,
  type StatusTone,
} from "../ui";
import { FleetFrame, LiveDegraded } from "./FleetFrame";
import { formatFreshness, formatMoment } from "./format";
import { chipsFromMap } from "./labels";
import { RELEASE_REASON_BLURB, type Workspace } from "./rows";
import { fleetSurfaceById } from "./urls";
import { useNow } from "./useNow";
import { useWorkbenches } from "./useWorkbenches";

// /fleet/workbenches (memql#4356): the cluster's own sandboxed working
// directories -- the replicas that host them, and the per-plan workspaces
// living on each.
//
// ===========================================================================
// WHY THE NODE SECTION EXISTS AT ALL
// ===========================================================================
// A workspace is a DIRECTORY ON A PARTICULAR REPLICA'S DISK, and the files are
// not migrated when that replica goes away -- the design accepts a fresh
// directory and records why (releasedReason=node_lost, memql#4354). That makes
// "which replica" a first-class fact rather than an implementation detail, and
// a page listing workspaces without listing the machines under them could not
// answer the one question an operator brings here: why did this plan lose its
// files, or why is this node full.
//
// ===========================================================================
// WHAT THIS PAGE CANNOT SAY, STATED PLAINLY
// ===========================================================================
// v1:cluster:node declares no capacity field -- no disk figure, no workspace
// cap, no quota. So "capacity" here is the count of workspaces this page has
// LOADED that name the node, and it is captioned as exactly that rather than
// as a fill level. Any node label shaped like a capacity is shown beside it,
// because an operator who set one meant it to be read.

const HEALTH_TONE: Record<string, StatusTone> = {
  healthy: "ok",
  connecting: "warn",
  degraded: "warn",
  draining: "warn",
  offline: "danger",
  stopped: "danger",
};

const STATUS_TONE: Record<string, StatusTone> = {
  provisioned: "ok",
  released: "neutral",
};

export function WorkbenchesPage(): ReactNode {
  const state = useWorkbenches();
  const now = useNow();
  const [confirming, setConfirming] = useState<Workspace | null>(null);

  const surface = fleetSurfaceById("workbenches");
  if (surface === undefined) return null;

  const workspacesEmpty =
    !state.workspacesLoading && state.workspacesError === "" && state.workspaces.length === 0;

  return (
    <Container>
      <FleetFrame
        surface={surface}
        actions={
          <>
            {state.isClusterOwner ? (
              <Select
                value={state.scope}
                onChange={(next) => state.setScope(next === "all" ? "all" : "mine")}
                ariaLabel="Whose workspaces"
              >
                <option value="mine">My workspaces</option>
                <option value="all">Every workspace in this cluster</option>
              </Select>
            ) : null}
            <Button
              pressed={state.showReleased}
              onClick={() => state.setShowReleased(!state.showReleased)}
            >
              {state.showReleased ? "Hide released" : "Show released"}
            </Button>
            <Button onClick={state.reload}>Reload</Button>
          </>
        }
      >
        <LiveDegraded reason={state.liveDegraded} noun="workspace" />

        {state.actionError === "" ? null : (
          <Callout tone="danger" title="That did not work">
            {state.actionError}
          </Callout>
        )}

        <Band
          title="Replicas"
          meta={
            state.nodesLoading
              ? "Loading…"
              : `${state.nodes.length} ${state.nodes.length === 1 ? "replica" : "replicas"}`
          }
        >
          {state.nodesError !== "" ? (
            <Callout tone="danger" title="Could not read the cluster's nodes">
              {state.nodesError}
            </Callout>
          ) : state.nodesLoading && state.nodes.length === 0 ? (
            <Skeleton variant="rows" rows={2} />
          ) : state.nodes.length === 0 ? (
            <EmptyState
              firstRun
              statement={
                "No workbench is running. Until one is, an agent that needs to write a file or " +
                "run a command has nowhere sandboxed to do it, and those calls are refused " +
                "rather than run somewhere they should not be."
              }
            />
          ) : (
            <ul className="flex flex-col gap-2">
              {state.nodes.map((node) => {
                const here = state.workspaces.filter((one) => one.nodeId === node.id);
                const live = here.filter((one) => one.status === "provisioned").length;
                const labels = chipsFromMap(node.labels);
                return (
                  <li key={node.id}>
                    <Panel>
                      <div className="flex flex-wrap items-center gap-2">
                        <Badge tone={HEALTH_TONE[node.health] ?? "neutral"}>
                          {node.health || "unknown"}
                        </Badge>
                        {/* The id in the DATA voice rather than as a bold
                            sans heading: it is the replica's address, which is
                            a value an operator reads character by character
                            and pastes elsewhere -- not a title. */}
                        <DataText kind="id">{node.id}</DataText>
                        <span className="ml-auto text-xs text-subtle">
                          last seen{" "}
                          <DataText kind="time">{formatFreshness(node.lastSeen, now)}</DataText>
                        </span>
                      </div>
                      <dl className="mt-2 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-0.5 text-xs sm:grid-cols-[max-content_1fr_max-content_1fr]">
                        <dt className="text-subtle">Address</dt>
                        <dd className="min-w-0 break-all text-muted">
                          <DataText kind="id">{node.address || "--"}</DataText>
                        </dd>
                        <dt className="text-subtle">Region</dt>
                        <dd className="text-muted">
                          {[node.provider, node.region].filter((one) => one !== "").join(" / ") ||
                            "--"}
                        </dd>
                        <dt className="text-subtle">Workspaces here</dt>
                        <dd className="text-muted">
                          <DataText kind="number">{live}</DataText> live
                          {here.length === live ? "" : `, ${here.length - live} released`}
                          <span className="text-subtle">
                            {" "}
                            -- of the ones listed below, not a cluster total
                          </span>
                        </dd>
                        <dt className="text-subtle">Labels</dt>
                        <dd className="min-w-0 break-words text-muted">
                          {labels.length === 0 ? "none" : labels.join(", ")}
                        </dd>
                      </dl>
                    </Panel>
                  </li>
                );
              })}
            </ul>
          )}
        </Band>

        <Band
          title={state.scope === "all" ? "Every workspace" : "Your workspaces"}
          meta={
            state.workspacesLoading
              ? "Loading…"
              : `${state.workspaces.length} loaded${state.showReleased ? ", released included" : ", live only"}`
          }
        >
          {state.workspacesError !== "" ? (
            <ErrorNotice
              sentence="Could not read the workspaces."
              next="Nothing is listed below. Do not read that as there being none -- this read failed, so the answer is unknown."
              detail={state.workspacesError}
            />
          ) : state.workspacesLoading && state.workspaces.length === 0 ? (
            <Skeleton variant="rows" rows={3} />
          ) : workspacesEmpty ? (
            <EmptyState
              statement={
                state.showReleased
                  ? "No workspace has been provisioned."
                  : "No live workspace. One is created the first time a plan asks the workbench to do something, and released when that plan finishes."
              }
            />
          ) : (
            <ul className="flex flex-col gap-2">
              {state.workspaces.map((workspace) => (
                <li key={workspace.id}>
                  <Panel>
                    <div className="flex flex-wrap items-center gap-2">
                      <Badge tone={STATUS_TONE[workspace.status] ?? "neutral"}>
                        {workspace.status || "unknown"}
                      </Badge>
                      <span className="text-sm font-semibold break-all">
                        <DataText kind="id">{workspace.planId || workspace.id}</DataText>
                      </span>
                      {state.isClusterOwner && workspace.status === "provisioned" ? (
                        <span className="ml-auto">
                          <Button
                            size="xs"
                            tone="danger"
                            busy={state.busyId === workspace.id}
                            busyLabel="Releasing…"
                            onClick={() => setConfirming(workspace)}
                          >
                            Release
                          </Button>
                        </span>
                      ) : null}
                    </div>
                    <dl className="mt-2 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-0.5 text-xs sm:grid-cols-[max-content_1fr_max-content_1fr]">
                      <dt className="text-subtle">Owner</dt>
                      <dd className="min-w-0 break-all text-muted">
                        <DataText kind="id">{workspace.ownerUserId || "--"}</DataText>
                      </dd>
                      <dt className="text-subtle">Node</dt>
                      <dd className="min-w-0 break-all text-muted">
                        <DataText kind="id">{workspace.nodeId || "not stamped"}</DataText>
                      </dd>
                      <dt className="text-subtle">Storage root</dt>
                      <dd className="min-w-0 break-all text-muted">
                        <DataText kind="id">{workspace.storageRoot || "--"}</DataText>
                      </dd>
                      <dt className="text-subtle">Last used</dt>
                      <dd className="text-muted">
                        <DataText kind="time">
                          {workspace.lastUsedAt === ""
                            ? "never"
                            : formatFreshness(workspace.lastUsedAt, now)}
                        </DataText>
                      </dd>
                    </dl>
                    {workspace.status === "released" ? (
                      <p className="mt-2 text-xs text-muted">
                        Released {formatMoment(workspace.releasedAt)}
                        {workspace.releasedReason === "" ? "" : ` -- ${workspace.releasedReason}`}.{" "}
                        {RELEASE_REASON_BLURB[workspace.releasedReason] ?? ""}
                      </p>
                    ) : null}
                  </Panel>
                </li>
              ))}
            </ul>
          )}
        </Band>
      </FleetFrame>

      <ConfirmDialog
        open={confirming !== null}
        title="Release this workspace"
        confirmLabel="Release it"
        tone="danger"
        onCancel={() => setConfirming(null)}
        onConfirm={() => {
          const target = confirming;
          setConfirming(null);
          if (target !== null) state.release(target.id);
        }}
      >
        <p>
          The directory is torn down and everything in it goes with it. Whatever the plan wrote
          there -- files it produced, work in progress -- is not recoverable and is not moved
          anywhere first.
        </p>
        <p className="mt-2">
          The plan itself is untouched. If it is still running, its next workbench call
          provisions a fresh, empty directory.
        </p>
      </ConfirmDialog>
    </Container>
  );
}
