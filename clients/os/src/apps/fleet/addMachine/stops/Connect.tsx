import { Caption, Fact, Facts } from "../../../../kit";
import { formatFreshness } from "../../../../kit/format";
import { machineName, type MachineRow } from "../../rows";
import { workerClusterUrl } from "../install";

// CONNECT: the cluster listens, and nothing needs reloading.
//
// While waiting the body is one line saying what it is listening for and, after
// a long wait, one more naming the usual cause and the log that says which.
// Once matched it is the machine's own account of itself.

export function ConnectStop({
  name,
  machine,
  waitedLong,
  domain,
  renameError,
  now,
}: {
  /** The name the person typed. */
  name: string;
  machine: MachineRow | null;
  waitedLong: boolean;
  domain: string;
  renameError: string;
  now: Date;
}) {
  if (machine === null) {
    const api = workerClusterUrl(domain) || "the cluster's API";
    return (
      <div className="os-stop-body os-fleet-addstop">
        <p className="os-status-line" role="status">
          Listening for {name.trim() === "" ? "the machine" : name.trim()}. This moves on by itself.
        </p>
        {waitedLong ? (
          <Caption>
            Taking a while? The machine may not reach {api}; its log says why: <code className="os-mono">~/.memql/state/worker.log</code>
          </Caption>
        ) : null}
      </div>
    );
  }

  return (
    <div className="os-stop-body os-fleet-addstop">
      <Facts>
        <Fact label="Name" value={machineName(machine)} copyValue={machineName(machine)} />
        <Fact label="Hostname" value={machine.hostname} mono copyValue={machine.hostname} />
        <Fact label="Platform" value={machine.platform} mono copyValue={machine.platform} />
        <Fact label="Cockpit" value={machine.version} mono copyValue={machine.version} />
        <Fact label="Registered" value={formatFreshness(machine.registeredAt, now)} title={machine.registeredAt || undefined} copyValue={machine.registeredAt || undefined} />
      </Facts>
      {renameError === "" ? null : <Caption>{renameError}</Caption>}
    </div>
  );
}
