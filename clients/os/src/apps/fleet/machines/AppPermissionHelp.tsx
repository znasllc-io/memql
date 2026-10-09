import { useSessionIfPresent } from "../../../chrome/access";
import { Caption, CopyField } from "../../../kit";
import { workerClusterUrl } from "../addMachine/install";
import { isRevoked, machineName, RUNNABLE_APPS, type MachineApp, type MachineRow } from "../rows";

/** This permission belongs to the machine. Copying a command never grants it. */
export function AppPermissionHelp({ app, machine }: { app: MachineApp; machine: MachineRow }) {
  const session = useSessionIfPresent();
  if (app.allowed || isRevoked(machine) || !RUNNABLE_APPS.some(id => id === app.id)) return null;
  const cluster = workerClusterUrl(session?.config.domain ?? "");
  const version = /^v?(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$/.exec(machine.version);
  const current = version !== null && (Number(version[1]) > 0 || Number(version[2]) >= 17);
  // Quote the entire cluster argument: a deployment value must never become shell syntax.
  const command = `memql worker apps --allow ${app.id} --home '${cluster.replace(/'/g, "'\\''")}'`;
  return <details className="fleet-machine-facts">
    <summary>Allow {app.label}</summary>
    <div className="fleet-app-permission-body">
    <Caption>This lets this cluster send tasks to {app.label} on {machineName(machine)}. The app can edit files and run commands within Cockpit’s allowed workspace.</Caption>
    {!current ? <Caption>Update Cockpit to 0.17.0 or later on this machine first. {machine.version ? `It reports ${machine.version}.` : "Its version has not been reported."}</Caption> : null}
    {cluster ? <>
      <Caption>Run this on {machineName(machine)}. Fleet updates when Cockpit reports the permission.</Caption>
      <CopyField value={command} label={`the command to allow ${app.label} for this cluster`} />
    </> : <Caption>The cluster address is unavailable. Reconnect to this cluster to load the command.</Caption>}
    {!app.signedIn ? <Caption>Then sign in to {app.label} on that machine.</Caption> : null}
    </div>
  </details>;
}
