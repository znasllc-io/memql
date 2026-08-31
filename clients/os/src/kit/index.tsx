// The OS kit: the shared surface app epics build against (spec H). One
// import site -- apps must not respell these. LiveList joins in the live
// substrate task (PR B) and is exported from here the moment it exists.

import { deriveProvenance, type MachinePresence, type ProvenanceFacts, type ProvenanceTone } from "../items/provenance";
import type { FileEntry } from "../system/desktop";
import { roleAdmits, ROLE_LADDER, type ClusterRole, type RoleRequirement } from "../system/roles";

export { Caption } from "./Caption";
export {
  Button,
  Check,
  Chip,
  Chips,
  ChoiceStack,
  Fact,
  Facts,
  FormRow,
  Head,
  Input,
  Notice,
  Panel,
  Select,
  Subhead,
  type ButtonTone,
  type ChipTone,
  type ChoiceOption,
  type NoticeTone,
} from "./controls";
export { LiveList, type LiveListSource } from "../live/LiveList";
export { deriveProvenance, roleAdmits, ROLE_LADDER };
export type { ClusterRole, MachinePresence, ProvenanceFacts, ProvenanceTone, RoleRequirement };

/**
 * The dot language (spec D3): green = reachable now, amber = not reachable,
 * unknown = NO dot. The same component renders dock "running", connection
 * state and fleet "online" so aliveness reads identically everywhere.
 */
export function ProvenanceDot({
  tone,
  label,
}: {
  tone: ProvenanceTone;
  /** Accessible name; the dot itself is not text. */
  label?: string;
}) {
  if (tone === "unknown") return null;
  return (
    <span
      className="os-dot"
      data-os-dot={tone}
      role={label ? "img" : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
    />
  );
}

export function FileProvenanceDot({
  file,
  machine = null,
}: {
  file: Pick<FileEntry, "source" | "producedByWorkerId" | "uploadState">;
  machine?: MachinePresence | null;
}) {
  const facts = deriveProvenance(file, machine);
  return <ProvenanceDot tone={facts.tone} label={facts.origin || undefined} />;
}
