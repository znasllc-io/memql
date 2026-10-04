import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import { useEffect, useId, useLayoutEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from "react";
import { Plus, User, Users, X } from "lucide-react";

import { Button, Caption, Chip, Chips, ChoiceStack, Notice, Subhead, type ChoiceOption } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import { listScrollTop } from "../../../kit/controls";
import { machineName, type MachineRow, type SharingMode } from "../rows";
import {
  draftChoice,
  draftDiffers,
  draftFrom,
  draftNamesSomebody,
  draftUntouched,
  groupSize,
  matchesQuery,
  sharingSignature,
  sharingSummary,
  standInNames,
  storedShare,
  subjectKey,
  withSubject,
  withoutSubject,
  type ShareDraft,
  type StoredShare,
  type Subject,
  type SubjectKind,
} from "./sharing";
import type { MachineWrites, SharingReceipt } from "./useMachineWrites";
import { useShareDirectory, type ShareDirectory } from "./useShareDirectory";

// Change who may use one machine (epic memql#5344, design section 5).
//
// ===========================================================================
// A NATIVE MODAL, FOR WHAT THE PLATFORM ALREADY DOES
// ===========================================================================
// `showModal()` puts the dialog in the top layer, makes the window beneath it
// inert and contains focus -- the InfoDetail / DetailDialog pattern, and the
// reason there is no focus trap written here. Escape is handled on the
// dialog's own keydown rather than left to the close request, for two
// reasons: a search with text in it takes the first Escape to clear itself,
// and a save in flight must not be walked away from (see `discard`).
//
// ===========================================================================
// A DRAFT UNTIL THE ENGINE ANSWERS
// ===========================================================================
// Nothing here claims a result. The floor says "Unsaved changes" -- a
// proposal, named as one -- until Save is pressed; success is the engine's
// receipt, handed to the panel, which shows it only after the write landed. A
// refusal keeps the dialog, every choice in it, and the engine's own sentence
// (SUPERVISED-VISUAL-COMPOSITION.md, "Human control and supervision").
//
// ===========================================================================
// THE PICKER IS A COMBOBOX OVER A LIST THAT STAYS OPEN
// ===========================================================================
// People and groups the owner may pick are few enough to show, so the list is
// drawn inline under the search rather than popped over it: a person who does
// not know whom they may pick can read the answer instead of guessing names.
// Focus stays in the search while arrow keys move the active option
// (`aria-activedescendant`), and Enter adds it -- type a name, Enter, the next
// name, Enter. Options take a click without taking focus.
//
// ===========================================================================
// THE ROW KEEPS MOVING; THE DRAFT REMEMBERS WHERE IT STARTED (memql#5659)
// ===========================================================================
// The draft is copied from the row once, and the row keeps arriving on the
// subscription -- another tab, another device. So the draft carries its
// STARTING value, and "the share moved" means the row differing from THAT.
// Comparing the row with the draft instead made a change from elsewhere look
// like the owner's own unsaved edit, and Save wrote the old draft back over it.
//
// What happens next depends on what the person stands to lose:
//
//   - Nothing (the draft is untouched): it follows the row, and a quiet note
//     says why the choice in front of them changed. Staleness resolves toward
//     the row, but only into an untouched draft -- Fleet > Routing's rule.
//   - Nothing to choose (the draft already says what is stored): the starting
//     point catches up, silently, because nobody's decision is at stake.
//   - Their edit: the dialog SAYS SO BEFORE SAVING, keeps the draft, and Save
//     waits for a decision -- take the new share, or keep the edit knowing it
//     replaces it. Neither side is dropped without the person choosing.
//
// ===========================================================================
// WHO IS ON THE LIST COMES FROM THE ROW; ONLY THEIR NAMES ARE READ
// ===========================================================================
// The chips are the stored ids, drawn from the machine row in every state of
// the directory read. While it is out they are the list's own shape; when it
// fails they are stand-ins ("Person 2") beside a calm notice with a way to
// read again -- still removable, so a failed read never takes the owner's
// control of their own machine with it.

const CHOICES: readonly ChoiceOption[] = [
  { value: "owner", label: "Only me", description: "Nobody else's work runs on it." },
  {
    value: "people",
    label: "Specific people and groups",
    description: "The people you choose, and the members of the groups you choose.",
  },
  {
    value: "cluster",
    label: "Everyone in this cluster",
    description: "Anyone signed in to this cluster, and the cluster's own automations.",
  },
];

/** One entry the search can offer. */
interface Candidate {
  kind: SubjectKind;
  id: string;
  name: string;
  /** A group's size, or a person's email for a caller who already sees it. */
  detail: string;
}

function unknownName(kind: SubjectKind): string {
  return kind === "group" ? "Unknown group" : "Unknown person";
}

/**
 * Everything the directory offers, GROUPS FIRST and each kind by name (the
 * engine sorts within a kind). A group is the unit most shares are about, and
 * the way to lend a machine to more people than a list would hold.
 */
function candidatesFrom(directory: ShareDirectory | null): Candidate[] {
  if (directory === null) return [];
  return [
    ...directory.groups.map((g): Candidate => ({ kind: "group", id: g.id, name: g.name, detail: groupSize(g.members) })),
    ...directory.people.map((p): Candidate => ({ kind: "person", id: p.id, name: p.name, detail: p.detail })),
  ];
}

export function ShareDialog({
  machine,
  writes,
  onDiscard,
  onSaved,
  onDirectory,
  returnFocus,
}: {
  machine: MachineRow;
  writes: MachineWrites;
  /** Cancel or Escape: the draft is thrown away and nothing is written. */
  onDiscard: () => void;
  /** The write landed; here is what the engine said it did. */
  onSaved: (receipt: SharingReceipt) => void;
  /** Every directory read that lands, so the panel can name the people and
   *  groups on the list without asking the engine a second time. */
  onDirectory?: (directory: ShareDirectory) => void;
  /** Where focus goes when the dialog closes: the control that opened it. */
  returnFocus?: () => HTMLElement | null;
}) {
  const label = machineName(machine);
  const titleId = useId();
  const machineId = useId();
  const searchId = useId();
  const listId = useId();
  const movedId = useId();
  const optionId = (index: number) => `${listId}-option-${index}`;

  const dialogRef = useRef<HTMLDialogElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const refusalRef = useRef<HTMLDivElement>(null);
  const movedRef = useRef<HTMLDivElement>(null);
  const { read, retry } = useShareDirectory(machine.id);
  const directory = read.state === "ready" ? read.directory : null;

  const [draft, setDraft] = useState<ShareDraft>(() => draftFrom(machine));
  // THE DRAFT'S STARTING VALUE: the stored share it was taken from, or last
  // caught up with (see the header, and `storedShare`).
  const [start, setStart] = useState<StoredShare>(() => storedShare(machine));
  // The draft followed a change made elsewhere and nobody has edited it since.
  // The note saying so stands exactly as long as that is true.
  const [followed, setFollowed] = useState(false);
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(-1);
  const [refused, setRefused] = useState(false);
  const [pendingFocus, setPendingFocus] = useState<number | "search" | "save" | "mode" | "follow" | null>(null);
  // What the last add or remove did, and what changed elsewhere, for a screen
  // reader: a chip appearing or going, or a choice moving under the person, is
  // otherwise silent to anybody who cannot see it.
  const [announced, setAnnounced] = useState("");

  const busy = writes.busyId === machine.id;

  // ---- the stored share, moving under the draft ----------------------------

  const storedSig = sharingSignature(machine);
  const moved = storedSig !== sharingSignature(start);
  // THE PERSON'S EDIT AND THE NEW SHARE DISAGREE: the one state in which Save
  // waits for a decision.
  const asking = moved && draftDiffers(draft, machine);

  // The row moved. LAYOUT, not passive: an untouched draft follows before the
  // browser paints, or the disagreement it resolves -- and a Save that would
  // revert the other change -- would flash for a frame first.
  //
  // KEYED ON WHAT IS STORED, not on the draft: this is the moment the row
  // moved. A draft edited back to its starting value DURING a disagreement
  // must not be swept onto the new share, which would undo the very click
  // that made it.
  useLayoutEffect(() => {
    if (!moved) return;
    if (draftUntouched(draft, start)) {
      setDraft(draftFrom(machine));
      setStart(storedShare(machine));
      setFollowed(true);
      setAnnounced("Sharing was changed somewhere else. This now shows the new sharing.");
      setPendingFocus("follow");
    } else if (draftDiffers(draft, machine)) {
      setAnnounced("Sharing was changed somewhere else. Choose which sharing to keep.");
    }
  }, [storedSig]);

  // THE DRAFT SAYS WHAT IS STORED, whichever of the two moved to make it so:
  // nothing is left to choose between, so the starting point catches up, and
  // a later edit is measured from the share as it now is.
  const draftKey = [draft.mode, ...draft.subjects.map((s) => subjectKey(s.kind, s.id)).sort()].join("|");
  useLayoutEffect(() => {
    if (moved && !draftDiffers(draft, machine)) setStart(storedShare(machine));
  }, [moved, draftKey, storedSig]);

  // THE KEYBOARD STAYS IN THE MODAL, AND OFF THE CHOICES. A Save that had
  // focus when the change landed is disabled now, and a browser drops focus
  // from a disabled control to the page behind the modal. It goes to the
  // notice -- never to one of its buttons, where the Enter the person was
  // about to press would make the decision for them.
  useEffect(() => {
    if (!asking) return;
    const held = document.activeElement;
    if (held instanceof HTMLElement && dialogRef.current?.contains(held) && !held.matches(":disabled")) return;
    movedRef.current?.focus();
  }, [asking]);

  /** A change the PERSON made. The draft is theirs from here, so a note that
   *  it shows the new share would stop being true. */
  function edit(next: (held: ShareDraft) => ShareDraft): void {
    setDraft(next);
    setFollowed(false);
  }

  /** Take the share as it is stored now, dropping the draft it replaces. */
  function takeStored(): void {
    setDraft(draftFrom(machine));
    setStart(storedShare(machine));
    setAnnounced("This now shows the new sharing.");
    setPendingFocus("mode");
  }

  /** Keep the draft, knowing that saving it replaces the new share. */
  function keepDraft(): void {
    setStart(storedShare(machine));
    setAnnounced("Your changes are kept. Saving them replaces the new sharing.");
    setPendingFocus("save");
  }

  // The latest callbacks and state, for the handlers the platform calls.
  const discardRef = useRef(onDiscard);
  discardRef.current = onDiscard;
  const busyRef = useRef(busy);
  busyRef.current = busy;
  const returnFocusRef = useRef(returnFocus);
  returnFocusRef.current = returnFocus;
  const closingRef = useRef(false);

  useEffect(() => {
    if (directory !== null) onDirectory?.(directory);
    // THE DIRECTORY IS THE DEPENDENCY: a new read is a new answer to pass on,
    // and a re-render with the same one is not.
  }, [directory]);

  // Open as a modal, put focus on the choice in force, and give focus back to
  // the opener on the way out -- whether the way out was Save, Cancel or
  // Escape. `showModal` is feature-tested for the environments that have a
  // <dialog> element with no behaviour behind it.
  useEffect(() => {
    const dialog = dialogRef.current;
    if (dialog === null) return undefined;
    // Reset on every open, not only the first: StrictMode runs this effect,
    // its cleanup and the effect again, and a flag left set by that cleanup
    // would make a later close by the platform look like one of ours.
    closingRef.current = false;
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    if (typeof dialog.showModal === "function") dialog.showModal();
    else dialog.setAttribute("open", "");
    dialog.querySelector<HTMLElement>('[role="radio"][aria-checked="true"]')?.focus();
    return () => {
      closingRef.current = true;
      if (dialog.hasAttribute("open")) {
        if (typeof dialog.close === "function") dialog.close();
        else dialog.removeAttribute("open");
      }
      const back = returnFocusRef.current?.() ?? opener;
      if (back?.isConnected) back.focus({ preventScroll: true });
    };
  }, []);

  /**
   * Leave with nothing written.
   *
   * NOT WHILE A SAVE IS IN FLIGHT. Its answer is either the receipt -- which
   * closes the dialog anyway -- or a refusal, and a refusal that arrived after
   * the dialog had gone would have nowhere to be read: the person would be
   * left believing a change that was never made.
   */
  function discard(): void {
    if (busyRef.current) return;
    discardRef.current();
  }

  async function save(): Promise<void> {
    setRefused(false);
    const receipt = await writes.setSharing(machine.id, draftChoice(draft));
    if (receipt === null) {
      setRefused(true);
      return;
    }
    onSaved(receipt);
  }

  // THE REASON GOES WHERE THE PERSON IS LOOKING. The notice is the last thing
  // in a body that scrolls, below the fold at common sizes, and the busy Save
  // took focus with it to the page: so the notice takes focus, which also
  // scrolls it into view, and a keyboard is left inside the dialog.
  useEffect(() => {
    // The notice is pinned above the floor (see the markup), so it is on
    // screen however far the body is scrolled; focus puts the keyboard on it
    // rather than on the page behind the modal, where the busy Save left it.
    if (refused) refusalRef.current?.focus();
  }, [refused]);

  // ---- the draft -----------------------------------------------------------

  const candidates = candidatesFrom(directory);
  const chosen = new Set(draft.subjects.map((s) => subjectKey(s.kind, s.id)));
  const available = candidates.filter((c) => !chosen.has(subjectKey(c.kind, c.id)));
  // A group is found by its name; a person by name, or by the email an admin
  // sees -- never by "Group of 4 people", which every group would match.
  const results = available.filter((c) => matchesQuery(query, c.name, c.kind === "person" ? c.detail : ""));
  const activeIndex = active >= 0 && active < results.length ? active : -1;

  const standIns = standInNames(draft.subjects, start);

  /**
   * A chip's name; whether it is only a stand-in for one; and whether it has
   * left what this owner may pick.
   *
   * UNREAD IS NOT UNKNOWN. With no directory there is no answer about anybody,
   * so the chip is a stand-in by kind and place ("Person 2") -- not "Unknown
   * person", which is the directory's word for somebody no row names any more,
   * and not "no longer available", which nobody has said.
   */
  function describe(subject: Subject): { name: string; standIn: boolean; stale: boolean } {
    const key = subjectKey(subject.kind, subject.id);
    if (directory === null) return { name: standIns.get(key) ?? unknownName(subject.kind), standIn: true, stale: false };
    const stored = (subject.kind === "group" ? directory.current.groups : directory.current.people).find(
      (s) => subjectKey(subject.kind, s.id) === key,
    );
    if (stored !== undefined) {
      return { name: stored.known ? stored.name : unknownName(subject.kind), standIn: false, stale: !stored.inDirectory };
    }
    const offered = candidates.find((c) => subjectKey(c.kind, c.id) === key);
    if (offered !== undefined) return { name: offered.name, standIn: false, stale: false };
    // On the row but in neither list: something no read has named.
    return { name: unknownName(subject.kind), standIn: false, stale: true };
  }

  /** A real name the directory gave, or nothing -- and `sharingSummary` then
   *  counts, which is a true sentence with nothing guessed. */
  function nameOf(subject: Subject): string | undefined {
    if (directory === null) return undefined;
    const key = subjectKey(subject.kind, subject.id);
    const stored = (subject.kind === "group" ? directory.current.groups : directory.current.people).find(
      (s) => subjectKey(subject.kind, s.id) === key,
    );
    if (stored !== undefined) return stored.known ? stored.name : undefined;
    return candidates.find((c) => subjectKey(c.kind, c.id) === key)?.name;
  }

  function pick(candidate: Candidate): void {
    edit((held) => withSubject(held, { kind: candidate.kind, id: candidate.id }));
    // The next name starts from the whole list again.
    setQuery("");
    setActive(-1);
    setAnnounced(`Added ${candidate.name}.`);
  }

  function remove(index: number, subject: Subject): void {
    edit((held) => withoutSubject(held, subject));
    setAnnounced(`Removed ${describe(subject).name}.`);
    const remaining = draft.subjects.length - 1;
    // Focus goes to a NEIGHBOUR -- the chip that took this one's place, else
    // the one before it -- so removing three in a row is three presses.
    setPendingFocus(remaining <= 0 ? "search" : Math.min(index, remaining - 1));
  }

  // Focus asked for by an act, placed once the render it waited for is in.
  // Anything that is not there to take it -- the search while the names could
  // not be read, a Save that the draft cannot use -- falls to the choice in
  // force, which always is.
  useEffect(() => {
    if (pendingFocus === null) return;
    const root = dialogRef.current;
    if (pendingFocus === "follow") {
      // A FOLLOW MOVES THE CHOICE, NOT THE PERSON. Focus on one of the choices
      // goes with the choice in force, so focus and selection do not point at
      // two different cards; focus whose control the follow took away (a
      // chip no longer on the list) lands there too. Anywhere else -- the
      // search, a chip still standing -- it stays where the person put it.
      const held = document.activeElement;
      const stays = held instanceof HTMLElement && root?.contains(held) && held.getAttribute("role") !== "radio";
      if (!stays) root?.querySelector<HTMLElement>('[role="radio"][aria-checked="true"]')?.focus();
      setPendingFocus(null);
      return;
    }
    const target =
      pendingFocus === "search"
        ? document.getElementById(searchId)
        : pendingFocus === "save"
          ? root?.querySelector<HTMLElement>(".os-actbar-acts .os-button[data-tone='primary']:not(:disabled)") ?? null
          : pendingFocus === "mode"
            ? null
            : root?.querySelectorAll<HTMLElement>(".fleet-share-chip-remove")[pendingFocus] ?? null;
    (target ?? root?.querySelector<HTMLElement>('[role="radio"][aria-checked="true"]'))?.focus();
    setPendingFocus(null);
  }, [pendingFocus, searchId]);

  // Arrowing through a long list keeps the active option in view -- by moving
  // the LIST, never the page underneath it.
  useEffect(() => {
    const list = listRef.current;
    const item = activeIndex >= 0 ? list?.children[activeIndex] : undefined;
    if (!list || !(item instanceof HTMLElement)) return;
    list.scrollTop = listScrollTop(
      { scrollTop: list.scrollTop, viewHeight: list.clientHeight },
      { top: item.offsetTop, height: item.offsetHeight },
    );
  }, [activeIndex]);

  function onSearchKeyDown(event: ReactKeyboardEvent<HTMLInputElement>): void {
    if (event.key === "ArrowDown") {
      event.preventDefault();
      if (results.length > 0) setActive(Math.min(activeIndex + 1, results.length - 1));
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      if (results.length > 0) setActive(Math.max(activeIndex - 1, 0));
    } else if (event.key === "Enter") {
      // Never a form submit: Save is its own act, and Enter here means "this one".
      event.preventDefault();
      const target = results[activeIndex];
      if (target !== undefined) pick(target);
    } else if (event.key === "Escape" && query !== "") {
      // ESCAPE UNWINDS ONE LAYER AT A TIME: the search first, then the dialog.
      event.preventDefault();
      event.stopPropagation();
      setQuery("");
      setActive(-1);
    }
  }

  const dirty = draftDiffers(draft, machine);
  const namesSomebody = draftNamesSomebody(draft);
  // NOT WHILE THERE IS A DECISION TO MAKE: that Save is the write that would
  // put the old draft back over a change made elsewhere.
  const canSave = dirty && namesSomebody && !busy && !asking;
  const floor = asking
    ? "Choose which sharing to keep."
    : refused
      ? "Not saved."
      : !namesSomebody
        ? "Choose at least one person or group."
        : dirty
          ? "Unsaved changes"
          : "Nothing changed yet.";

  return (
    <dialog
      ref={dialogRef}
      className="fleet-share-dialog"
      aria-labelledby={`${titleId} ${machineId}`}
      onKeyDown={(event) => {
        if (event.key !== "Escape") return;
        // Handled here, so the platform's close request never fires and no
        // shell listener further up hears it.
        event.preventDefault();
        event.stopPropagation();
        discard();
      }}
      onCancel={(event) => {
        // Any other close request (a back gesture): the same discard, through
        // the same guard.
        event.preventDefault();
        discard();
      }}
      onClose={(event) => {
        // The platform closed it without asking (a cancel it would not let us
        // refuse). Keep the page's state in step with what is on screen.
        //
        // ONLY IF IT IS STILL CLOSED. `close` is fired from a queued task, so
        // the one queued by StrictMode's rehearsal cleanup lands after the
        // dialog has been opened again -- and acting on it would shut a
        // dialog the person has only just opened.
        if (!closingRef.current && !event.currentTarget.open) discardRef.current();
      }}
    >
      <header className="fleet-share-head">
        <h3 id={titleId}>Change sharing</h3>
        <p id={machineId}>{label}</p>
      </header>

      <div className="fleet-share-body">
        <ChoiceStack
          name={`fleet-share-mode-${machine.id}`}
          label="Who can use this machine"
          voice="prose"
          value={draft.mode}
          onChange={(next) => {
            // The choice already in force, chosen again, is not an edit.
            if (busy || next === draft.mode) return;
            edit((held) => ({ ...held, mode: next as SharingMode }));
          }}
          options={CHOICES}
        />

        {draft.mode === "people" ? (
          <section className="fleet-share-picker" aria-label="People and groups">
            <Subhead>People and groups</Subhead>

            {read.state === "loading" && draft.subjects.length > 0 ? (
              // THE LIST'S OWN SHAPE while its names are read: a quiet pill per
              // stored entry, as many as the row holds, with nothing to focus
              // and no name guessed. The skeleton below carries the status.
              <div className="os-chips" aria-hidden="true">
                {draft.subjects.map((subject) => (
                  <span key={subjectKey(subject.kind, subject.id)} className="fleet-share-chip" data-shape>
                    <span className="os-chip">
                      <span className="os-skeleton-block fleet-share-chip-shape" />
                    </span>
                  </span>
                ))}
              </div>
            ) : null}

            {read.state !== "loading" && draft.subjects.length > 0 ? (
              <Chips label="Chosen people and groups">
                {draft.subjects.map((subject, index) => {
                  const { name, standIn, stale } = describe(subject);
                  return (
                    <span
                      key={subjectKey(subject.kind, subject.id)}
                      className="fleet-share-chip"
                      role="listitem"
                      data-stale={stale || undefined}
                      data-stand-in={standIn || undefined}
                    >
                      <Chip tone={stale ? "muted" : "neutral"}>
                        {subject.kind === "group" ? <Users size={12} aria-hidden /> : null}
                        <span className="fleet-share-chip-name">{name}</span>
                        {stale ? <span className="fleet-share-chip-note">No longer available to pick</span> : null}
                        <button
                          type="button"
                          className="os-chip-remove fleet-share-chip-remove"
                          aria-label={`Remove ${name}`}
                          disabled={busy}
                          onClick={() => remove(index, subject)}
                        >
                          <X size={12} aria-hidden />
                        </button>
                      </Chip>
                    </span>
                  );
                })}
              </Chips>
            ) : null}

            {read.state === "loading" ? <RecordListSkeleton label="Loading people and groups" /> : null}

            {read.state === "failed" ? (
              // NOT the empty state: nobody was found because nobody was asked.
              // And CALM, in the search's place: everything the row holds is
              // above, named by kind and still the owner's to remove or to
              // stop with Only me. Only the names, and anybody new, wait on a
              // second read.
              <Notice tone="warn" sentence="Names could not be loaded." detail={read.error}>
                <Button onClick={retry}>Try again</Button>
              </Notice>
            ) : null}

            {directory !== null && candidates.length === 0 ? (
              <Caption>
                {directory.everyone
                  ? "Nobody else is on this cluster yet."
                  : "Nobody shares a group with you yet. Choose Everyone, or ask an admin to add you to a group."}
              </Caption>
            ) : null}

            {directory !== null && candidates.length > 0 ? (
              <>
                <label className="os-sr-only" htmlFor={searchId}>
                  Search people and groups
                </label>
                <input
                  id={searchId}
                  className="os-input fleet-share-search"
                  type="text"
                  role="combobox"
                  aria-autocomplete="list"
                  aria-expanded={results.length > 0}
                  // Pointed at the list only while there is one: aria-controls
                  // naming an id that is not in the document is a dangling
                  // reference (the kit Select's rule).
                  aria-controls={results.length > 0 ? listId : undefined}
                  aria-activedescendant={activeIndex >= 0 ? optionId(activeIndex) : undefined}
                  autoComplete="off"
                  spellCheck={false}
                  placeholder="Search"
                  value={query}
                  disabled={busy}
                  onChange={(event) => {
                    const next = event.target.value;
                    setQuery(next);
                    // Typing makes the first match the active one, so Enter
                    // always has an answer the person can see.
                    setActive(next.trim() === "" ? -1 : 0);
                  }}
                  onKeyDown={onSearchKeyDown}
                />
                {available.length === 0 ? (
                  // THE SEARCH STAYS MOUNTED once everyone is chosen: it is
                  // where focus was when the last one was picked, and a
                  // control that vanished from under the keyboard would drop
                  // focus to the page behind a modal.
                  <Caption>Everyone you can choose is already on the list.</Caption>
                ) : results.length > 0 ? (
                  <div
                    ref={listRef}
                    id={listId}
                    role="listbox"
                    aria-label="People and groups you can choose"
                    className="fleet-share-results"
                  >
                    {results.map((candidate, index) => (
                      <div
                        key={subjectKey(candidate.kind, candidate.id)}
                        id={optionId(index)}
                        role="option"
                        aria-selected={index === activeIndex}
                        aria-label={candidate.detail ? `${candidate.name}, ${candidate.detail}` : candidate.name}
                        className="fleet-share-option"
                        data-active={index === activeIndex || undefined}
                        // Keeps focus where it is: a mousedown on a node that
                        // cannot take focus still blurs whatever had it.
                        onMouseDown={(event) => event.preventDefault()}
                        onClick={() => {
                          if (!busy) pick(candidate);
                        }}
                      >
                        {candidate.kind === "group" ? (
                          <Users size={14} aria-hidden className="fleet-share-option-icon" />
                        ) : (
                          <User size={14} aria-hidden className="fleet-share-option-icon" />
                        )}
                        <span className="fleet-share-option-name">{candidate.name}</span>
                        {candidate.detail ? <span className="fleet-share-option-detail">{candidate.detail}</span> : null}
                        <Plus size={14} aria-hidden className="fleet-share-option-add" />
                      </div>
                    ))}
                  </div>
                ) : (
                  <p className="os-caption" role="status">{`No person or group matches “${query.trim()}”.`}</p>
                )}
              </>
            ) : null}
          </section>
        ) : null}

        {draft.mode !== "owner" && machine.inferenceServe !== "cluster" ? (
          // THE CONSEQUENCE, BEFORE SAVE RATHER THAN AFTER IT: a share on a
          // machine whose own policy.yaml still says owner changes nothing
          // anybody can use yet, and the person should learn that while they
          // are deciding -- not from the receipt.
          <Notice
            tone="info"
            sentence="This machine has not agreed to serve anyone else yet."
            next="Until inference.serve is set to cluster in its policy.yaml, nobody else's work runs on it."
          />
        ) : null}

        <div className="fleet-share-terms">
          {/* THE TERMS OF THE OFFER, where the question is being asked. They
              are about lending, so they stand only while the draft lends. */}
          {draft.mode !== "owner" ? (
            <Caption>
              You see how many calls ran and for how many people. You never see what anybody asked or what the model
              answered.
            </Caption>
          ) : null}
          <Caption>Changes take effect on the next call. A call already running finishes.</Caption>
        </div>

      </div>

      {/* ONE STATUS REGION FOR THE WHOLE DIALOG, standing from the first render
          so that what it says is announced -- an add, a remove, a change made
          elsewhere. It used to live beside the search, and a chip removed
          while the names could not be read went unannounced. */}
      <p className="os-sr-only" role="status">
        {announced}
      </p>

      {asking || followed || refused ? (
        // PINNED ABOVE THE FLOOR, not at the end of the scrolling body: that
        // is below the fold at common sizes, and a reason the person has to
        // scroll to find is a reason they did not get. What is said here is
        // always on screen, beside the Save it is about.
        <div className="fleet-share-band">
          {asking ? (
            // A NAMED GROUP, because it can take focus (see the fixup above),
            // and focus landing on an unnamed box would be read as nothing.
            <div ref={movedRef} tabIndex={-1} role="group" aria-labelledby={movedId} className="fleet-share-moved">
              {/* WHAT IT IS NOW, in the panel's own words, so the choice is
                  between two things the person can see: the new share here,
                  their draft above it. Neither act writes anything -- the
                  write stays on the floor's one Save (DESIGN.md rule 12). */}
              <Notice tone="warn" sentence={<span id={movedId}>Sharing changed somewhere else while this was open.</span>}>
                <p className="os-caption">
                  Now: <span className="fleet-share-moved-now">{sharingSummary(machine, true, nameOf)}</span>
                </p>
                <div className="fleet-share-moved-acts">
                  <Button disabled={busy} onClick={takeStored}>
                    Use the new sharing
                  </Button>
                  <Button disabled={busy} onClick={keepDraft}>
                    Keep my changes
                  </Button>
                </div>
              </Notice>
            </div>
          ) : followed ? (
            // Why the choice in front of them changed, said once and quietly;
            // it goes with the person's first edit.
            <Notice
              tone="info"
              sentence="Sharing changed somewhere else while this was open."
              next="This shows the new sharing."
            />
          ) : null}
          {refused ? (
            <div ref={refusalRef} tabIndex={-1} className="fleet-share-refusal">
              {/* HONEST ABOUT WHAT IT KNOWS. A refusal changed nothing, but a
                  dropped connection may have landed the write anyway; the
                  choices are kept either way, and the panel's live line says
                  what is actually stored. */}
              <Notice
                tone="error"
                sentence="That change was not saved."
                next="Your choices are still here."
                detail={writes.actionError}
              />
            </div>
          ) : null}
        </div>
      ) : null}

      {/* THE FLOOR SAYS WHOSE TURN IT IS (DESIGN.md's wizard floor): the state
          in words on the left, the way out as a label, the one act as the
          button. Save is DISABLED rather than absent here, because the design
          record asks for it: the floor's words say what it is waiting for. */}
      <ActionBar state="" detail={floor}>
        <button type="button" className="os-actbar-text" disabled={busy} onClick={discard}>
          Cancel
        </button>
        <Button tone="primary" disabled={!canSave} busy={busy} busyLabel="Saving..." onClick={() => void save()}>
          Save
        </Button>
      </ActionBar>
    </dialog>
  );
}
