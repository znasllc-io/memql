import { useSession } from "../../../chrome/access";
import { Caption, Notice, Subhead } from "../../../kit";
import { PageHelp } from "../../../kit/pageHelp";
import { isRevoked, type MachineRow } from "../rows";
import { SharingEditor } from "./SharingEditor";
import { isShared, ownsMachine, servesOthers, sharingSummary } from "./sharing";
import type { MachineWrites } from "./useMachineWrites";
import type { SharingLedger } from "./useMachineInference";

export function SharingGroup({ machine, writes, ledger, standalone = false }: {
  standalone?: boolean;
  machine: MachineRow;
  writes: MachineWrites;
  ledger: SharingLedger | null;
}) {
  const { access } = useSession();
  const isOwner = ownsMachine(machine, access?.userId ?? "");
  const revoked = isRevoked(machine);

  const usage = servesOthers(machine) && ledger ? ledger.readable ? <Caption>{ledger.sentence}</Caption> : <Notice tone="warn" sentence={ledger.sentence} /> : null;

  return (
    <div className="os-fleet-sharing">
      {!standalone ? <Subhead>Model sharing</Subhead> : null}
      <PageHelp title="Model sharing">
        <p>Enable Share models, then choose people and groups. Save applies your choices; Discard changes restores the saved audience. Turn sharing off and save to keep models private.</p>
        <p>Sharing grants model inference only. Files, commands, apps and computer control stay with the machine’s owner. Cockpit must also allow sharing before anyone else can use its models.</p>
        <p>Changes apply to the next call; a call already running finishes. Usage shows counts without prompts or responses.</p>
        <p>If another session changes sharing while you edit, choose whether to use the new sharing or keep your changes before saving.</p>
      </PageHelp>
      {isOwner && !revoked ? <SharingEditor key={machine.id} machine={machine} writes={writes}>{usage}</SharingEditor> : <>
        <p className="os-fleet-sharing-state">{sharingSummary(machine, isOwner, () => undefined)}</p>
        <Caption>{revoked ? "This machine was removed, so it cannot be shared." : "Only this machine's owner can change who it is shared with."}</Caption>
        {!revoked && isShared(machine) && machine.inferenceServe !== "cluster" ? <Notice tone="info" sentence="Model sharing is waiting for Cockpit." next="The person who enrolled this machine needs to enable model sharing in Cockpit." /> : null}
        {usage}
      </>}
    </div>
  );
}
