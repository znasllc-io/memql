import { ContentSkeleton } from "../../../kit/ContentSkeleton";
import { useCallback, useMemo, useSyncExternalStore } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { Monitor } from "lucide-react";
import { useSession } from "../../../chrome/access";
import { EmptyState, Notice, RefreshButton, useNow } from "../../../kit";
import { Overview, OverviewBreakdown } from "../../../kit/Overview";
import { MapControls } from "../../../kit/MapControls";
import { MapHeading } from "../../../kit/MapHeading";
import { absent, figureOf } from "../../../kit/measure";
import { usePanZoom } from "../../../kit/usePanZoom";
import { transformOf } from "../../../kit/viewport";
import { useLiveView } from "../../../live/liveView";
import { useMachines } from "../../../live/machines";
import { isWorkerOnline } from "../online";
import { isRevoked, machineFromRow, machineName, type MachineRow } from "../rows";

export function FleetOverview({ onOpenMachine }: { onOpenMachine: (id: string) => void }) {
  const { collection, settled, feedState, reload } = useMachines();
  const { config } = useSession();
  const now = useNow(15_000);
  const source = useLiveView<Row, MachineRow>(collection, "overview", rows => rows.map(machineFromRow).filter(m => m.id && !isRevoked(m)));
  const subscribe = useCallback((listener: () => void) => source?.subscribe(listener) ?? (() => {}), [source]);
  const snapshot = useSyncExternalStore(subscribe, () => source?.snapshot ?? null, () => null);
  const machines = snapshot?.rows ?? [];
  const fresh = settled && feedState === "live" && !snapshot?.error;
  const online = machines.filter(m => isWorkerOnline(m, now));
  const count = (n: number) => fresh ? figureOf(n) : absent(snapshot?.error ? "failed" : "unread");
  const behind = feedState === "degraded" || feedState === "disconnected";
  return <Overview scope={config.domain || "This cluster"} actions={<RefreshButton label="Refresh overview" onClick={reload} />} metrics={[
    { label: "Machines", figure: count(machines.length) },
    { label: "Online", figure: count(online.length) },
    { label: "Active calls", figure: count(online.reduce((sum, m) => sum + Math.max(0, m.activeCount), 0)) },
    { label: "Reported apps", figure: count(machines.reduce((sum, m) => sum + m.apps.length, 0)) },
  ]}>
    {behind || snapshot?.error ? <Notice tone="warn" sentence="Machine updates are interrupted." next="Showing the last reported connections. Refresh to reconnect." /> : null}
    {!machines.length && !fresh && !snapshot?.error ? <ContentSkeleton kind="map" label="Loading fleet connections" /> : !machines.length ? <EmptyState icon={Monitor} title={fresh ? "No machines connected" : "Machines unavailable"}>{fresh ? "Add a machine from Machines to see its connection here." : "Reconnect to read this cluster’s machine inventory."}</EmptyState> : <FleetMap machines={machines} now={now} cluster={config.domain || "This cluster"} fresh={fresh} onOpenMachine={onOpenMachine} />}
    {fresh ? <OverviewBreakdown title="Machine availability" segments={[
      { label: "Online", count: online.length, tone: "good" },
      { label: "Offline", count: machines.length - online.length, tone: "quiet" },
    ]} /> : null}
  </Overview>;
}

function FleetMap({ machines, now, cluster, fresh, onOpenMachine }: { machines: readonly MachineRow[]; now: Date; cluster: string; fresh: boolean; onOpenMachine: (id: string) => void }) {
  const ordered = useMemo(() => [...machines].sort((a, b) => machineName(a).localeCompare(machineName(b)) || a.id.localeCompare(b.id)), [machines]);
  const height = Math.max(200, ordered.length * 100 + 40);
  const pan = usePanZoom({ width: 620, height, ready: true, wheelZoom: false, resetToFit: true });
  return <div className="os-deploy-map" data-behind={!fresh || undefined}>
    <MapHeading title="Connection map">
      <p>Machines visible to your account in this cluster. A solid line means an active connection with a recent heartbeat. A dashed line means the machine is registered but offline.</p>
      <p>Activity is the latest reported count of calls on online machines. Other clusters are not included in this cluster’s inventory.</p>
    </MapHeading>
    <div className="os-map-frame" ref={pan.frameRef}>
      <svg className="os-deploy-map-canvas" role="application" aria-label="Fleet connection map" tabIndex={0} {...pan.handlers}>
        <g data-os-map-view transform={transformOf(pan.view)}>
          {ordered.map((machine, index) => {
            const y = (height - ordered.length * 100) / 2 + index * 100;
            const online = fresh && isWorkerOnline(machine, now);
            return <path key={machine.id} className="fleet-map-connection" data-online={online || undefined} d={`M 242 ${height / 2} C 300 ${height / 2}, 300 ${y + 32}, 350 ${y + 32}`} />;
          })}
          <g className="fleet-map-node" data-cluster transform={`translate(24 ${height / 2 - 36})`} role="img" aria-label={`Cluster ${cluster}`}>
            <rect width={218} height={72} rx={9} />
            <text className="fleet-map-status" x={16} y={24}>Cluster</text>
            <text className="fleet-map-name" x={16} y={47}>{shortName(cluster, 25)}</text>
            <title>{cluster}</title>
          </g>
          {ordered.map((machine, index) => {
            const name = machineName(machine);
            const y = (height - ordered.length * 100) / 2 + index * 100;
            const online = fresh && isWorkerOnline(machine, now);
            const state = !fresh ? "Unknown" : online ? "Online" : "Offline";
            return <g key={machine.id} className="fleet-map-node" transform={`translate(350 ${y})`} role="button" tabIndex={0} aria-label={`Open ${name}, ${state.toLowerCase()}`} onClick={() => { if (!pan.steering()) onOpenMachine(machine.id); }} onKeyDown={event => {
              if (event.key === "Enter" || event.key === " ") { event.preventDefault(); event.stopPropagation(); onOpenMachine(machine.id); }
            }}>
              <rect width={246} height={64} rx={9} />
              <circle className="fleet-map-dot" data-online={online || undefined} cx={18} cy={22} r={4} />
              <text className="fleet-map-name" x={30} y={26}>{shortName(name, 25)}</text>
              <text className="fleet-map-status" x={16} y={46}>{state}{machine.os ? ` · ${machine.os}` : ""}</text>
              <title>{name} · {state}</title>
            </g>;
          })}
        </g>
      </svg>
      <MapControls zoomIn={pan.zoomIn} zoomOut={pan.zoomOut} reset={pan.reset} />
    </div>
  </div>;
}

function shortName(value: string, length: number): string { return value.length > length ? value.slice(0, length - 1) + "…" : value; }
