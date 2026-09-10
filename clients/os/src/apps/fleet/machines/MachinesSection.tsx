import { useEffect, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { MonitorSmartphone } from "lucide-react";

import type { OsAppProps } from "../../../system/registry";
import { LiveList } from "../../../live/LiveList";
import { useMachines } from "../../../live/machines";
import { ProvenanceDot } from "../../../kit";
import { AddMachinePage } from "../addMachine/AddMachinePage";
import type { AddMachineFlow } from "../addMachine/useAddMachineFlow";
import { useLiveView } from "../../../live/liveView";
import { formatFreshness } from "../../../kit/format";
import { isWorkerOnline } from "../online";
import { isRevoked, machineFromRow, machineName, type MachineRow } from "../rows";
import { Button, Chip, Chips, Head } from "../../../kit";
import { useNow } from "../../../kit/useNow";
import { MachineDetail } from "./MachineDetail";
import { useMachineWrites } from "./useMachineWrites";

// The machines directory: every machine the caller owns, live, with the four
// things an owner does to one -- name it, label it, revoke it, and look at
// what it is -- and the guided install that adds one (design record
// 2026-09-08-cockpit-install-wizard, D1): a PAGE that replaces this list
// while a flow is live, held by the Fleet app so it survives the window's own
// navigation.

export function MachinesSection({
  showRevoked,
  flow,
  intent,
  consumeIntent,
}: {
  showRevoked: boolean;
  /** The guided install's state, held by FleetApp (D7). */
  flow: AddMachineFlow;
  intent?: OsAppProps["intent"];
  consumeIntent?: OsAppProps["consumeIntent"];
}) {
  const { collection, settled } = useMachines();
  const writes = useMachineWrites();
  const [openId, setOpenId] = useState("");

  // ARRIVING BY INTENT OPENS THE GUIDED INSTALL (epic memql#5106). The
  // first-run wizard's fleet door sends somebody here to pair a machine that
  // will serve a model, and landing them on a list with an "Add machine"
  // button still to find is one step of the act left undone.
  //
  // CONSUMED BY ID, so acting on a stale render can never re-open the page
  // somebody has since closed -- the rule every intent in this shell follows.
  // PRESENCE OF THE OBJECT means "open the page"; `inference` inside it is a
  // separate question, so `{ addMachine: {} }` is a valid request that opens
  // it with nothing pre-selected. The shape is an OBJECT and not a boolean
  // specifically so a merely-truthy value cannot pre-select a
  // several-gigabyte download: `true`, `"yes"` and `1` are all malformed here
  // and open nothing. A flow already past its mint is left where it is: the
  // hook's start() refuses to throw a live credential away.
  const request = intent?.payload["addMachine"];
  const wants = typeof request === "object" && request !== null && !Array.isArray(request);
  const presetInference = wants && (request as { inference?: unknown }).inference === true;
  const start = flow.start;
  useEffect(() => {
    if (!intent || !wants) return;
    start({ inference: presetInference });
    consumeIntent?.(intent.id);
  }, [intent, wants, presetInference, consumeIntent, start]);
  // ONE clock for the section, ticking at the heartbeat cadence. Every
  // freshness reading and every online dot resolves against the same instant,
  // so two rows cannot disagree about what "now" is -- and a machine going
  // offline is noticed without an event, which matters because going offline
  // produces NO event: the row simply stops being bumped.
  const now = useNow(15_000);

  // PROJECT, then narrow -- in that order and in one pass. The collection
  // holds raw wire rows (see live/machines.tsx), so every predicate below has
  // to run on a machineFromRow result; `isRevoked` reading a raw row's absent
  // `revokedAt` is a throw, not a false.
  const source = useLiveView<Row, MachineRow>(collection, `revoked:${showRevoked}`, (rows) => {
    const machines = rows.map(machineFromRow).filter((m) => m.id !== "");
    return showRevoked ? machines : machines.filter((m) => !isRevoked(m));
  });

  // THE PAGE REPLACES THE LIST (interface rule 11). Two Heads in one scroller
  // is the tell that neither a page nor a list happened; the guided install
  // takes the whole section and hands back to the list through its own bar.
  if (flow.active) {
    return (
      <AddMachinePage
        flow={flow}
        onLeave={(openMachineId) => {
          if (openMachineId !== "") setOpenId(openMachineId);
        }}
      />
    );
  }

  return (
    <div className="os-fleet">
      <Head title="Machines">
        <Button tone="primary" onClick={() => flow.start({})} ariaLabel="Add a machine">
          Add machine
        </Button>
      </Head>

      {/* Keyed on the filter so flipping the toggle RE-BASELINES the arrival
          cues. Without it, revealing revoked rows makes them flash "new" on
          the next heartbeat from any machine -- which claims the cluster just
          sent them, when all that happened is that this browser started
          showing rows it already had. */}
      <LiveList<MachineRow>
        key={`machines:${showRevoked}`}
        source={source}
        rowId={(m) => m.id}
        // A HEARTBEAT IS NOT NEWS, and this is the line that decides it.
        //
        // The fingerprint drives the arrival cue, so anything named here
        // announces itself when it changes. `lastSeenAt` moves every 15
        // seconds for every machine, forever -- naming it made the whole list
        // pulse on a timer, which is precisely the standing badge this cue is
        // supposed not to be. Liveness already has a continuous display, the
        // dot, and it needs no cue on top of it.
        //
        // What is left is what a person would call a change to a machine:
        // its name, its labels, whether it was revoked, whether it picked up
        // work.
        fingerprint={(m) =>
          `${m.revokedAt}|${m.displayName}|${m.activeCount}|${JSON.stringify(m.operatorLabels)}`
        }
        label="Your machines"
        emptyText={
          !settled
            ? "Loading your machines…"
            : showRevoked
              ? "No machines yet for this signed-in account. Add one to pair a computer you own."
              : "No active machines for this signed-in account. Add one to pair a computer you own -- or turn on revoked machines in this app's settings if you are looking for one you retired. If you paired under a different sign-in, switch accounts or re-pair here."
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

      {openId === "" ? null : (
        <DetailFor id={openId} source={source} writes={writes} now={now} />
      )}
    </div>
  );
}

function DetailFor({
  id,
  source,
  writes,
  now,
}: {
  id: string;
  source: ReturnType<typeof useLiveView<Row, MachineRow>>;
  writes: ReturnType<typeof useMachineWrites>;
  now: Date;
}) {
  // Read the machine out of the same VIEW the list renders, rather than
  // holding the row the list handed us: the detail panel has to show what the
  // feed is showing, and a captured row would go stale the moment its next
  // heartbeat lands -- the panel would render a "last heartbeat" that stopped
  // advancing while the list beside it kept moving.
  //
  // The view rather than the collection, because the collection's rows are
  // raw and this panel reads derived fields on every line.
  const machine = useMemo(
    () => source?.snapshot.rows.find((m) => m.id === id),
    [source, source?.snapshot, id],
  );
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
        <Chips label={`Labels on ${machineName(machine)}`}>
          {labels.slice(0, 4).map((one) => (
            <Chip
              key={one.key}
              tone={one.source === "operator" ? "accent" : "muted"}
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
        </Chips>
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
