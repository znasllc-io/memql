import { bareShortId, sameEntityId } from "@znasllc-io/memql-sdk-core/client";

import { isRevoked, type MachineRow } from "../rows";

// Who a machine is lent to, read and edited as values (epic memql#5344,
// design section 5).
//
// PURE, beside the components that draw it, for the reason rows.ts is: the
// saved audience and individual grants are the parts of this
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

/** The owner has shared model inference with other people. */
export function servesOthers(machine: Pick<MachineRow, "sharingMode">): boolean {
  return isShared(machine);
}

/** The saved audience; model availability is reported separately. */
export function sharingLinkWords(machine: Pick<MachineRow, "sharingMode">): string {
  if (!isShared(machine)) return "Personal inference";
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
  paused?: boolean;
}

/** The comparison key for a subject: its kind and its BARE id, so a stored
 *  canonical id and the directory's bare one are the same entry. */
export function subjectKey(kind: SubjectKind, id: string): string {
  return `${kind}:${bareShortId(id.trim())}`;
}

/** The machine's stored list, people first, each in stored order. */
export function storedSubjects(machine: Pick<MachineRow, "sharedUserIds" | "sharedGroupIds" | "pausedUserIds" | "pausedGroupIds">): Subject[] {
  return [
    ...machine.sharedUserIds.map((id): Subject => ({ kind: "person", id, paused: machine.pausedUserIds?.some((v) => sameEntityId(v, id)) })),
    ...machine.sharedGroupIds.map((id): Subject => ({ kind: "group", id, paused: machine.pausedGroupIds?.some((v) => sameEntityId(v, id)) })),
  ];
}

/**
 * A stand-in for each subject on a list nobody has named yet -- its kind and
 * its place among that kind: "Person 2", "Group 1".
 *
 * NEVER THE ID. A person's id is an opaque 36-character token, and printing it
 * in a name's place tells the owner nothing they can act on (the Deployables
 * rule, apps/deployables/people.ts). NEVER "UNKNOWN" EITHER: that is the
 * directory's own answer for a subject no row names any more, and here the
 * directory has not answered at all.
 *
 * STABLE UNDER REMOVAL. The place is counted on the list the draft STARTED
 * from, so removing Person 1 leaves Person 2 as Person 2: a label that
 * renumbered under the person would have them remove the wrong one next. A
 * subject that list does not hold is counted on after it.
 */
export function standInNames(
  subjects: readonly Subject[],
  start: Pick<MachineRow, "sharedUserIds" | "sharedGroupIds">,
): Map<string, string> {
  const names = new Map<string, string>();
  const counted = { person: 0, group: 0 };
  const word = { person: "Person", group: "Group" };
  for (const subject of [...storedSubjects(start), ...subjects]) {
    const key = subjectKey(subject.kind, subject.id);
    if (names.has(key)) continue;
    counted[subject.kind] += 1;
    names.set(key, `${word[subject.kind]} ${counted[subject.kind]}`);
  }
  return names;
}

// ---------------------------------------------------------------------------
// The one-line state
// ---------------------------------------------------------------------------

/**
 * The saved audience, in one line for viewers without editing access.
 *
 * NAMES WHEN EVERY ONE IS KNOWN, COUNTS OTHERWISE. Two names, then how many
 * more: "Shared with Ana Ruiz, Design and 2 more". A list whose names have not
 * been read (the directory answers only the owner, and a read can fail) is
 * counted instead -- "Shared with 2 people and 1 group" is a true sentence
 * with nothing guessed, where a half-named list or a bare id would not be.
 */
export function sharingSummary(
  machine: Pick<MachineRow, "sharingMode" | "sharedUserIds" | "sharedGroupIds" | "pausedUserIds" | "pausedGroupIds">,
  viewerIsOwner: boolean,
  nameOf: (subject: Subject) => string | undefined,
): string {
  if (machine.sharingMode === "cluster") return "Everyone in this cluster";
  if (machine.sharingMode !== "people") return viewerIsOwner ? "Only you" : "Only its owner";
  const subjects = storedSubjects(machine);
  if (subjects.length === 0) return "No users or groups added";
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
  machine: Pick<MachineRow, "sharingMode" | "sharedUserIds" | "sharedGroupIds" | "pausedUserIds" | "pausedGroupIds">,
): string {
  const who = storedSubjects(machine).map((s) => `${subjectKey(s.kind, s.id)}:${s.paused === true}`);
  return [machine.sharingMode, ...who.sort()].join("|");
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

/** The saved audience and the individual grants that are paused. */
export type StoredShare = Pick<MachineRow, "sharingMode" | "sharedUserIds" | "sharedGroupIds" | "pausedUserIds" | "pausedGroupIds">;

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

/** Lower case with the accents taken off, so "ruiz" finds "Ruíz". */
function folded(text: string): string {
  return text.normalize("NFD").replace(/[\u0300-\u036f]/g, "").toLowerCase();
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
