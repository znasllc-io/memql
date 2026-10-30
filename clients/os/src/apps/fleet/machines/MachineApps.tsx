import { useRef, useState } from "react";
import { Button, Fact, Facts, Notice } from "../../../kit";
import { formatFreshness, formatMoment } from "../../../kit/format";
import { PageHelp } from "../../../kit/pageHelp";
import { useSession } from "../../../chrome/access";
import { useOsConnection } from "../../../live/connection";
import { useOpenAskWork } from "../../../ask/AskProvider";
import { appLabel, machineName, RUNNABLE_APPS, type MachineRow } from "../rows";
import { isWorkerOnline } from "../online";
import { workerClusterUrl } from "../addMachine/install";
import { MachineDetailsSection } from "./MachineDetailsInfo";
import { MachineAppRouting } from "./MachineAppRouting";
import { AppPermissionHelp } from "./AppPermissionHelp";
import { appStatus } from "./features";
import { AppSignInAction } from "./AppSignInAction";
import { useAppSignIns, type AppSignInsHandle } from "./useAppSignIns";

export function subscriptionLabel(value?: string): string {
  return value === "present" ? "Active" : value === "none" ? "No subscription" : "Not reported";
}

export function MachineAppsHelp() {
  return <PageHelp title="Apps"><p>Apps supported by MemQL. Each machine checks on a schedule that its apps can answer. Ready means signed in with usage left. Out of usage says when it resets. MemQL sends work only to an app that is ready.</p><p>Could not check means the machine asked and got no answer, so the app gets no work until it does. Not checked means this machine's Cockpit predates the checks. The app is still used until Cockpit is updated, and a call it refuses moves to the next app.</p><p>Sign in opens the app’s own page in this browser, wherever you are, and the login stays on the machine. Subscription comes from the app; Not reported does not prevent a ready app from running.</p></PageHelp>;
}

export function MachineApps({ machine, now }: { machine: MachineRow; now: Date }) {
  const { access } = useSession();
  // Only a machine's owner may sign it in, so nobody else has a feed to read.
  const signIns = useAppSignIns(machine.ownerUserId === access?.userId ? machine.id : "");
  return <div className="fleet-machine-details fleet-machine-apps"><MachineAppRouting key={machine.id} machine={machine} />{RUNNABLE_APPS.map(id =>
    <MachineAppSection key={`${machine.id}:${id}`} id={id} machine={machine} now={now} signIns={signIns} />
  )}</div>;
}

function MachineAppSection({ id, machine, now, signIns }: { id: typeof RUNNABLE_APPS[number]; machine: MachineRow; now: Date; signIns: AppSignInsHandle }) {
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
  // What the Ask-driven setup can still do: install, ask for the permission, or sign in
  // a machine whose Cockpit cannot run a sign-in itself.
  const setupLabel = !app ? "Set up" : !app.allowed ? "Allow app" : !app.signedIn && !app.canSignIn ? "Connect" : "";
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
      {status === "Out of usage" && app?.resetsAt ? <Fact label="Resets" value={formatMoment(app.resetsAt)} title={app.resetsAt} /> : null}
      {status === "Could not check" && app?.readinessDetail ? <Fact label="Why" value={app.readinessDetail} /> : null}
      {app?.checkedAt ? <Fact label="Last checked" value={formatFreshness(app.checkedAt, now)} title={app.checkedAt} /> : null}
      <Fact label="Subscription" value={subscriptionLabel(app?.subscription)} />
      {app?.version ? <Fact label="Version" value={app.version} /> : null}
    </Facts>
    {app ? <AppPermissionHelp app={app} machine={machine} /> : null}
    {app && canSetup ? <AppSignInAction app={id} label={label} signIns={signIns} now={now} available={app.allowed && !app.signedIn && app.canSignIn} /> : null}
    {runId ? <div className="fleet-section-actions"><Button onClick={() => openWork?.(runId)}>View setup</Button></div>
      : setupLabel && canSetup ? <div className="fleet-section-actions"><Button tone="primary" disabled={!connection || !openWork} busy={busy} busyLabel="Starting…" onClick={() => void setup()}>{setupLabel}</Button></div> : null}
    {error ? <Notice tone="error" sentence="Setup could not start." detail={error} /> : null}
  </MachineDetailsSection>;
}
