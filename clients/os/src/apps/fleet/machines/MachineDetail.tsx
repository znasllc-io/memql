import { Trash2 } from "lucide-react";
import { useSession } from "../../../chrome/access";
import { localCockpitInstall } from "../addMachine/localInstall";
import { PageHelp } from "../../../kit/pageHelp";
import type { PageTrail } from "../../../kit/pageNavigation";
import { useEffect, useRef, useState } from "react";

import { MachineActivity } from "./MachineActivity";
import { Button, Caption, CopyField, Notice, Panel, Switch } from "../../../kit";
import { IconButton } from "../../../kit/IconButton";
import { formatMoment } from "../../../kit/format";
import { InPlaceAsk } from "../../../kit/pressHold";
import { uninstallCommand, workerClusterUrl, type InstallPlatform } from "../addMachine/install";
import {
  machineName,
  type MachineRow,
} from "../rows";
import { CredentialHealth } from "./CredentialHealth";
import { MachineApps } from "./MachineApps";
export { MachineAppsHelp } from "./MachineApps";
import { MachineDetailsInfo, MachineDetailsSection } from "./MachineDetailsInfo";
import { LabelEditor } from "./LabelEditor";
import { ModelsGroup } from "./ModelsGroup";
import { SharingGroup } from "./SharingGroup";
import { OwnerFeaturesGroup } from "./OwnerFeaturesGroup";
import { AutoRefresh } from "../../../kit/AutoRefresh";
import { useMachineRecommendation } from "./useMachineRecommendation";
import type { MachineWrites, RemovalReceipt } from "./useMachineWrites";

// One machine, in full: what it reported, what its owner set, what it can
// run, and what has run on it.

export function MachineDetail({
  machine,
  writes,
  now,
  view = "all",
  modelNavigation,
  openActivity,
  onActivityOpened,
}: {
  machine: MachineRow;
  writes: MachineWrites;
  now: Date;
  view?: "all" | "details" | "settings" | "models" | "apps" | "activity";
  modelNavigation?: Omit<PageTrail, "title">;
  /** An Activity entry to open, from Equipment's Recent activity. */
  openActivity?: string;
  onActivityOpened?: () => void;
}) {
  const label = machineName(machine);
  const busy = writes.busyId === machine.id;
  // The CLASS and the usable figure are computed on the engine and stored
  // nowhere, so they arrive with the recommendation rather than on the row --
  // one read for both, because a page that fetched the class separately could
  // show a class the recommendation disagreed with.
  const recommendation = useMachineRecommendation(machine.id, ["all", "details"].includes(view));

  return (
    <Panel label={`${label} detail`}>
      <AutoRefresh enabled={["all", "details"].includes(view)} busy={recommendation.state === "reading"} onRefresh={recommendation.reread} />
      {/* DETAILS is what the machine reported and how it is connected; SETTINGS
          is only what its owner can change. The advisories explain the
          connection facts, so they open the Details page. */}
      {view === "all" || view === "details" ? <div className="fleet-machine-details">
        <CredentialHealth machine={machine} now={now} />
        <MachineDetailsInfo machine={machine} now={now} machineClass={recommendation.recommended.machineClass} usableBytes={recommendation.recommended.usableBytes} />
      </div> : null}

      {view === "all" || view === "settings" ? <div className="fleet-machine-details">
        <MachineDetailsSection title="General">
          <div className="fleet-machine-setting">
            <span className="fleet-machine-setting-name">Name</span>
            <RenameField machine={machine} busy={busy} rename={writes.rename} />
          </div>
          <div className="fleet-machine-setting">
            <span className="fleet-machine-setting-name">Routing labels</span>
            <PageHelp title="Routing labels">
              <p>Operator labels influence routing and override matching labels reported by the machine. Each change saves automatically.</p>
              <p>Search by label name or value. In Label mode, enter a new name, choose Add and set its value. Names are unique within this machine and case-sensitive. Select a card to edit its value, or use its trash button to remove it.</p>
            </PageHelp>
            <LabelEditor key={machine.id} operatorLabels={machine.operatorLabels} busy={busy} onSave={labels => writes.setOperatorLabels(machine.id, labels)} />
          </div>
          <RemoveControl machine={machine} busy={busy} revoke={writes.revoke} />
          {writes.actionError && (!writes.actionErrorScope || writes.actionErrorScope === "machine") ? <Notice tone="error" sentence="The cluster refused that change." next="Nothing was written." detail={writes.actionError} /> : null}
        </MachineDetailsSection>
        <MachineDetailsSection title="Owner-only features">
          <OwnerFeaturesGroup machine={machine} writes={writes} />
        </MachineDetailsSection>
        <MachineDetailsSection title="Sharing">
          <SharingGroup machine={machine} writes={writes} />
          <SharingGroup machine={machine} writes={writes} resource="workbench" />
        </MachineDetailsSection>
      </div> : null}

      {view === "all" || view === "apps" ? <MachineApps machine={machine} now={now} /> : null}

      {/* Below Apps, because the two answer the same shape of question about
          this machine -- what it can be asked to do -- and a person scanning
          for either reads them together. */}
      {view === "all" || view === "models" ? <ModelsGroup key={machine.id} machine={machine} standalone={view === "models"} navigation={modelNavigation} /> : null}

      {view === "all" || view === "activity" ? <MachineActivity key={machine.id} workerId={machine.id} navigation={modelNavigation} openId={openActivity} onOpened={onActivityOpened} /> : null}

      {/* Sharing keeps refusals beside its inline draft. */}
      {writes.actionError && !["all", "settings"].includes(view) ? (
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
      className="os-form-row fleet-machine-rename"
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
  const root = useRef<HTMLDivElement>(null);
  const wasConfirming = useRef(false);
  useEffect(() => {
    if (confirming) root.current?.focus();
    else if (wasConfirming.current) root.current?.querySelector<HTMLButtonElement>(".fleet-machine-remove")?.focus();
    wasConfirming.current = confirming;
  }, [confirming]);
  const [reason, setReason] = useState("");
  const [purge, setPurge] = useState(false);
  // The receipt from the last removal on THIS machine (epic memql#5327,
  // design D2). It is held rather than discarded because a removal is two
  // writes and either can land alone: "removed, and its credential could not
  // be revoked" is a different outcome from "removed", and the person who
  // just removed a stolen laptop is the one who needs to know which they got.
  const [receipt, setReceipt] = useState<RemovalReceipt | null>(null);
  useEffect(() => { setPurge(false); setReceipt(null); }, [machine.id]);
  const label = machineName(machine);
  // The line names THIS cluster (design D12 as amended 2026-09-27): the
  // uninstaller removes this cluster's enrollment and, when it was the
  // machine's last one, Cockpit and its service as well. It carries no
  // --user-local, because the registration does not report its installation
  // path and the person removing a machine cannot be expected to know it:
  // until the amendment this page asked them to choose between a system and
  // an account-only installation, and a wrong answer was a line that found
  // nothing to remove. The uninstaller now detects which shape is installed
  // and removes what is there, so there is nothing left to ask.
  const uninstall = uninstallCommand(platformOf(machine), { localTest, purge: localTest !== null && purge, clusterUrl: workerClusterUrl(config.domain) });
  const uninstallControls = (
    <>
      <Caption>{localTest ? "Run this command on the machine to uninstall its connection to this cluster:" : "Run this command on the machine to remove its connection to this cluster:"}</Caption>
      {localTest ? <Caption>Uses the matching local test uninstaller for your account.</Caption> : null}
      {localTest ? <>
        <Switch checked={purge} onChange={setPurge}>Remove saved worker data</Switch>
        <Caption>Also removes worker policy, state and MemQL-managed model data. Refused if another cluster connection still uses them.</Caption>
      </> : null}
      <CopyField value={uninstall} label="the uninstall command" />
      <Caption>
        {localTest ? "Other cluster connections keep running. After the last connection is removed, the app and services are removed and MemQL’s Accessibility and Screen Recording authorizations are reset. macOS may retain a row in Settings." : "Removes this cluster's connection; other cluster connections keep running. After the last one is removed, Cockpit and its service are removed too, whether it was installed system-wide or for one account. A system-wide installation asks for the account password. Logs and settings remain; add --purge to remove retained data too."}
      </Caption>
      {localTest ? <Caption>CLI credentials, cluster settings, certificates and rollback backups are retained in either mode.</Caption> : null}
    </>
  );

  // THE RECEIPT IS ENOUGH ON ITS OWN, and that is not a convenience.
  //
  // A removal's own answer comes back before the row does: the write lands,
  // the engine answers, and the revoked row arrives moments later on the
  // subscription. Gating this branch on `machine.revokedAt` alone would put
  // the "Remove this machine" button back on screen for that gap -- after a
  // removal the operator has already performed -- and a partial receipt would
  // never be seen at all on a page they close in between.
  if (machine.revokedAt !== "" || receipt !== null) {
    // THE RECEIPT OUTRANKS THE ROW while this page is open. The row says the
    // machine was removed; the receipt says whether its CREDENTIAL went with
    // it. Rendering the standing sentence over a partial result is how the
    // finding this closes happened the first time: an operator was told the
    // whole job was done.
    const partial = receipt !== null && receipt.credentialState !== "revoked";
    return (
      <div className="os-fleet-revoked">
        {partial ? (
          <Notice
            tone={receipt.credentialState === "revoke_failed" ? "warn" : "info"}
            sentence={receipt.sentence}
            next={
              receipt.credentialState === "revoke_failed"
                ? "Revoke the credential from Settings > Credentials before treating this machine as gone."
                : undefined
            }
          />
        ) : (
          <p className="os-caption">
            {machine.revokedAt !== ""
              ? `Removed ${formatMoment(machine.revokedAt)}${machine.revokeReason ? ` -- ${machine.revokeReason}` : ""}.`
              : "Removed."}{" "}
            The registration row is kept as audit history and its credential no longer connects.
          </p>
        )}
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
      <div className="fleet-machine-remove-row" ref={root}>
        <span>Remove machine</span>
        <IconButton label="Remove this machine" className="fleet-machine-remove" disabled={busy} onClick={() => setConfirming(true)}>
          <Trash2 size={17} aria-hidden />
        </IconButton>
      </div>
    );
  }

  return (
    // THE REGION AND THE ACTION MUST NOT SHARE A NAME. The button inside is
    // `Remove <machine>` -- the action keeps that name from "Remove this
    // machine" all the way through -- so the region is named for the STATE it
    // puts the page in. Two controls answering to one accessible name is a
    // screen reader saying "Remove Studio mini, group" and then "Remove Studio
    // mini, button", which is the same phrase twice for two different things.
    <div ref={root} role="group" tabIndex={-1} aria-label={`Removing ${label}`}><InPlaceAsk className="os-fleet-confirm">
      {/* WHAT THIS SENTENCE SAYS IS NOW TRUE. It has always claimed the token
          stops working immediately; until epic memql#5327 the shell made one
          call, which revoked the registration and left the credential active,
          so the machine kept its stream and could come back. One act does both
          now, and the cluster ends the live stream from whichever replica is
          holding it. */}
      <p className="os-fleet-confirm-line">
        Remove <strong>{label}</strong>? Its connection ends within seconds and its token stops
        working, so it can neither take calls nor reconnect. The registration stays as audit
        history; pairing it again means a new token.
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
      <div className="os-panel-actions">
        <Button disabled={busy} onClick={() => setConfirming(false)}>
          Keep it
        </Button>
        <Button
          tone="danger"
          busy={busy}
          busyLabel="Removing..."
          onClick={() => {
            void revoke(machine.id, reason).then((result) => {
              // The confirm stays open on a refusal, with the error beside
              // it: closing would leave an operator believing a removal
              // happened that did not. A receipt -- even a partial one -- is
              // a removal that happened, so it closes and reports.
              if (result !== null) {
                setReceipt(result);
                setConfirming(false);
              }
            });
          }}
        >
          Remove {label}
        </Button>
      </div>
    </InPlaceAsk></div>
  );
}
