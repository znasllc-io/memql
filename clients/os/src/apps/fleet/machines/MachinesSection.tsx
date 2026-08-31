import { useState } from "react";
import { MonitorSmartphone } from "lucide-react";

import { LiveList } from "../../../live/LiveList";
import { useMachines } from "../../../live/machines";
import { ProvenanceDot } from "../../../kit";
import { AddMachine } from "../addMachine/AddMachine";
import { useLiveView } from "../liveView";
import { formatFreshness } from "../format";
import { isWorkerOnline } from "../online";
import { isRevoked, machineName, type MachineRow } from "../rows";
import { Button, Chip, ChipRow, SectionHead } from "../ui";
import { useNow } from "../useNow";
import { MachineDetail } from "./MachineDetail";
import { useMachineWrites } from "./useMachineWrites";

// The machines directory: every machine the caller owns, live, with the four
// things an owner does to one -- name it, label it, revoke it, and look at
// what it is.

export function MachinesSection({ showRevoked }: { showRevoked: boolean }) {
  const { collection, count } = useMachines();
  const writes = useMachineWrites();
  const [openId, setOpenId] = useState("");
  const [adding, setAdding] = useState(false);
  // ONE clock for the section, ticking at the heartbeat cadence. Every
  // freshness reading and every online dot resolves against the same instant,
  // so two rows cannot disagree about what "now" is -- and a machine going
  // offline is noticed without an event, which matters because going offline
  // produces NO event: the row simply stops being bumped.
  const now = useNow(15_000);

  const source = useLiveView<MachineRow>(collection, `revoked:${showRevoked}`, (rows) =>
    showRevoked ? [...rows] : rows.filter((m) => !isRevoked(m)),
  );

  return (
    <div className="os-fleet">
      <SectionHead title="Machines">
        <Button
          tone={adding ? "quiet" : "primary"}
          onClick={() => setAdding((v) => !v)}
          ariaLabel="Add a machine"
        >
          {adding ? "Close" : "Add machine"}
        </Button>
      </SectionHead>

      {adding ? <AddMachine machineCount={count} onClose={() => setAdding(false)} /> : null}

      {/* Keyed on the filter so flipping the toggle RE-BASELINES the arrival
          cues. Without it, revealing revoked rows makes them flash "new" on
          the next heartbeat from any machine -- which claims the cluster just
          sent them, when all that happened is that this browser started
          showing rows it already had. */}
      <LiveList<MachineRow>
        key={`machines:${showRevoked}`}
        source={source}
        rowId={(m) => m.id}
        fingerprint={(m) =>
          `${m.lastSeenAt}|${m.revokedAt}|${m.displayName}|${m.activeCount}|${JSON.stringify(m.operatorLabels)}`
        }
        label="Your machines"
        emptyText={
          showRevoked
            ? "No machines yet. Add one to pair a computer you own."
            : "No active machines. Add one to pair a computer you own -- or turn on revoked machines in this app's settings if you are looking for one you retired."
        }
        renderRow={(m, tick) => (
          <MachineLine
            machine={m}
            tick={tick}
            now={now}
            open={openId === m.id}
            onToggle={() => setOpenId((held) => (held === m.id ? "" : m.id))}
          />
        )}
      />

      {openId === "" ? null : <DetailFor id={openId} writes={writes} now={now} />}
    </div>
  );
}

function DetailFor({
  id,
  writes,
  now,
}: {
  id: string;
  writes: ReturnType<typeof useMachineWrites>;
  now: Date;
}) {
  const { collection } = useMachines();
  // Read the machine out of the SNAPSHOT rather than holding the row the list
  // handed us: the detail panel has to show what the feed is showing, and a
  // captured row would go stale the moment its next heartbeat lands -- the
  // panel would render a "last heartbeat" that stopped advancing while the
  // list beside it kept moving.
  const machine = collection?.snapshot.rows.find((m) => m.id === id);
  if (!machine) return null;
  return <MachineDetail machine={machine} writes={writes} now={now} />;
}

function MachineLine({
  machine,
  tick,
  now,
  open,
  onToggle,
}: {
  machine: MachineRow;
  tick: "added" | "updated" | null;
  now: Date;
  open: boolean;
  onToggle: () => void;
}) {
  const revoked = isRevoked(machine);
  // A revoked machine is NEVER online, whatever its last heartbeat says:
  // isWorkerOnline refuses on `revokedAt` before it looks at the clock. The
  // dot follows that, so a machine retired thirty seconds ago does not sit
  // there green.
  const online = isWorkerOnline(machine, now);
  const labels = machine.mergedLabels;

  return (
    <button
      type="button"
      className="os-machine"
      data-online={online || undefined}
      data-revoked={revoked || undefined}
      aria-expanded={open}
      onClick={onToggle}
    >
      <MonitorSmartphone size={16} aria-hidden />
      <span className="os-machine-name">{machineName(machine)}</span>
      {machine.platform ? <span className="os-caption">{machine.platform}</span> : null}
      {labels.length > 0 ? (
        <ChipRow label={`Labels on ${machineName(machine)}`}>
          {labels.slice(0, 4).map((one) => (
            <Chip
              key={one.key}
              tone={one.source === "operator" ? "operator" : "reported"}
              title={
                one.overrides
                  ? "You set this, replacing the value the machine reported"
                  : one.source === "operator"
                    ? "You set this"
                    : "Reported by the machine"
              }
            >
              {`${one.key}=${one.value}`}
            </Chip>
          ))}
          {labels.length > 4 ? <span className="os-caption">+{labels.length - 4}</span> : null}
        </ChipRow>
      ) : null}
      <span className="os-machine-state">
        <span className="os-caption">{formatFreshness(machine.lastSeenAt, now)}</span>
        {revoked ? (
          <span className="os-fleet-revoked-tag">revoked</span>
        ) : (
          <ProvenanceDot
            tone={online ? "reachable" : "unreachable"}
            label={online ? "Online" : "Offline"}
          />
        )}
        {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
      </span>
    </button>
  );
}
