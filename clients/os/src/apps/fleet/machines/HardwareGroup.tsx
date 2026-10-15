import { Monitor } from "lucide-react";
import { CopyValue, EmptyState, Fact, Facts, Subhead } from "../../../kit";
import { formatBytes, formatFreshness } from "../../../kit/format";
import {
  acceleratorSentence,
  classSentence,
  FLOOR_SENTENCE,
  NOT_REPORTED,
  runtimeLabel,
} from "./hardware";
import type { MachineRow } from "../rows";

// What this machine IS (epic memql#5146, D1).
//
// ===========================================================================
// THE CLASS SITS ABOVE THE FACTS BECAUSE IT IS THE ONE DERIVED VALUE
// ===========================================================================
// Everything in the Facts list below is something the machine SAID. The class
// is something the cluster CONCLUDED from what it said, and it is the only
// value here that no cockpit reports. Putting it in the list would make a
// conclusion look like a report, and a person reading a column of reported
// figures would have no way to tell which one the cluster had computed.
//
// So it is one line, in words, above the facts -- with the usable-memory
// figure that produced it, because a class with no working shown is a verdict.
//
// ===========================================================================
// THREE ABSENCES, THREE SENTENCES
// ===========================================================================
// NOT REPORTED    the cockpit predates the field. NOTHING IS WRONG, and the
//                 sentence says so, because the reader's question is whether
//                 something is.
// UNDER THE FLOOR the machine reported and the answer is no. The floor is
//                 named in the wizard's own words, and the facts stay on
//                 screen so a person can see the figure it was judged on.
// NO ACCELERATOR  a real state with its own sentence, and it is not "no GPU":
//                 the machine may have a card whose driver does not work, and
//                 that repair lives on the machine.
//
// A blank cell would collapse all three, and the three lead to different
// actions -- do nothing, buy hardware, fix a driver.

export function HardwareGroup({
  machine,
  machineClass,
  usableBytes,
  now,
  heading = true,
}: {
  machine: MachineRow;
  /** Computed by the engine and stored nowhere: "" when the cockpit has not
   *  reported, "unsupported" below the floor, otherwise the rung. */
  machineClass: string;
  usableBytes: number;
  now: Date;
  heading?: boolean;
}) {
  const hw = machine.hardware;

  if (!hw.present) {
    return (
      <div className="os-fleet-hardware">
        {heading ? <Subhead>Hardware</Subhead> : null}
        <EmptyState icon={Monitor} title="Hardware not reported">{NOT_REPORTED}</EmptyState>
      </div>
    );
  }

  const unsupported = machineClass === "unsupported";

  return (
    <div className="os-fleet-hardware">
      {heading ? <Subhead>Hardware</Subhead> : null}

      {machineClass ? <p className="os-fleet-class" data-unsupported={unsupported || undefined}>
        <CopyValue value={classSentence(machineClass, usableBytes)} label="Machine class" presentation="row" />
      </p> : null}
      {unsupported ? <p className="os-caption">{FLOOR_SENTENCE}</p> : null}

      <Facts>
        <Fact label="Chip" value={hw.chip} copyValue={hw.chip} />
        <Fact label="Memory" value={hw.memoryBytes > 0 ? formatBytes(hw.memoryBytes) : ""} copyValue={hw.memoryBytes > 0 ? formatBytes(hw.memoryBytes) : undefined} />
        <Fact label="Accelerator" value={acceleratorSentence(hw)} copyValue={acceleratorSentence(hw)} />
        <Fact label="Cores" value={hw.cpuCores > 0 ? String(hw.cpuCores) : ""} copyValue={hw.cpuCores > 0 ? String(hw.cpuCores) : undefined} />
        <Fact label="System" value={hw.osVersion} copyValue={hw.osVersion} />
        <Fact label="Free disk" value={hw.diskFreeBytes > 0 ? formatBytes(hw.diskFreeBytes) : ""} copyValue={hw.diskFreeBytes > 0 ? formatBytes(hw.diskFreeBytes) : undefined} />
        <Fact
          label="Reported"
          value={formatFreshness(hw.reportedAt, now)}
          title={hw.reportedAt || undefined}
          copyValue={hw.reportedAt}
        />
      </Facts>

      <RuntimeFacts runtimes={hw.runtimes} />
    </div>
  );
}

/** Runtime names stay individually readable and copy as one line per runtime. */
function RuntimeFacts({ runtimes }: { runtimes: { name: string; version: string }[] }) {
  // NOTHING, and rule 7 is why. The Models group below already says a machine
  // with no runtime cannot serve a model, and says how to install one -- so a
  // sentence here would be the same fact twice on one screen, and the browser
  // is where that became obvious: the two paragraphs sat four inches apart
  // saying the same thing in different words.
  //
  // The absence is not hidden. Hardware reports what the machine HAS, and the
  // chips being absent is that report; the repair belongs to the group that
  // owns the act.
  if (runtimes.length === 0) return null;
  return (
    <Facts><Fact label="Runtimes" copyValue={runtimes.map(r => [runtimeLabel(r.name), r.version].filter(Boolean).join(" ")).join("\n")}
      value={<span className="fleet-details-runtimes">{runtimes.map((r) => (
        <span key={r.name}>
          {runtimeLabel(r.name)}
          {r.version ? <span className="os-fleet-runtime-version">{r.version}</span> : null}
        </span>
      ))}</span>} /></Facts>
  );
}
