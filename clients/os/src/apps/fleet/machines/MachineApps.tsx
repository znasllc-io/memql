import { useRef, useState } from "react";
import { Button, Fact, Facts, Notice } from "../../../kit";
import { PageHelp } from "../../../kit/pageHelp";
import { useSession } from "../../../chrome/access";
import { useOsConnection } from "../../../live/connection";
import { useOpenAskWork } from "../../../ask/AskProvider";
import { appLabel, machineName, RUNNABLE_APPS, type MachineApp, type MachineRow } from "../rows";
import { isWorkerOnline } from "../online";
import { workerClusterUrl } from "../addMachine/install";
import { MachineDetailsSection } from "./MachineDetailsInfo";
import { MachineAppRouting } from "./MachineAppRouting";
import { AppPermissionHelp } from "./AppPermissionHelp";

export function appStatus(machine: MachineRow, app: MachineApp | undefined, now: Date): string {
  if (machine.revokedAt) return "Machine removed";
  if (machine.ownerFeatures?.apps === false) return "Disabled in Settings";
  if (!isWorkerOnline(machine, now)) return "Machine offline";
  if (!machine.appsReported) return "Not reported";
  if (!app) return "Not installed";
  if (!app.signedIn) return "Sign-in needed";
  if (!app.allowed) return "Permission needed";
  return "Ready";
}

export function subscriptionLabel(value?: string): string {
  return value === "present" ? "Active" : value === "none" ? "No subscription" : "Not reported";
}

export function MachineAppsHelp() {
  return <PageHelp title="Apps"><p>Apps supported by MemQL. Ready means installed, signed in and allowed on this machine. Offline machines show their last reported information.</p><p>Subscription comes from the app. Not reported means the app has not supplied a plan; it does not prevent a ready app from running.</p><p>Setup starts work in Ask on this machine. MemQL checks what is needed and asks you for any permissions or sign-in steps. Readiness changes only after Cockpit reports it.</p></PageHelp>;
}

export function MachineApps({ machine, now }: { machine: MachineRow; now: Date }) {
  return <div className="fleet-machine-details fleet-machine-apps"><MachineAppRouting key={machine.id} machine={machine} />{RUNNABLE_APPS.map(id =>
    <MachineAppSection key={`${machine.id}:${id}`} id={id} machine={machine} now={now} />
  )}</div>;
}

function MachineAppSection({ id, machine, now }: { id: typeof RUNNABLE_APPS[number]; machine: MachineRow; now: Date }) {
  const app = machine.apps.find(item => item.id === id);
  const status = appStatus(machine, app, now);
  const connection = useOsConnection();
  const { access, config } = useSession();
  const openWork = useOpenAskWork();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [runId, setRunId] = useState("");
  const starting = useRef(false);
  const label = appLabel(id);
  const available = machine.ownerFeatures?.apps === false ? "Disabled in Settings" : machine.appsReported ? app ? app.allowed && app.signedIn ? "Allowed and signed in" : [app.allowed ? "Allowed" : "Not allowed", app.signedIn ? "Signed in" : "Signed out"].join(" · ") : "Install to connect" : "Waiting for Cockpit";
  const canSetup = machine.ownerFeatures?.apps !== false && !machine.revokedAt && isWorkerOnline(machine, now) && machine.ownerUserId === access?.userId;
  async function setup() {
    if (starting.current || !connection || !canSetup || !openWork) return;
    starting.current = true; setBusy(true); setError("");
    try {
      const result = await connection.query.createGoal({
        statement: `Set up ${label} for MemQL on the selected machine ${machineName(machine)} (registration ${machine.id}) only. Use the Computer Use knowledge for Fleet app setup. Check installation, sign-in and this cluster's app permission; complete missing setup and hand off any account sign-in to me. Do not switch machines.`,
        input: { appId: id, registrationId: machine.id, clusterUrl: workerClusterUrl(config.domain) },
        requestedVia: "nexus", noPaidProviders: true,
      });
      const receipt = result.rows()[0];
      const created = typeof receipt?.["runId"] === "string" ? receipt["runId"] : "";
      if (!created) throw new Error("Setup was not confirmed by the cluster.");
      setRunId(created); openWork(created);
    } catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { starting.current = false; setBusy(false); }
  }
  // A STATUS PAGE, NOT REFERENCE DATA: these rows say where each app stands,
  // and nothing on them is worth pasting elsewhere, so none offers copy.
  return <MachineDetailsSection title={label}>
    <Facts>
      <Fact label="Status" value={<span className="fleet-app-status" data-ready={status === "Ready" || undefined}>{status}</span>} />
      <Fact label="Installed" value={machine.appsReported ? app ? "Yes" : "No" : "Not reported"} />
      <Fact label="Availability" value={available} />
      <Fact label="Subscription" value={subscriptionLabel(app?.subscription)} />
      {app?.version ? <Fact label="Version" value={app.version} /> : null}
    </Facts>
    {app ? <AppPermissionHelp app={app} machine={machine} /> : null}
    {runId ? <div className="fleet-section-actions"><Button onClick={() => openWork?.(runId)}>View setup</Button></div>
      : status !== "Ready" && canSetup ? <div className="fleet-section-actions"><Button tone="primary" disabled={!connection || !openWork} busy={busy} busyLabel="Starting…" onClick={() => void setup()}>{!app ? "Set up" : !app.signedIn ? "Connect" : "Allow app"}</Button></div> : null}
    {error ? <Notice tone="error" sentence="Setup could not start." detail={error} /> : null}
  </MachineDetailsSection>;
}
