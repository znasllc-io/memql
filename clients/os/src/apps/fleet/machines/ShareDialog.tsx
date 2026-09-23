import { useEffect, useId, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from "react";
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
  groupSize,
  matchesQuery,
  subjectKey,
  withSubject,
  withoutSubject,
  type ShareDraft,
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
  const optionId = (index: number) => `${listId}-option-${index}`;

  const dialogRef = useRef<HTMLDialogElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const refusalRef = useRef<HTMLDivElement>(null);
  const { read, retry } = useShareDirectory(machine.id);
  const directory = read.state === "ready" ? read.directory : null;

  const [draft, setDraft] = useState<ShareDraft>(() => draftFrom(machine));
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(-1);
  const [refused, setRefused] = useState(false);
  const [pendingFocus, setPendingFocus] = useState<number | "search" | null>(null);
  // What the last add or remove did, for a screen reader: a chip appearing or
  // going is otherwise silent to anybody who cannot see it.
  const [announced, setAnnounced] = useState("");

  const busy = writes.busyId === machine.id;

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

  /** A chip's name, and whether it has left what this owner may pick. */
  function describe(subject: Subject): { name: string; stale: boolean } {
    const key = subjectKey(subject.kind, subject.id);
    const stored = (subject.kind === "group" ? directory?.current.groups : directory?.current.people)?.find(
      (s) => subjectKey(subject.kind, s.id) === key,
    );
    if (stored !== undefined) {
      return { name: stored.known ? stored.name : unknownName(subject.kind), stale: !stored.inDirectory };
    }
    const offered = candidates.find((c) => subjectKey(c.kind, c.id) === key);
    if (offered !== undefined) return { name: offered.name, stale: false };
    // On the row but in neither list: something no read has named.
    return { name: unknownName(subject.kind), stale: true };
  }

  function pick(candidate: Candidate): void {
    setDraft((held) => withSubject(held, { kind: candidate.kind, id: candidate.id }));
    // The next name starts from the whole list again.
    setQuery("");
    setActive(-1);
    setAnnounced(`Added ${candidate.name}.`);
  }

  function remove(index: number, subject: Subject): void {
    setDraft((held) => withoutSubject(held, subject));
    setAnnounced(`Removed ${describe(subject).name}.`);
    const remaining = draft.subjects.length - 1;
    // Focus goes to a NEIGHBOUR -- the chip that took this one's place, else
    // the one before it -- so removing three in a row is three presses.
    setPendingFocus(remaining <= 0 ? "search" : Math.min(index, remaining - 1));
  }

  useEffect(() => {
    if (pendingFocus === null) return;
    const root = dialogRef.current;
    const target =
      pendingFocus === "search"
        ? document.getElementById(searchId)
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
  const canSave = dirty && namesSomebody && !busy;
  const floor = refused
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
            if (busy) return;
            setDraft((held) => ({ ...held, mode: next as SharingMode }));
          }}
          options={CHOICES}
        />

        {draft.mode === "people" ? (
          <section className="fleet-share-picker" aria-label="People and groups">
            <Subhead>People and groups</Subhead>

            {read.state === "loading" ? (
              <p className="os-caption" role="status">
                Reading who you can share with…
              </p>
            ) : null}

            {read.state === "failed" ? (
              // NOT the empty state: nobody was found because nobody was asked.
              <Notice tone="error" sentence="Could not read who you can share with." detail={read.error}>
                <Button onClick={retry}>Try again</Button>
              </Notice>
            ) : null}

            {directory !== null && draft.subjects.length > 0 ? (
              <Chips label="Chosen people and groups">
                {draft.subjects.map((subject, index) => {
                  const { name, stale } = describe(subject);
                  return (
                    <span
                      key={subjectKey(subject.kind, subject.id)}
                      className="fleet-share-chip"
                      role="listitem"
                      data-stale={stale || undefined}
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
                <p className="os-sr-only" role="status">
                  {announced}
                </p>
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

      {refused ? (
        // PINNED ABOVE THE FLOOR, not at the end of the scrolling body: that
        // is below the fold at common sizes, and a reason the person has to
        // scroll to find is a reason they did not get. Here it is always on
        // screen, beside the Save that produced it.
        <div ref={refusalRef} tabIndex={-1} className="fleet-share-refusal">
          {/* HONEST ABOUT WHAT IT KNOWS. A refusal changed nothing, but a
              dropped connection may have landed the write anyway; the choices
              are kept either way, and the panel's live line says what is
              actually stored. */}
          <Notice
            tone="error"
            sentence="That change was not saved."
            next="Your choices are still here."
            detail={writes.actionError}
          />
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
