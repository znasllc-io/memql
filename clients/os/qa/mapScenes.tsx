import { useEffect, useState, type ComponentType, type ReactNode } from "react";
import { useOs } from "../src/chrome/state";
import { resolveThemePack } from "../src/themes/registry";
import { FleetOverview } from "../src/apps/fleet/overview/FleetOverview";
import { DeployMap } from "../src/apps/deployables/map/DeployMap";
import { siteFromRow } from "../src/apps/deployables/rows";
import { BeaconMap } from "../src/apps/nexus/BeaconMap";
import { AutomationMap } from "../src/apps/cluster/automations/AutomationMap";
import { layout, type Automation } from "../src/apps/cluster/automations/layout";
import { fanOutWorld, world } from "../src/nexus/scene/fixtures";
import { MachinesProvider } from "../src/live/machines";
import { DOCS, SHOP, withSession } from "../test/deployables/harness";
import { fakeConnection, machineRow } from "../test/fleet/harness";

type Frame = ComponentType<{ fallback: string; children: ReactNode }>;
type Kind = "fleet" | "deployables" | "nexus" | "automations";

export function mapScenes(Frame: Frame) {
  const scene = (kind: Kind, empty = false, behind = false) => ({
    connect: () => fakeConnection({ myWorkersWithStatus: empty ? [] : [
      machineRow({ id: "studio", displayName: "Studio Mac", lastSeenAt: "" }),
      machineRow({ id: "builder", displayName: "Build server", connectedNodeId: "", platformInfo: { os: "linux", arch: "amd64" } }),
      machineRow({ id: "laptop", displayName: "Travel laptop", lastSeenAt: "" }),
    ] }),
    wrap: (element: JSX.Element, role: string) => withSession(<MachinesProvider>{element}</MachinesProvider>, { role }),
    render: () => <Frame fallback={kind === "fleet" ? "Overview" : kind === "automations" ? "Automations" : "Map"}><MapScene kind={kind} empty={empty} behind={behind} /></Frame>,
  });
  return {
    "map-fleet": scene("fleet"), "map-fleet-empty": scene("fleet", true),
    "map-deployables": scene("deployables"), "map-deployables-empty": scene("deployables", true), "map-deployables-behind": scene("deployables", false, true),
    "map-nexus": scene("nexus"), "map-nexus-empty": scene("nexus", true), "map-nexus-behind": scene("nexus", false, true),
    "map-automations": scene("automations"),
  };
}

const automationRows: Automation[] = Array.from({ length: 3 }, (_, index) => ({
  name: ["receiveItem", "checkItem", "recordItem"][index]!, stratum: index,
  writes: [], publishes: [], opaque: [],
  edgesOut: index < 2 ? [{ to: ["checkItem", "recordItem"][index]!, concept: "v1:items:item", decided: true, reason: "Reacts to the written item." }] : [],
}));

function MapScene({ kind, empty, behind }: { kind: Kind; empty: boolean; behind: boolean }) {
  const { actions } = useOs();
  useEffect(() => { actions.setThemePack(resolveThemePack(new URLSearchParams(window.location.search).get("theme") ?? "graphite").id); }, [actions]);
  const [selected, select] = useState("");
  const [columns, setColumns] = useState(new Set<number>());
  const [folds, setFolds] = useState(new Set<number>());
  const toggle = (held: Set<number>, depth: number) => { const next = new Set(held); if (!next.delete(depth)) next.add(depth); return next; };
  const state = behind ? "degraded" : "live";
  return <div className="os-app-stack">
    {kind === "fleet" ? <FleetOverview onOpenMachine={select} /> :
      kind === "deployables" ? <DeployMap sites={empty ? [] : [SHOP, DOCS].map(siteFromRow)} ticks={new Map()} state={state} selectedNodeId={selected} onSelect={node => select(node.id)} /> :
      kind === "automations" ? <AutomationMap map={layout(automationRows)} selected={selected} select={select} /> :
      <BeaconMap world={empty ? world({ goal: null }) : fanOutWorld(3)} state={state} selectedStepKey={selected} onSelectStep={select} onOpenApproval={select} expandedColumns={columns} expandedFolds={folds} onToggleColumn={depth => setColumns(held => toggle(held, depth))} onToggleFold={depth => setFolds(held => toggle(held, depth))} at="" />}
    {selected ? <output aria-label="Selected map node">{selected}</output> : null}
  </div>;
}
