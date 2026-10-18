import { CopyValue, Fact, Facts, DetailsSection } from "../../../kit";
import { formatFreshness, formatMoment } from "../../../kit/format";
import { PageHelp } from "../../../kit/pageHelp";
import { roundTripFigure } from "../addMachine/flow";
import { isWorkerOnline } from "../online";
import { computerUseStatus, formatClockOffset, hasRoundTrip, pipelineStepsStatus, type MachineRow } from "../rows";
import { HardwareGroup } from "./HardwareGroup";
import { setupNow, setupSummary } from "./setup";

export const MachineDetailsSection = DetailsSection;

/** Read-only information stays open, ordered and separate from machine edits. */
export function MachineDetailsInfo({ machine, now, machineClass, usableBytes }: {
  machine: MachineRow; now: Date; machineClass: string; usableBytes: number;
}) {
  const status = machine.revokedAt ? "Revoked" : isWorkerOnline(machine, now) ? "Online" : "Offline";
  const roundTrip = hasRoundTrip(machine) ? roundTripFigure(machine, now) : "Not measured";
  const clock = machine.clockSkewKnown ? formatClockOffset(machine.clockSkewMs) : "Not reported";
  const computerUse = computerUseStatus(machine).answer;
  const pipelines = pipelineStepsStatus(machine).answer;
  // What the installer is doing, while it is (memql#6229): the same words the
  // add-machine wizard shows, and nothing once it has finished.
  const setup = setupSummary(setupNow(machine.setup, now));
  const reportedKeys = Object.keys(machine.reportedLabels).sort();
  return <>
    <PageHelp title="Machine information"><p>Use the copy icon beside a value to copy it. Text can also be selected normally. Relative dates copy their exact timestamps.</p></PageHelp>
    <MachineDetailsSection title="System">
      <Facts>
        <Fact label="Reported name" value={machine.name} copyValue={machine.name} />
        <Fact label="Hostname" value={machine.hostname} copyValue={machine.hostname} />
        <Fact label="Platform" value={machine.platform} copyValue={machine.platform} />
      </Facts>
    </MachineDetailsSection>
    <MachineDetailsSection title="Hardware">
      <HardwareGroup machine={machine} machineClass={machineClass} usableBytes={usableBytes} now={now} heading={false} />
    </MachineDetailsSection>
    <MachineDetailsSection title="Connection">
      <PageHelp title="Connection"><p>Online means Cockpit is connected. Active calls reflect the last heartbeat and may trail newly started work. Round trip measures the cluster’s ping to this machine. A missing ping or clock offset means Cockpit has not reported that measurement; it does not mean zero.</p></PageHelp>
      <Facts>
        <Fact label="Status" value={status} copyValue={status} />
        {setup ? <Fact label="Setup" value={setup} copyValue={setup} /> : null}
        <Fact label="Last heartbeat" value={formatFreshness(machine.lastSeenAt, now)} title={machine.lastSeenAt || undefined} copyValue={machine.lastSeenAt} />
        <Fact label="Round trip" value={roundTrip} title={machine.rttAt || undefined} copyValue={hasRoundTrip(machine) ? roundTrip : undefined} />
        <Fact label="Clock offset" value={clock} copyValue={machine.clockSkewKnown ? clock : undefined} />
        <Fact label="Active calls" value={String(machine.activeCount)} copyValue={String(machine.activeCount)} />
      </Facts>
    </MachineDetailsSection>
    <MachineDetailsSection title="Permissions">
      <PageHelp title="Pipeline steps">
        <p>Set in the <code>pipelines</code> block of this machine&apos;s Cockpit <code>policy.yaml</code>.</p>
        <p>Setting <code>allow: true</code> lets it run pipeline steps, and its <code>repos</code> list names the repositories it accepts. An empty list accepts any.</p>
        <p>Labels you set in Fleet cannot allow it.</p>
      </PageHelp>
      <Facts>
        <Fact label="Display server" value={machine.displayServer} copyValue={machine.displayServer} />
        <Fact label="Computer use" value={computerUse} copyValue={computerUse} />
        <Fact label="Pipeline steps" value={pipelines} copyValue={pipelines} />
      </Facts>
    </MachineDetailsSection>
    <MachineDetailsSection title="Registration">
      <Facts>
        <Fact label="Registration id" value={machine.id} copyValue={machine.id} mono />
        <Fact label="Registered" value={formatMoment(machine.registeredAt)} copyValue={machine.registeredAt} />
        <Fact label="Cockpit version" value={machine.version} copyValue={machine.version} />
        <Fact label="Build" value={machine.buildTag} copyValue={machine.buildTag} mono />
        <Fact label="Credential expires" value={machine.credentialExpiresAt ? formatMoment(machine.credentialExpiresAt) : "Does not expire"} title={machine.credentialExpiresAt || undefined} copyValue={machine.credentialExpiresAt} />
      </Facts>
    </MachineDetailsSection>
    <MachineDetailsSection title="Reported labels" label="Machine-reported data">
      <PageHelp title="Reported labels"><p>These read-only labels are replaced whenever Cockpit connects. Operator labels in Machine settings override matching values for routing. Copying a reported label includes its key and value.</p></PageHelp>
      {reportedKeys.length ? <ul className="fleet-details-labels" aria-label="Reported labels">
        {reportedKeys.map(key => <li key={key}>
          <CopyValue label={`${key} label`} value={`${key}=${machine.reportedLabels[key] ?? ""}`} presentation="row"
            displayValue={<span className="fleet-details-label-value"><span>{key}</span><span>{machine.reportedLabels[key] || "--"}{machine.operatorLabels[key] !== undefined ? <small>overridden</small> : null}</span></span>} />
        </li>)}
      </ul> : <p className="os-caption">None reported.</p>}
    </MachineDetailsSection>
  </>;
}
