import { useMemo } from "react";
import type { LiveSnapshot } from "@znasllc-io/memql-sdk-core/client";
import { Button, Notice, EmptyState } from "../../../kit";
import { Overview, OverviewBreakdown, type OverviewSegment } from "../../../kit/Overview";
import { absent, figureOf } from "../../../kit/measure";
import type { ArrivalTick } from "../../../live/arrival";
import type { SiteRow } from "../rows";
import { siteStateWord } from "../words";
import { DeployMap } from "./DeployMap";
import type { MapChecks, MapNode } from "./layout";
import type { PipelineRow, RunRow } from "../pipelines/rows";
import { checksBranchOf, lastRunPerBranch, outcomeKey, runsOfPipeline } from "../pipelines/runs";
import { runOutcome } from "../pipelines/words";

export interface MapSelection { nodeId: string; siteIds: string[] }
export const NO_SELECTION: MapSelection = { nodeId: "", siteIds: [] };

/**
 * What the map says about each checked source (D14, issue memql#5501): the
 * newest run on its DEFAULT branch, in words, keyed by the source's package id.
 *
 * ONLY AN ACTIVE PIPELINE DRAWS A NODE. A disconnected one opens no more runs,
 * so a node reading its last run would present history as the source's
 * current state; its runs stay on the source's page, where they are history.
 *
 * THE DEFAULT BRANCH, NOT THE NEWEST RUN ANYWHERE. A feature branch failing is
 * that branch's news, and the source's page lists it beside the others; the map
 * is a picture of what serves, and what serves is built from the default
 * branch. A source whose default branch has not run yet says so rather than
 * borrowing another branch's answer.
 */
export function mapChecksFor(pipelines: readonly PipelineRow[], runs: readonly RunRow[]): Map<string, MapChecks> {
  const out = new Map<string, MapChecks>();
  for (const pipeline of pipelines) {
    if (pipeline.status !== "active" || pipeline.packageId === "") continue;
    const own = runsOfPipeline(runs, pipeline.id);
    const branch = pipeline.defaultBranch;
    // `lastRunPerBranch` puts the default branch first, when it has a run.
    const first = lastRunPerBranch(own, branch)[0];
    const newest = first !== undefined && branch !== "" && checksBranchOf(first) === branch ? first : null;
    if (newest === null) {
      out.set(pipeline.packageId, {
        sublabel: own.length === 0 || branch === "" ? "No runs yet" : `No runs on ${branch} yet`,
        status: "",
        source: pipeline.repository,
      });
      continue;
    }
    out.set(pipeline.packageId, { sublabel: checksWords(newest, branch), status: outcomeKey(newest), source: pipeline.repository });
  }
  return out;
}

/** "Passed on main", "Failed on main, at tests": the outcome's own word, then where it stopped. */
function checksWords(run: RunRow, branch: string): string {
  const said = `${runOutcome(run).word} on ${branch}`;
  const at = outcomeKey(run) === "failed" ? run.stages.find((s) => s.status === "failed") : undefined;
  return at ? `${said}, at ${at.name}` : said;
}

/** Overview reads the same measured rows as the list; it never repeats the list. */
export function MapSection({ sites, snapshot, ticks, selection, onSelectNode, onOpenDeployable, onBrowse, pipelines = [], runs = [], onOpenSource }: {
  sites: readonly SiteRow[];
  snapshot: LiveSnapshot<SiteRow>;
  ticks: Map<string, ArrivalTick>;
  selection: MapSelection;
  onSelectNode: (node: MapNode) => void;
  onOpenDeployable: (siteId: string) => void;
  onBrowse?: () => void;
  /** The caller's pipelines and runs (epic memql#5479): a source with checks draws a Checks node. */
  pipelines?: readonly PipelineRow[];
  runs?: readonly RunRow[];
  /** Open a source's page: what selecting its Checks node does. */
  onOpenSource?: (packageId: string) => void;
}) {
  // KEYED BY WHAT IT SAYS. The feeds hand over a fresh array whenever the app
  // re-renders, and a map rebuilt from them would re-lay the picture every
  // time; keyed by its content, it changes only when a reading does.
  const checksKey = JSON.stringify([...mapChecksFor(pipelines, runs)]);
  const checks = useMemo(() => new Map<string, MapChecks>(JSON.parse(checksKey) as [string, MapChecks][]), [checksKey]);
  const current = sites.filter(site => site.status !== "archived");
  const fresh = snapshot.state === "live" && !snapshot.error;
  const count = (value: number) => fresh ? figureOf(value) : absent(snapshot.error ? "failed" : "unread");
  const states = new Map<string, number>();
  for (const site of current) { const word = siteStateWord(site); states.set(word, (states.get(word) ?? 0) + 1); }
  const segments: OverviewSegment[] = [...states].map(([label, value]) => ({ label, count: value, tone: label === "Live" ? "good" : label === "Unavailable" ? "warn" : "quiet" }));
  return <Overview metrics={[
    { label: "Deployables", figure: count(current.length) },
    { label: "Live", figure: count(states.get("Live") ?? 0) },
    { label: "Unavailable", figure: count(states.get("Unavailable") ?? 0) },
    { label: "Unknown", figure: count(states.get("Unknown") ?? 0) },
  ]}>
    {snapshot.error ? <Notice tone="error" sentence="Deployables could not be read." next="Reconnect to update this overview." /> : null}
    {fresh ? <OverviewBreakdown title="Deployment status" segments={segments} /> : null}
    {current.length === 0 && fresh ? <EmptyState title="No deployables to map yet" action={onBrowse ? <Button onClick={onBrowse}>Open deployables</Button> : undefined}>Add an app to see its address and source here.</EmptyState> : <DeployMap
      // A Checks node with nowhere to go would be a button that does nothing,
      // so without a way to the source page there is no node to choose.
      sites={current} checks={onOpenSource ? checks : undefined} ticks={ticks} state={snapshot.state} selectedNodeId={selection.nodeId}
      onSelect={node => {
        // A CHECKS NODE OPENS ITS SOURCE AND SELECTS NOTHING. It may stand
        // for exactly one deployable, and the selection is what opens a
        // deployable's page -- so it must not reach that path, and a source's
        // checks chosen here must not leave a deployable selected behind.
        if (node.kind === "checks") {
          if (node.packageId !== "") onOpenSource?.(node.packageId);
          return;
        }
        onSelectNode(node);
        if (node.siteIds.length === 1) onOpenDeployable(node.siteIds[0]!);
      }}
    />}
  </Overview>;
}
