import { MonitorSmartphone } from "lucide-react";

import { LiveList } from "../../live/LiveList";
import { machineName, useMachines, type MachineRow } from "../../live/machines";
import { ProvenanceDot } from "../../kit";
import type { OsAppProps } from "../../system/registry";
import { isWorkerOnline } from "./online";

// The Fleet exemplar (spec D7): the foundation's proof that the live
// substrate works end to end -- a read-only machine list whose rows arrive
// with the LiveList cue while you watch. Everything else about Fleet
// (rename, labels, routing, workbenches) is epic #4729.

export function FleetApp({ sectionId }: OsAppProps) {
  if (sectionId === "settings") return <FleetSettingsStub />;
  return <MachinesSection />;
}

function MachinesSection() {
  const { collection } = useMachines();
  return (
    <div className="os-fleet">
      <h3 className="os-settings-title">Machines</h3>
      <LiveList<MachineRow>
        source={collection}
        rowId={(m) => m.id}
        fingerprint={(m) => `${m.lastSeenAt ?? ""}|${m.revokedAt ?? ""}|${machineName(m)}`}
        label="Your machines"
        emptyText="No machines yet. Pair one with the MemQL Cockpit on a computer you own."
        renderRow={(m, tick) => <MachineLine machine={m} tick={tick} />}
      />
      <p className="os-caption">
        Read-only in the foundation. Rename, labels, routing and workbenches arrive with epic
        #4729.
      </p>
    </div>
  );
}

function MachineLine({ machine, tick }: { machine: MachineRow; tick: "added" | "updated" | null }) {
  const online = isWorkerOnline(machine);
  const labels = { ...(machine.labels ?? {}), ...(machine.operatorLabels ?? {}) };
  const labelLine = Object.entries(labels)
    .map(([k, v]) => (v ? `${k}=${v}` : k))
    .join("  ");
  return (
    <div className="os-machine" data-online={online || undefined}>
      <MonitorSmartphone size={16} aria-hidden />
      <span className="os-machine-name">{machineName(machine)}</span>
      {machine.platform ? <span className="os-caption">{machine.platform}</span> : null}
      {labelLine ? <span className="os-caption os-mono">{labelLine}</span> : null}
      <span className="os-machine-state">
        <ProvenanceDot
          tone={online ? "reachable" : "unreachable"}
          label={online ? "Online" : "Offline"}
        />
        {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
      </span>
    </div>
  );
}

function FleetSettingsStub() {
  return (
    <div className="os-settings">
      <h3 className="os-settings-title">Fleet settings</h3>
      <p className="os-stub-summary">Routing policy and machine defaults arrive with epic #4729.</p>
    </div>
  );
}
