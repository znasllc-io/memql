import { bareShortId, sameEntityId } from "@znasllc-io/memql-sdk-core/client";

import { isRevoked, type MachineRow, type SharingMode } from "../rows";

// Who a machine is lent to, read and edited as values (epic memql#5344,
// design section 5).
//
// PURE, beside the components that draw it, for the reason rows.ts is: the
// sentence the panel says and the draft the dialog holds are the parts of this
// surface a person is promised things by, and asserting them through render()
// asserts them through three layers that can each fail for unrelated reasons.
//
// ===========================================================================
// IDS ARE COMPARED, NEVER COMPOSED
// ===========================================================================
// A row reaches the browser bare-ified on egress while the session's own id is
// canonical, so one person routinely arrives in two spellings. Every
// comparison here goes through the SDK's `bareShortId` / `sameEntityId` -- the
// engine's own rule -- and nothing here builds an id. What is sent back on a
// save is the spelling that came in: a picked subject as the directory named
// it, a stored one as the row holds it.

/** Whether this viewer is the machine's owner. An unresolved viewer ("") is
 *  nobody: answering true there would hand every machine's controls to the
 *  window before identity lands. */
export function ownsMachine(machine: Pick<MachineRow, "ownerUserId">, viewerId: string): boolean {
  return sameEntityId(viewerId, machine.ownerUserId);
}

/** The owner has said somebody other than them may use it. */
export function isShared(machine: Pick<MachineRow, "sharingMode">): boolean {
  return machine.sharingMode !== "owner";
}

/** Both halves given: the owner lent it AND its cockpit agreed, so somebody
 *  else's call can actually land on it. */
export function servesOthers(machine: Pick<MachineRow, "sharingMode" | "inferenceServe">): boolean {
  return isShared(machine) && machine.inferenceServe === "cluster";
}

/**
 * The words on the Equipment view's way into Sharing.
 *
 * WHAT THE MACHINE DOES, NOT WHAT WAS ASKED OF IT: the owner's mode AND the
 * cockpit's consent together (design section 5). A share the cockpit has not
 * agreed to serves nobody else yet, so it reads as personal here -- the
 * Sharing view, one click away, says which half is missing.
 */
export function sharingLinkWords(machine: Pick<MachineRow, "sharingMode" | "inferenceServe">): string {
  if (!servesOthers(machine)) return "Personal inference";
  return machine.sharingMode === "people" ? "Shared with people" : "Shared with everyone";
}

// ---------------------------------------------------------------------------
// Subjects
// ---------------------------------------------------------------------------

export type SubjectKind = "person" | "group";

/** One entry on a share list: a person or a group, by id. */
export interface Subject {
  kind: SubjectKind;
  id: string;
}

/** The comparison key for a subject: its kind and its BARE id, so a stored
 *  canonical id and the directory's bare one are the same entry. */
export function subjectKey(kind: SubjectKind, id: string): string {
  return `${kind}:${bareShortId(id.trim())}`;
}

/** The machine's stored list, people first, each in stored order. */
export function storedSubjects(machine: Pick<MachineRow, "sharedUserIds" | "sharedGroupIds">): Subject[] {
  return [
    ...machine.sharedUserIds.map((id): Subject => ({ kind: "person", id })),
    ...machine.sharedGroupIds.map((id): Subject => ({ kind: "group", id })),
  ];
}

// ---------------------------------------------------------------------------
// The one-line state
// ---------------------------------------------------------------------------

/**
 * Who can use the machine, in one line -- the owner's decision, which the two
 * consent lines beneath it qualify.
 *
 * NAMES WHEN EVERY ONE IS KNOWN, COUNTS OTHERWISE. Two names, then how many
 * more: "Shared with Ana Ruiz, Design and 2 more". A list whose names have not
 * been read (the directory answers only the owner, and a read can fail) is
 * counted instead -- "Shared with 2 people and 1 group" is a true sentence
 * with nothing guessed, where a half-named list or a bare id would not be.
 */
export function sharingSummary(
  machine: Pick<MachineRow, "sharingMode" | "sharedUserIds" | "sharedGroupIds">,
  viewerIsOwner: boolean,
  nameOf: (subject: Subject) => string | undefined,
): string {
  if (machine.sharingMode === "cluster") return "Everyone in this cluster";
  if (machine.sharingMode !== "people") return viewerIsOwner ? "Only you" : "Only its owner";
  const subjects = storedSubjects(machine);
  const names = subjects.map(nameOf);
  if (names.every((name): name is string => name !== undefined && name !== "")) {
    const [first, second] = names;
    if (names.length === 1) return `Shared with ${first}`;
    if (names.length === 2) return `Shared with ${first} and ${second}`;
    return `Shared with ${first}, ${second} and ${names.length - 2} more`;
  }
  return `Shared with ${countPhrase(machine.sharedUserIds.length, machine.sharedGroupIds.length)}`;
}

/** The stored decision as one comparable value: the mode, and who, by kind
 *  and bare id. Heartbeats and renames leave it alone. */
export function sharingSignature(
  machine: Pick<MachineRow, "sharingMode" | "sharedUserIds" | "sharedGroupIds">,
): string {
  const who = storedSubjects(machine).map((s) => subjectKey(s.kind, s.id));
  return [machine.sharingMode, ...who.sort()].join("|");
}

/**
 * Whether a save's receipt still describes what is stored: the same mode and,
 * under `people`, lists of the sizes it reported. The receipt carries counts,
 * never ids, so that is as much as it can be held to.
 */
export function receiptDescribes(
  receipt: { mode: SharingMode; people: number; groups: number },
  machine: Pick<MachineRow, "sharingMode" | "sharedUserIds" | "sharedGroupIds">,
): boolean {
  if (receipt.mode !== machine.sharingMode) return false;
  if (receipt.mode !== "people") return true;
  return receipt.people === machine.sharedUserIds.length && receipt.groups === machine.sharedGroupIds.length;
}

/** "2 people and 1 group", with the plurals right -- the engine's own
 *  phrasing for the same counts (shareCountPhrase). */
export function countPhrase(people: number, groups: number): string {
  const parts: string[] = [];
  if (people === 1) parts.push("1 person");
  else if (people > 1) parts.push(`${people} people`);
  if (groups === 1) parts.push("1 group");
  else if (groups > 1) parts.push(`${groups} groups`);
  return parts.join(" and ");
}

/** A group's size, as the picker shows it. */
export function groupSize(members: number): string {
  if (members <= 0) return "Group with nobody in it yet";
  return members === 1 ? "Group of 1 person" : `Group of ${members} people`;
}

// ---------------------------------------------------------------------------
// The dialog's draft
// ---------------------------------------------------------------------------

/**
 * What the dialog holds while it is open.
 *
 * THE LIST SURVIVES A CHANGE OF MODE. Choosing Everyone and then Specific
 * people again brings the chips back: the draft is the person's until they
 * save or cancel (design G10), and only the WRITE drops the list -- see
 * `draftChoice`.
 */
export interface ShareDraft {
  mode: SharingMode;
  subjects: Subject[];
}

export function draftFrom(machine: Pick<MachineRow, "sharingMode" | "sharedUserIds" | "sharedGroupIds">): ShareDraft {
  return { mode: machine.sharingMode, subjects: storedSubjects(machine) };
}

/**
 * Whether saving the draft would change anything.
 *
 * AGAINST THE ROW AS IT IS NOW, not as it was when the dialog opened: a change
 * landing from another tab is what is stored, and Save must say whether THIS
 * draft differs from it. Lists are compared as SETS of bare ids -- order is
 * not a decision anybody made, and the spelling is not either.
 */
export function draftDiffers(
  draft: ShareDraft,
  machine: Pick<MachineRow, "sharingMode" | "sharedUserIds" | "sharedGroupIds">,
): boolean {
  if (draft.mode !== machine.sharingMode) return true;
  if (draft.mode !== "people") return false;
  const drafted = new Set(draft.subjects.map((s) => subjectKey(s.kind, s.id)));
  const stored = new Set(storedSubjects(machine).map((s) => subjectKey(s.kind, s.id)));
  if (drafted.size !== stored.size) return true;
  for (const key of drafted) if (!stored.has(key)) return true;
  return false;
}

/** A people share must name somebody (the engine's share_needs_someone). */
export function draftNamesSomebody(draft: ShareDraft): boolean {
  return draft.mode !== "people" || draft.subjects.length > 0;
}

/** The choice a save sends: the lists only under `people`, and empty under
 *  the other two modes whatever the draft still holds (design G10). */
export function draftChoice(draft: ShareDraft): { mode: SharingMode; userIds: string[]; groupIds: string[] } {
  if (draft.mode !== "people") return { mode: draft.mode, userIds: [], groupIds: [] };
  return {
    mode: "people",
    userIds: draft.subjects.filter((s) => s.kind === "person").map((s) => s.id),
    groupIds: draft.subjects.filter((s) => s.kind === "group").map((s) => s.id),
  };
}

export function withSubject(draft: ShareDraft, subject: Subject): ShareDraft {
  const key = subjectKey(subject.kind, subject.id);
  if (draft.subjects.some((s) => subjectKey(s.kind, s.id) === key)) return draft;
  return { ...draft, subjects: [...draft.subjects, subject] };
}

export function withoutSubject(draft: ShareDraft, subject: Subject): ShareDraft {
  const key = subjectKey(subject.kind, subject.id);
  return { ...draft, subjects: draft.subjects.filter((s) => subjectKey(s.kind, s.id) !== key) };
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

/** Lower case with the accents taken off, so "ruiz" finds "Ruíz". */
function folded(text: string): string {
  return text.normalize("NFD").replace(/[̀-ͯ]/g, "").toLowerCase();
}

/** Whether any of these fields contains the query. An empty query matches. */
export function matchesQuery(query: string, ...fields: readonly string[]): boolean {
  const needle = folded(query.trim());
  if (needle === "") return true;
  return fields.some((field) => folded(field).includes(needle));
}

/** Whether a machine is one this viewer could lend -- owned, and still in
 *  the fleet. The engine refuses the rest (not_your_machine, machine_revoked). */
export function canLend(machine: Pick<MachineRow, "ownerUserId" | "revokedAt">, viewerId: string): boolean {
  return ownsMachine(machine, viewerId) && !isRevoked(machine);
}
