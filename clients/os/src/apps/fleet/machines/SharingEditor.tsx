import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import { useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode, type KeyboardEvent as ReactKeyboardEvent } from "react";
import { Plus, User, Users, X } from "lucide-react";

import { Button, Caption, Chip, Chips, Notice, Subhead, Switch } from "../../../kit";
import { AutoRefresh } from "../../../kit/AutoRefresh";
import { ActionBar } from "../../../kit/ActionBar";
import { listScrollTop } from "../../../kit/controls";
import type { MachineRow } from "../rows";
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

interface Candidate {
  kind: SubjectKind;
  id: string;
  name: string;
  detail: string;
}

function unknownName(kind: SubjectKind): string {
  return kind === "group" ? "Unknown group" : "Unknown person";
}

function candidatesFrom(directory: ShareDirectory | null): Candidate[] {
  if (directory === null) return [];
  return [
    ...directory.groups.map((g): Candidate => ({ kind: "group", id: g.id, name: g.name, detail: groupSize(g.members) })),
    ...directory.people.map((p): Candidate => ({ kind: "person", id: p.id, name: p.name, detail: p.detail })),
  ];
}

export function SharingEditor({ machine, writes, children }: { machine: MachineRow; writes: MachineWrites; children?: ReactNode }) {
  const searchId = useId();
  const listId = useId();
  const movedId = useId();
  const optionId = (index: number) => `${listId}-option-${index}`;

  const editorRef = useRef<HTMLElement>(null);
  const heldFocus = useRef(false);
  const listRef = useRef<HTMLDivElement>(null);
  const refusalRef = useRef<HTMLDivElement>(null);
  const movedRef = useRef<HTMLDivElement>(null);

  const [draft, setDraft] = useState<ShareDraft>(() => draftFrom(machine));
  // Read once when sharing is first revealed, retaining it through toggles.
  const [directoryNeeded, setDirectoryNeeded] = useState(machine.sharingMode !== "owner");
  const { read, refreshing, retry } = useShareDirectory(machine.id, directoryNeeded);
  const directory = read.state === "ready" ? read.directory : null;
  useEffect(() => { if (draft.mode !== "owner") setDirectoryNeeded(true); }, [draft.mode]);

  // Keep the acknowledged choice while its broadcast is in transit. A later
  // live change always wins over this temporary baseline.
  const [saved, setSaved] = useState<{
    before: string; share: StoredShare; receipt: SharingReceipt;
  } | null>(null);
  const [echoed, setEchoed] = useState(false);
  const liveSig = sharingSignature(machine);
  const current = saved !== null && !echoed && liveSig === saved.before ? saved.share : machine;
  useLayoutEffect(() => {
    if (saved !== null && liveSig !== saved.before) setEchoed(true);
  }, [liveSig, saved]);

  const [start, setStart] = useState<StoredShare>(() => storedShare(machine));
  const [followed, setFollowed] = useState(false);
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(-1);
  const [refused, setRefused] = useState(false);
  const [pendingFocus, setPendingFocus] = useState<number | "search" | "save" | "mode" | "follow" | null>(null);
  const [announced, setAnnounced] = useState("");

  const busy = writes.busyId === machine.id;

  const storedSig = sharingSignature(current);
  const moved = storedSig !== sharingSignature(start);
  const asking = moved && draftDiffers(draft, current);

  // Follow an untouched draft; never overwrite edits from a live event.
  useLayoutEffect(() => {
    if (!moved) return;
    if (draftUntouched(draft, start)) {
      setDraft(draftFrom(current));
      setStart(storedShare(current));
      setFollowed(true);
      setAnnounced("Sharing was changed somewhere else. This now shows the new sharing.");
      setPendingFocus("follow");
    } else if (draftDiffers(draft, current)) {
      setAnnounced("Sharing was changed somewhere else. Choose which sharing to keep.");
    }
  }, [storedSig]);

  const draftKey = [draft.mode, ...draft.subjects.map((s) => subjectKey(s.kind, s.id)).sort()].join("|");
  useLayoutEffect(() => {
    if (moved && !draftDiffers(draft, current)) setStart(storedShare(current));
  }, [moved, draftKey, storedSig]);

  useEffect(() => {
    if (!asking || !heldFocus.current || editorRef.current?.closest('[hidden], [aria-hidden="true"], [inert]')) return;
    const held = document.activeElement;
    if (held instanceof HTMLElement && editorRef.current?.contains(held) && !held.matches(":disabled")) return;
    movedRef.current?.focus();
  }, [asking]);

  function edit(next: (held: ShareDraft) => ShareDraft): void {
    setDraft(next);
    setFollowed(false);
  }

  function takeStored(): void {
    setDraft(draftFrom(current));
    setStart(storedShare(current));
    setAnnounced("This now shows the new sharing.");
    setPendingFocus("mode");
  }

  function keepDraft(): void {
    setStart(storedShare(current));
    setAnnounced("Your changes are kept. Saving them replaces the new sharing.");
    setPendingFocus("save");
  }

  function discard(): void {
    if (busy) return;
    setDraft(draftFrom(current));
    setStart(storedShare(current));
    setQuery("");
    setActive(-1);
    setRefused(false);
    setFollowed(false);
    setAnnounced("Changes discarded.");
    setPendingFocus("mode");
  }

  async function save(): Promise<void> {
    if (!canSave) return;
    setRefused(false);
    const choice = draftChoice(draft);
    const before = liveSig;
    const receipt = await writes.setSharing(machine.id, choice);
    if (receipt === null) {
      setRefused(true);
      return;
    }
    const share: StoredShare = { sharingMode: choice.mode, sharedUserIds: choice.userIds, sharedGroupIds: choice.groupIds };
    setSaved({ before, share, receipt });
    setEchoed(false);
    setStart(share);
    setDraft(draftFrom(share));
    setFollowed(false);
    setAnnounced("");
    setPendingFocus("mode");
  }

  useEffect(() => {
    if (refused && heldFocus.current && !editorRef.current?.closest('[hidden], [aria-hidden="true"], [inert]')) refusalRef.current?.focus();
  }, [refused]);

  const candidates = candidatesFrom(directory);
  const chosen = new Set(draft.subjects.map((s) => subjectKey(s.kind, s.id)));
  const available = candidates.filter((c) => !chosen.has(subjectKey(c.kind, c.id)));
  const results = available.filter((c) => matchesQuery(query, c.name, c.kind === "person" ? c.detail : ""));
  const activeIndex = active >= 0 && active < results.length ? active : -1;

  const standIns = standInNames(draft.subjects, start);

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
    return { name: unknownName(subject.kind), standIn: false, stale: true };
  }

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
    if (busy) return;
    edit((held) => withSubject(held, { kind: candidate.kind, id: candidate.id }));
    setQuery("");
    setActive(-1);
    setAnnounced(`Added ${candidate.name}.`);
  }

  function remove(index: number, subject: Subject): void {
    edit((held) => withoutSubject(held, subject));
    setAnnounced(`Removed ${describe(subject).name}.`);
    const remaining = draft.subjects.length - 1;
    setPendingFocus(remaining <= 0 ? "search" : Math.min(index, remaining - 1));
  }

  useEffect(() => {
    if (pendingFocus === null) return;
    if (!heldFocus.current || editorRef.current?.closest('[hidden], [aria-hidden="true"], [inert]')) { setPendingFocus(null); return; }
    const root = editorRef.current;
    if (pendingFocus === "follow") {
      const held = document.activeElement;
      const stays = held instanceof HTMLElement && root?.contains(held) && held.getAttribute("role") !== "switch";
      if (!stays && heldFocus.current && !root?.closest("[hidden]")) root?.querySelector<HTMLElement>('[role="switch"]')?.focus();
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
    (target ?? root?.querySelector<HTMLElement>('[role="switch"]'))?.focus();
    setPendingFocus(null);
  }, [pendingFocus, searchId]);

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
      event.preventDefault();
      const target = results[activeIndex];
      if (target !== undefined) pick(target);
    } else if (event.key === "Escape" && query !== "") {
      event.preventDefault();
      event.stopPropagation();
      setQuery("");
      setActive(-1);
    }
  }

  const dirty = draftDiffers(draft, current);
  const namesSomebody = draftNamesSomebody(draft);
  const canSave = draft.mode !== "cluster" && dirty && namesSomebody && !busy && !asking;
  const floor = asking
    ? "Choose which sharing to keep."
    : refused
      ? "Not saved."
      : !namesSomebody
        ? "Choose at least one person or group."
        : dirty
          ? "Unsaved changes"
          : "Nothing changed yet.";

  const showReceipt = saved !== null && !dirty && !asking &&
    sharingSignature(current) === sharingSignature(saved.share);

  return (
    <section ref={editorRef} className="fleet-share-editor" aria-label="Model sharing"
      onFocusCapture={() => { heldFocus.current = true; }}
      onBlurCapture={(event) => {
        if (event.relatedTarget instanceof Node && !event.currentTarget.contains(event.relatedTarget)) heldFocus.current = false;
      }}>

      <AutoRefresh enabled={draft.mode !== "owner"} busy={refreshing || busy} onRefresh={retry} />
      <div className="fleet-share-body">
        <div className="fleet-share-toggle">
          <Switch checked={draft.mode !== "owner"} disabled={busy} onChange={(enabled) => {
            edit((held) => ({ ...held, mode: enabled ? "people" : "owner" }));
          }}>Share models</Switch>
          <Caption>{draft.mode === "owner" ? "Only you can use this machine’s models." : "Share model inference. Machine control stays with you."}</Caption>
        </div>
        {draft.mode === "cluster" ? (
          <Notice tone="info" sentence="Currently shared with everyone in this cluster.">
            <Button disabled={busy} onClick={() => edit((held) => ({ ...held, mode: "people" }))}>Choose people and groups</Button>
          </Notice>
        ) : null}

        {draft.mode === "people" ? (
          <section className="fleet-share-picker" aria-label="People and groups">
            <Subhead>People and groups</Subhead>

            {read.state === "loading" && draft.subjects.length > 0 ? (
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

            {read.state === "ready" && read.error ? <Notice tone="warn" sentence="People and groups may be out of date." detail={read.error}>
              <Button disabled={refreshing} onClick={retry}>Try again</Button>
            </Notice> : null}

            {read.state === "failed" ? (
              <Notice tone="warn" sentence="Names could not be loaded." detail={read.error}>
                <Button onClick={retry}>Try again</Button>
              </Notice>
            ) : null}

            {directory !== null && candidates.length === 0 ? (
              <Caption>
                {directory.everyone
                  ? "Nobody else is on this cluster yet."
                  : "Nobody shares a group with you yet. Ask an admin to add you to a group."}
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
                    setActive(next.trim() === "" ? -1 : 0);
                  }}
                  onKeyDown={onSearchKeyDown}
                />
                {available.length === 0 ? (
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

        {children}
        {dirty ? <Caption>Changes take effect on the next call. A call already running finishes.</Caption> : null}

      </div>

      <p className="os-sr-only" role="status">
        {announced}
      </p>

      {asking || followed || refused ? (
        <div className="fleet-share-band">
          {asking ? (
            <div ref={movedRef} tabIndex={-1} role="group" aria-labelledby={movedId} className="fleet-share-moved">
              <Notice tone="warn" sentence={<span id={movedId}>Sharing changed somewhere else while this was open.</span>}>
                <p className="os-caption">
                  Now: <span className="fleet-share-moved-now">{sharingSummary(current, true, nameOf)}</span>
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
            <Notice
              tone="info"
              sentence="Sharing changed somewhere else while this was open."
              next="This shows the new sharing."
            />
          ) : null}
          {refused ? (
            <div ref={refusalRef} tabIndex={-1} className="fleet-share-refusal">
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

      {dirty || busy || asking || refused ? <ActionBar state="" detail={floor}>
        <button type="button" className="os-actbar-text" disabled={busy} onClick={discard}>
          Discard changes
        </button>
        <Button tone="primary" disabled={!canSave} busy={busy} busyLabel="Saving..." onClick={() => void save()}>
          Save
        </Button>
      </ActionBar> : null}
      {showReceipt ? <p className="os-caption fleet-share-receipt" role="status">{saved.receipt.sentence}</p> : null}
    </section>
  );
}
