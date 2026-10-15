import { useSession } from "../../../chrome/access";
import { Caption } from "../../../kit";
import { PageHelp } from "../../../kit/pageHelp";
import { isRevoked, type MachineRow } from "../rows";
import { SharingEditor } from "./SharingEditor";
import { ownsMachine, sharingSummary } from "./sharing";
import type { MachineWrites } from "./useMachineWrites";

export function SharingGroup({ machine, writes }: {
  machine: MachineRow;
  writes: MachineWrites;
}) {
  const { access } = useSession();
  const isOwner = ownsMachine(machine, access?.userId ?? "");
  const revoked = isRevoked(machine);

  return (
    <div className="os-fleet-sharing">
      <PageHelp title="Model sharing">
        <p>Enable Share models, then open search to choose existing users or groups. The list starts with five options; Load more reveals another five. Search covers the full eligible directory. Filter by All, Users or Groups. Selections stay checked across searches and filters, and one counter shows the total. Click the plus button to add all selected entries together; it stays disabled with nothing selected.</p>
        <p>Use the arrow keys to move through options and Enter to select or unselect. Arrow down past the last option reaches Load more. Escape closes the list without clearing selections. Search also finds access already granted. Cards use the same paging arrows and counter as routing labels.</p>
        <p>Select a card to edit whether it accepts new calls, then Save or Cancel. The pause, resume and trash icons save immediately. Removed entries can be added again. Turning Share models off removes the access list and keeps inference private.</p>
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
