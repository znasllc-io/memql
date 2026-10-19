import { Caption, Fact, Facts } from "../../../../kit";
import { formatFreshness } from "../../../../kit/format";
import { machineName, type MachineRow } from "../../rows";
import { workerClusterUrl } from "../install";

// CONNECT: the cluster listens, and nothing needs reloading.
//
// While waiting the body says what it is listening for and, after a long
// wait, what usually went wrong -- three things, each of which a person can
// check on the machine, and the log that says which. Once matched it is the
// machine's own account of itself.

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
          Listening for {name.trim() === "" ? "the machine" : name.trim()}. The moment it registers
          with the token above, this moves on by itself -- there is nothing to reload.
        </p>
        {waitedLong ? (
          <>
            <Caption>This is taking a while. What usually went wrong:</Caption>
            <ul className="os-fleet-causes">
              <li>The line was pasted on a different machine, or not yet.</li>
              <li>The machine cannot reach {api} -- a firewall, a VPN, or a cluster URL with no scheme.</li>
              <li>The installer stopped at a prompt: the password, or a permission dialog.</li>
            </ul>
            <Caption>
              On the machine, the worker log says which: <code className="os-mono">~/.memql/state/worker.log</code>
            </Caption>
          </>
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
        <Fact label="Build" value={machine.buildTag} mono copyValue={machine.buildTag} />
        <Fact label="Registered" value={formatFreshness(machine.registeredAt, now)} title={machine.registeredAt || undefined} copyValue={machine.registeredAt || undefined} />
      </Facts>
      {renameError === "" ? null : <Caption>{renameError}</Caption>}
    </div>
  );
}
