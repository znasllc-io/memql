import { useSession } from "../../../chrome/access";
import { Caption } from "../../../kit";
import { PageHelp } from "../../../kit/pageHelp";
import { isRevoked, sharingFrom, type MachineRow } from "../rows";
import { SharingEditor } from "./SharingEditor";
import { ownsMachine, sharingSummary } from "./sharing";
import type { MachineWrites, SharingResource } from "./useMachineWrites";

export function SharingGroup({ machine: source, writes, resource = "models" }: {
  machine: MachineRow;
  writes: MachineWrites;
  resource?: SharingResource;
}) {
  const share = source.workbenchSharing ?? sharingFrom(undefined);
  const machine = resource === "models" ? source : {
    ...source, sharingMode: share.mode, sharingPaused: share.paused,
    sharedUserIds: share.userIds, sharedGroupIds: share.groupIds,
    pausedUserIds: share.pausedUserIds, pausedGroupIds: share.pausedGroupIds,
    sharingActivity: source.workbenchSharingActivity,
  };
  const { access } = useSession();
  const isOwner = ownsMachine(machine, access?.userId ?? "");
  const revoked = isRevoked(machine);

  return (
    <div className="os-fleet-sharing">
      <PageHelp title={resource === "models" ? "Model sharing" : "Fleet workbench sharing"}>
        <p>Enable sharing, then open search to choose existing users or groups. The list starts with five options; Load more reveals another five. Search covers the full eligible directory. Filter by All, Users or Groups. Selections stay checked across searches and filters, and one counter shows the total. Click the plus button to add all selected entries together; it stays disabled with nothing selected.</p>
        <p>Use the arrow keys to move through options and Enter to select or unselect. Arrow down past the last option reaches Load more. Escape closes the list without clearing selections. Search also finds access already granted. Cards use the same paging arrows and counter as routing labels.</p>
        <p>Select a card to edit whether it accepts new work, then Save or Cancel. The pause, resume and trash icons save immediately. Turning sharing off pauses new borrowed work and preserves the list. Turning it on resumes the saved audience. Trash removes an individual grant.</p>
        <p>{resource === "models" ? "Model sharing grants inference only." : "Fleet workbench sharing grants access to isolated workbenches on this machine. Cloud workbenches run on the cluster and have separate capacity."} Host files, commands, signed-in CLIs and computer control stay with the machine’s owner. Sharing does not install a runtime or make an unavailable machine ready.</p>
        <p>Pausing blocks new calls through that grant; work already running finishes. Active calls update automatically. A call admitted through multiple grants appears on each matching row. A user with another active grant can still use the shared resource. Counts contain no prompts or responses.</p>
        <p>Unavailable call counts show a dash. A failed change leaves the saved list intact and offers a retry.</p>
      </PageHelp>
      {isOwner && !revoked ? <SharingEditor key={`${machine.id}:${resource}`} machine={machine} writes={writes} resource={resource} /> : <>
        <p className="os-fleet-sharing-state">{sharingSummary(machine, isOwner, () => undefined)}</p>
        <Caption>{revoked ? "This machine was removed, so it cannot be shared." : "Only this machine's owner can change who it is shared with."}</Caption>
      </>}
    </div>
  );
}
