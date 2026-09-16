import { Terminal } from "lucide-react";
import { useSession } from "../../../chrome/access";
import { localCockpitInstall } from "../addMachine/localInstall";
import { InfoDetail } from "../../../kit/InfoDetail";
import { useEffect, useState } from "react";

import { CallHistory } from "../routing/CallHistory";
import { Button, EmptyState, Caption, Chip, Chips, ChoiceStack, CopyField, Fact, Facts, Notice, Panel, Subhead, Switch } from "../../../kit";
import { formatFreshness, formatMoment } from "../../../kit/format";
import { uninstallCommand, workerClusterUrl, type InstallPlatform } from "../addMachine/install";
import { roundTripFigure } from "../addMachine/flow";
import { isWorkerOnline } from "../online";
import { computerUseStatus, hasRoundTrip, machineName, type MachineRow } from "../rows";
import { HardwareGroup } from "./HardwareGroup";
import { LabelEditor } from "./LabelEditor";
import { ModelsGroup } from "./ModelsGroup";
import { SharingGroup } from "./SharingGroup";
import { useMachineInference } from "./useMachineInference";
import type { MachineWrites } from "./useMachineWrites";

// One machine, in full: what it reported, what its owner set, what it can
// run, and what has run on it.

export function MachineDetail({
  machine,
  writes,
  now,
  view = "all",
}: {
  machine: MachineRow;
  writes: MachineWrites;
  now: Date;
  view?: "all" | "details" | "models" | "apps" | "sharing" | "activity";
}) {
  const label = machineName(machine);
  const busy = writes.busyId === machine.id;
  const online = isWorkerOnline(machine, now);
  // The CLASS and the usable figure are computed on the engine and stored
  // nowhere, so they arrive with the recommendation rather than on the row --
  // one read for both, because a page that fetched the class separately could
  // show a class the recommendation disagreed with.
  const inference = useMachineInference(machine.id);

  return (
    <Panel label={`${label} detail`}>
      {view === "all" || view === "details" ? <>
      <RenameField machine={machine} busy={busy} rename={writes.rename} />

      <details className="fleet-machine-facts"><summary>Connection, permissions & registration</summary>
      <Facts>
        <Fact label="Reported name" value={machine.name} mono />
        <Fact label="Hostname" value={machine.hostname} mono />
        <Fact label="Platform" value={machine.platform} mono />
        <Fact
          label="Last heartbeat"
          value={formatFreshness(machine.lastSeenAt, now)}
          title={machine.lastSeenAt || undefined}
        />
        <Fact label="Online" value={online ? "yes" : "no"} />
        {/* THE CLUSTER'S OWN ROUND TRIP (design record
            2026-09-08-cockpit-install-wizard, D11): the last Ping the holding
            replica sent and this machine answered. ABSENT IS "NOT MEASURED",
            never slow -- a cockpit that predates the ping never answers one. */}
        <Fact
          label="Round trip"
          value={hasRoundTrip(machine) ? roundTripFigure(machine, now) : "not measured -- this cockpit does not answer pings"}
          title={machine.rttAt || undefined}
        />
        <Fact label="Calls in flight" value={String(machine.activeCount)} />
        <Fact label="Registered" value={formatMoment(machine.registeredAt)} />
        <Fact label="Cockpit version" value={machine.version} mono />
        <Fact label="Build" value={machine.buildTag} mono />
        <Fact label="Display server" value={machine.displayServer} mono />
        <Fact label="Computer use" value={computerUseStatus(machine).answer} />
        <Fact label="Registration id" value={machine.id} mono />
      </Facts>

      {/* `activeCount` is up to one heartbeat interval stale by construction
          -- it is what the machine reported on its last beat, not a live
          count -- and saying so is cheaper than an operator concluding the
          number is wrong. */}
      <p className="os-caption">
        Calls in flight is as of the last heartbeat, so it can trail a call that started since.
      </p>

      </details>
      {/* HARDWARE FIRST, because what a machine IS precedes what it can be
          asked to do -- and because the machine class it computes is what the
          Models group's recommendation rests on. A reader who meets the
          recommendation first has to scroll back to find out why it is that
          set. */}
      <HardwareGroup
        machine={machine}
        machineClass={inference.recommended.machineClass}
        usableBytes={inference.recommended.usableBytes}
        now={now}
      />

      <LabelGroups machine={machine} busy={busy} writes={writes} />
      </> : null}

      {view === "all" || view === "apps" ? <AppsGroup machine={machine} standalone={view === "apps"} /> : null}

      {/* Below Apps, because the two answer the same shape of question about
          this machine -- what it can be asked to do -- and a person scanning
          for either reads them together. */}
      {view === "all" || view === "models" ? <ModelsGroup machine={machine} standalone={view === "models"} /> : null}

      {/* SHARING AFTER MODELS, because the question it asks -- will you lend
          this machine to everybody -- only means something once a reader knows
          what the machine can serve. Offering it above an empty Models group
          would be asking somebody to volunteer a machine that runs nothing. */}
      {view === "all" || view === "sharing" ? <SharingGroup machine={machine} writes={writes} ledger={inference.ledger} standalone={view === "sharing"} /> : null}

      {view === "all" || view === "activity" ? <CallHistory standalone={view === "activity"} workerId={machine.id} machineLabel={label} /> : null}

      {view === "all" || view === "details" ? <RemoveControl machine={machine} busy={busy} revoke={writes.revoke} /> : null}

      {writes.actionError ? (
        <Notice
          tone="error"
          sentence="The cluster refused that change."
          next="Nothing was written."
          detail={writes.actionError}
        />
      ) : null}
    </Panel>
  );
}

function RenameField({
  machine,
  busy,
  rename,
}: {
  machine: MachineRow;
  busy: boolean;
  rename: MachineWrites["rename"];
}) {
  const [draft, setDraft] = useState(machine.displayName);

  // The ROW wins, for the reason the label editor's does: a rename landing
  // from another tab has to reach this field. The dependency is the VALUE, so
  // a heartbeat -- which changes the row object and nothing an operator typed
  // -- does not stamp on a name being edited.
  useEffect(() => {
    setDraft(machine.displayName);
  }, [machine.displayName]);

  const changed = draft.trim() !== machine.displayName.trim();

  return (
    <form
      className="os-form-row"
      onSubmit={(e) => {
        e.preventDefault();
        if (!changed) return;
        // No local write-through: the value on screen comes back on the live
        // feed. Setting it here would show a name the cluster may have
        // refused, and the refusal renders below rather than reverting a
        // field the operator is looking at.
        void rename(machine.id, draft.trim());
      }}
    >
      <label className="os-sr-only" htmlFor={`fleet-rename-${machine.id}`}>
        Name for {machineName(machine)}
      </label>
      <input
        id={`fleet-rename-${machine.id}`}
        className="os-input"
        value={draft}
        disabled={busy}
        placeholder={machine.name || "Name this machine"}
        onChange={(e) => setDraft(e.target.value)}
      />
      <Button type="submit" tone="primary" disabled={!changed} busy={busy} busyLabel="Saving...">
        Rename
      </Button>
    </form>
  );
}

function LabelGroups({
  machine,
  busy,
  writes,
}: {
  machine: MachineRow;
  busy: boolean;
  writes: MachineWrites;
}) {
  const reportedKeys = Object.keys(machine.reportedLabels).sort();

  return (
    <div className="os-fleet-labels">
      <details className="fleet-machine-facts">
        <summary>Reported by the machine</summary>
        {/* The caveat is UI copy rather than a comment, because the person
            who needs it is the one about to look for an edit control here
            and not find one. */}
        <InfoDetail title="Reported labels"><p>Read-only labels are replaced whenever Cockpit connects. Set operator labels below to override them in routing.</p></InfoDetail>
        <Chips label="Reported labels">
          {reportedKeys.length === 0 ? (
            <span className="os-caption">None reported.</span>
          ) : (
            reportedKeys.map((key) => {
              const overridden = machine.operatorLabels[key] !== undefined;
              return (
                <Chip
                  key={key}
                  tone="muted"
                  title={overridden ? "Overridden by an operator label below" : undefined}
                >
                  {`${key}=${machine.reportedLabels[key] ?? ""}`}
                  {overridden ? <span className="os-fleet-overridden">overridden</span> : null}
                </Chip>
              );
            })
          )}
        </Chips>
      </details>

      <div className="os-fleet-labelgroup">
        <Subhead>Set by you</Subhead>
        <p className="os-caption">
          Use labels to influence routing. Your labels override matching labels reported by the machine.
        </p>
        <LabelEditor
          operatorLabels={machine.operatorLabels}
          busy={busy}
          onSave={(labels) => writes.setOperatorLabels(machine.id, labels)}
        />
      </div>
    </div>
  );
}

export function MachineAppsHelp() {
  return <InfoDetail title="Apps on this machine"><p>Apps installed on this machine that Cockpit reports to MemQL. Ready apps are supported, allowed by Cockpit and signed in. Subscription status comes from the app.</p></InfoDetail>;
}

function AppsGroup({ machine, standalone }: { machine: MachineRow; standalone: boolean }) {
  return (
    <div className="os-fleet-apps">
      {!standalone ? <div className="fleet-bank-heading"><Subhead>Apps on this machine</Subhead><MachineAppsHelp /></div> : null}
      {machine.apps.length === 0 ? (
        <EmptyState icon={Terminal} title="No apps reported">Install and sign in to a supported app on this machine. It will appear here when Cockpit reports it.</EmptyState>
      ) : (
        <ul className="os-fleet-applist">
          {machine.apps.map((app) => (
            <li key={app.id}>
              <details className="fleet-record" data-runnable={app.runnable || undefined}><summary><span className="fleet-record-identity"><strong>{app.label}</strong><small>{app.version || "Version not reported"}</small></span><span className="fleet-record-status">{app.runnable ? "Ready" : "Needs attention"}</span></summary>
                <div className="fleet-record-detail"><Facts><Fact label="Subscription" value={app.subscription || "Unknown"} /><Fact label="Availability" value={app.runnable ? "Allowed and signed in" : app.why || "Not available to run"} /></Facts></div>
              </details>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** The uninstaller the machine's own platform takes. Anything that is not
 *  recognisably Linux gets the macOS line, the same reading the guided
 *  install's default makes. */
function platformOf(machine: MachineRow): InstallPlatform {
  return /linux/i.test(machine.os) ? "linux" : "mac";
}

// REMOVE THIS MACHINE (design record 2026-09-08-cockpit-install-wizard, D12):
// the revoke, and the one line that takes the cockpit off the machine.
//
// Two halves in one act because they are one decision with two sides. The
// cluster's half is the revoke -- the token stops working, the registration
// stays as audit history. The machine's half is the uninstall line: without
// it the worker on the machine keeps retrying with a dead token, its service
// restarts it at every login, and nothing on the machine says why. So the line
// is shown BEFORE the revoke is confirmed and stays after, on the revoked
// row, for the person who revoked first and wondered about the machine later.
function RemoveControl({
  machine,
  busy,
  revoke,
}: {
  machine: MachineRow;
  busy: boolean;
  revoke: MachineWrites["revoke"];
}) {
  const { config } = useSession();
  const localTest = platformOf(machine) === "mac" ? localCockpitInstall(config.domain) : null;
  const [confirming, setConfirming] = useState(false);
  const [reason, setReason] = useState("");
  const [userLocal, setUserLocal] = useState(false);
  const [purge, setPurge] = useState(false);
  useEffect(() => { setUserLocal(false); setPurge(false); }, [machine.id]);
  const label = machineName(machine);
  const uninstall = uninstallCommand(platformOf(machine), { userLocal, localTest, purge: localTest !== null && purge, clusterUrl: workerClusterUrl(config.domain) });
  // The registration does not report its installation path. Ask rather than
  // inferring it from the version or the machine's operating system.
  const uninstallControls = (
    <>
      <Caption>{localTest ? "Run this command on the machine to uninstall its connection to this cluster:" : "Choose where Cockpit was installed on this machine, then run the matching command on it:"}</Caption>
      {localTest ? <Caption>Uses the matching local test uninstaller for your account.</Caption> : <ChoiceStack
        name={`fleet-uninstall-location-${machine.id}`}
        label="Cockpit installation location"
        voice="prose"
        value={userLocal ? "user" : "system"}
        onChange={(next) => setUserLocal(next === "user")}
        options={[
          { value: "system", label: "System installation", description: "Installed in /usr/local/bin using an account password." },
          { value: "user", label: "My account only", description: "Installed without a password in ~/.memql/bin." },
        ]}
      />
      }
      {localTest ? <>
        <Switch checked={purge} onChange={setPurge}>Remove saved worker data</Switch>
        <Caption>Also removes worker policy, state and MemQL-managed model data. Refused if another cluster connection still uses them.</Caption>
      </> : null}
      <CopyField value={uninstall} label="the uninstall command" />
      <Caption>
        {localTest ? "Other cluster connections keep running. After the last connection is removed, the app and services are removed and MemQL’s Accessibility and Screen Recording authorizations are reset. macOS may retain a row in Settings." : "Stops the service and removes Cockpit and its connection token. Logs and settings remain; add --purge to remove retained data too."}
      </Caption>
      {localTest ? <Caption>CLI credentials, cluster settings, certificates and rollback backups are retained in either mode.</Caption> : null}
    </>
  );

  if (machine.revokedAt) {
    return (
      <div className="os-fleet-revoked">
        <p className="os-caption">
          Revoked {formatMoment(machine.revokedAt)}
          {machine.revokeReason ? ` -- ${machine.revokeReason}` : ""}. The registration row is kept
          as audit history and its credential can never be used again.
        </p>
        {uninstallControls}
      </div>
    );
  }

  // The confirm is IN SURFACE and NAMES the machine. A browser confirm()
  // blocks the whole shell, and a generic "are you sure" invites the mistake
  // it exists to prevent: an operator with two machines open confirms the
  // wrong one because nothing on the dialog said which.
  if (!confirming) {
    return (
      <div className="os-head-actions">
        <Button tone="danger" onClick={() => setConfirming(true)}>
          Remove this machine
        </Button>
      </div>
    );
  }

  return (
    <div className="os-fleet-confirm" role="group" aria-label={`Remove ${label}`}>
      <p className="os-fleet-confirm-line">
        Remove <strong>{label}</strong>? Its worker token stops working immediately and it can no
        longer take calls. The registration stays as audit history; pairing it again means minting
        a new token.
      </p>
      {uninstallControls}
      <label className="os-sr-only" htmlFor={`fleet-revoke-reason-${machine.id}`}>
        Reason (optional)
      </label>
      <input
        id={`fleet-revoke-reason-${machine.id}`}
        className="os-input"
        value={reason}
        disabled={busy}
        placeholder="Reason (optional)"
        onChange={(e) => setReason(e.target.value)}
      />
      <div className="os-head-actions">
        <Button
          tone="danger"
          busy={busy}
          busyLabel="Revoking..."
          onClick={() => {
            void revoke(machine.id, reason).then((ok) => {
              // The confirm stays open on a refusal, with the error beside
              // it: closing would leave an operator believing a revocation
              // happened that did not.
              if (ok) setConfirming(false);
            });
          }}
        >
          Revoke {label}
        </Button>
        <Button disabled={busy} onClick={() => setConfirming(false)}>
          Keep it
        </Button>
      </div>
    </div>
  );
}
