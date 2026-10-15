import { useSession } from "../../../chrome/access";
import { Caption, Subhead } from "../../../kit";
import { PageHelp } from "../../../kit/pageHelp";
import { isRevoked, type MachineRow } from "../rows";
import { SharingEditor } from "./SharingEditor";
import { ownsMachine, sharingSummary } from "./sharing";
import type { MachineWrites } from "./useMachineWrites";

export function SharingGroup({ machine, writes, standalone = false }: {
  standalone?: boolean;
  machine: MachineRow;
  writes: MachineWrites;
}) {
  const { access } = useSession();
  const isOwner = ownsMachine(machine, access?.userId ?? "");
  const revoked = isRevoked(machine);

  return (
    <div className="os-fleet-sharing">
      {!standalone ? <Subhead>Model sharing</Subhead> : null}
      <PageHelp title="Model sharing">
        <p>Enable Share models, then search for users or groups to add. Changes save automatically. Pause or resume each entry with its icon; remove an entry to end that grant. Removed entries can be added again. Turning Share models off removes the list and keeps inference private.</p>
        <p>Sharing grants model inference only. Files, commands, apps and computer control stay with the machine’s owner. Your saved audience applies automatically; no machine configuration is needed.</p>
        <p>Pausing blocks new calls through that grant; work already running finishes. Active calls update automatically. A call admitted through multiple grants appears on each matching row. A user with another active grant can still use the models. Counts contain no prompts or responses.</p>
        <p>Unavailable call counts show a dash. A failed change leaves the saved list intact and offers a retry.</p>
      </PageHelp>
      {isOwner && !revoked ? <SharingEditor key={machine.id} machine={machine} writes={writes} /> : <>
        <p className="os-fleet-sharing-state">{sharingSummary(machine, isOwner, () => undefined)}</p>
        <Caption>{revoked ? "This machine was removed, so it cannot be shared." : "Only this machine's owner can change who it is shared with."}</Caption>
      </>}
    </div>
  );
}
