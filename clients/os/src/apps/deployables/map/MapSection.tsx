import type { LiveSnapshot } from "@znasllc-io/memql-sdk-core/client";
import { Button, Notice, EmptyState } from "../../../kit";
import { Overview, OverviewBreakdown, type OverviewSegment } from "../../../kit/Overview";
import { absent, figureOf } from "../../../kit/measure";
import type { ArrivalTick } from "../../../live/arrival";
import type { SiteRow } from "../rows";
import { siteStateWord } from "../words";
import { DeployMap } from "./DeployMap";
import type { MapNode } from "./layout";

export interface MapSelection { nodeId: string; siteIds: string[] }
export const NO_SELECTION: MapSelection = { nodeId: "", siteIds: [] };

/** Overview reads the same measured rows as the list; it never repeats the list. */
export function MapSection({ sites, snapshot, ticks, selection, onSelectNode, onOpenDeployable, onBrowse }: {
  sites: readonly SiteRow[];
  snapshot: LiveSnapshot<SiteRow>;
  ticks: Map<string, ArrivalTick>;
  selection: MapSelection;
  onSelectNode: (node: MapNode) => void;
  onOpenDeployable: (siteId: string) => void;
  onBrowse?: () => void;
}) {
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
      sites={current} ticks={ticks} state={snapshot.state} selectedNodeId={selection.nodeId}
      onSelect={node => {
        onSelectNode(node);
        if (node.siteIds.length === 1) onOpenDeployable(node.siteIds[0]!);
      }}
    />}
  </Overview>;
}
