import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { Check, LoaderCircle, Pause, Play, Trash2, User, Users } from "lucide-react";
import { sameEntityId } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, Notice, Switch } from "../../../kit";
import { IconButton } from "../../../kit/IconButton";
import { AutoRefresh } from "../../../kit/AutoRefresh";
import { CardCollectionSearch, EditableCard, CardCollectionSkeleton } from "../../../kit/CardCollectionEditor";
import { CardCarousel } from "../../../kit/CardCarousel";
import { AnchoredPicker } from "../../../kit/AnchoredPicker";
import { listScrollTop } from "../../../kit/controls";
import { isWorkerOnline } from "../online";
import type { MachineRow } from "../rows";
import { groupSize, matchesQuery, sharingSignature, standInNames, storedSubjects, subjectKey, type StoredShare, type Subject, type SubjectKind } from "./sharing";
import type { MachineWrites, ShareChoice, SharingResource } from "./useMachineWrites";
import { useShareDirectory } from "./useShareDirectory";

interface Candidate { kind: SubjectKind; id: string; name: string; detail: string }
interface ShareDraft { subject: Subject; name: string; paused: boolean; baseline: string }

function shareChoice(mode: StoredShare["sharingMode"], subjects: Subject[], paused: boolean): ShareChoice {
  return {
    mode,
    paused,
    userIds: subjects.filter((s) => s.kind === "person").map((s) => s.id),
    groupIds: subjects.filter((s) => s.kind === "group").map((s) => s.id),
    pausedUserIds: subjects.filter((s) => s.kind === "person" && s.paused).map((s) => s.id),
    pausedGroupIds: subjects.filter((s) => s.kind === "group" && s.paused).map((s) => s.id),
  };
}

/** Counts describe the grants at admission, so pausing does not erase draining work. */
export function sharingActiveCalls(machine: MachineRow, subject: Subject, now = Date.now()): number | null {
  if (!isWorkerOnline(machine, new Date(now))) return 0;
  const report = machine.sharingActivity;
  if (!report || report["nodeId"] !== machine.connectedNodeId) return null;
  const at = Date.parse(String(report["observedAt"] ?? ""));
  if (!Number.isFinite(at) || now - at > 45_000) return null;
  const rows = report[subject.kind === "person" ? "users" : "groups"];
  if (!Array.isArray(rows)) return null;
  const entry = rows.find((v: unknown) => v !== null && typeof v === "object" && sameEntityId(String((v as Record<string, unknown>)["id"] ?? ""), subject.id));
  if (!entry) return 0;
  const count = (entry as Record<string, unknown>)["activeCalls"];
  return typeof count === "number" && Number.isSafeInteger(count) && count >= 0 ? count : null;
}

export function SharingEditor({ machine, writes, resource = "models" }: { machine: MachineRow; writes: MachineWrites; resource?: SharingResource }) {
  const title = resource === "models" ? "Model sharing" : "Fleet workbench sharing";
  const toggleLabel = resource === "models" ? "Share models" : "Share fleet workbenches";
  const searchId = useId(), listId = useId();
  const root = useRef<HTMLElement>(null), search = useRef<HTMLInputElement>(null);
  const searchArea = useRef<HTMLDivElement>(null), popup = useRef<HTMLDivElement>(null), loadMore = useRef<HTMLButtonElement>(null);
  const inFlight = useRef(false);
  const [pending, setPending] = useState("");
  const [saved, setSaved] = useState<{ before: string; share: StoredShare } | null>(null);
  const liveSignature = sharingSignature(machine);
  const current = saved && liveSignature === saved.before ? saved.share : machine;
  useEffect(() => { if (saved && liveSignature !== saved.before) setSaved(null); }, [liveSignature, saved]);
  const currentRef = useRef(current); currentRef.current = current;
  const [failed, setFailed] = useState<{ choice: ShareChoice; baseline: string; finishEdit: boolean; addedKeys: string[] } | null>(null);
  const [query, setQuery] = useState("");
  const [searchBy, setSearchBy] = useState("all");
  const [draft, setDraft] = useState<ShareDraft | null>(null);
  const [revealKey, setRevealKey] = useState<string>();
  const [searching, setSearching] = useState(false);
  const [active, setActive] = useState(-1);
  const [visibleCount, setVisibleCount] = useState(5);
  const [chosenKeys, setChosenKeys] = useState<string[]>([]);
  const [focusAfter, setFocusAfter] = useState<string | null>(null);
  const subjects = useMemo(() => storedSubjects(current), [current]);
  const [directoryNeeded, setDirectoryNeeded] = useState(current.sharingMode !== "owner");
  useEffect(() => { if (current.sharingMode !== "owner") setDirectoryNeeded(true); }, [current.sharingMode]);
  const { read, refreshing, retry } = useShareDirectory(machine.id, directoryNeeded, resource);
  const directory = read.state === "ready" ? read.directory : null;
  const busy = pending !== "" || writes.busyId === machine.id;
  const candidates: Candidate[] = directory ? [
    ...directory.groups.map((g): Candidate => ({ kind: "group", id: g.id, name: g.name, detail: groupSize(g.members) })),
    ...directory.people.map((p): Candidate => ({ kind: "person", id: p.id, name: p.name, detail: p.detail })),
  ].sort((a, b) => a.name.localeCompare(b.name) || subjectKey(a.kind, a.id).localeCompare(subjectKey(b.kind, b.id))) : [];
  const selected = new Set(subjects.map((s) => subjectKey(s.kind, s.id)));
  const available = candidates.filter(c => !selected.has(subjectKey(c.kind, c.id)) && (searchBy === "all" || c.kind === searchBy) && matchesQuery(query, c.name, c.detail));
  const results = available.slice(0, visibleCount);
  const hasMore = available.length > results.length;
  const eligible = new Set(candidates.filter(c => !selected.has(subjectKey(c.kind, c.id))).map(c => subjectKey(c.kind, c.id)));
  const selection = chosenKeys.filter(key => eligible.has(key));
  const selectionSignature = JSON.stringify(selection);
  useEffect(() => {
    if (read.state === "ready") setChosenKeys(previous => previous.length === selection.length && previous.every((key, index) => key === selection[index]) ? previous : selection);
  }, [selectionSignature, read.state]);
  const activeIndex = active >= 0 && active < results.length ? active : -1;
  const standIns = standInNames(subjects, current);

  function describe(subject: Subject): { name: string; stale: boolean } {
    const matches = (s: { id: string }) => sameEntityId(s.id, subject.id);
    const stored = (subject.kind === "group" ? directory?.current.groups : directory?.current.people)?.find(matches);
    const offered = candidates.find((c) => c.kind === subject.kind && matches(c));
    return { name: offered?.name ?? (stored?.known ? stored.name : undefined) ?? (directory ? `Unknown ${subject.kind === "group" ? "group" : "user"}` : standIns.get(subjectKey(subject.kind, subject.id)) ?? ""), stale: !!directory && !offered && !stored?.inDirectory };
  }

  function choiceFor(mode: StoredShare["sharingMode"], selected: Subject[]): ShareChoice { return shareChoice(mode, selected, currentRef.current.sharingPaused === true); }

  async function persist(choice: ShareChoice, key: string, focus?: string, finishEdit = false, addedKeys: string[] = []): Promise<boolean> {
    if (inFlight.current) return false;
    const baseline = sharingSignature(currentRef.current);
    const startedHere = root.current?.contains(document.activeElement);
    inFlight.current = true; setPending(key); setFailed(null);
    try {
      const receipt = resource === "models" ? await writes.setSharing(machine.id, choice) : await writes.setSharing(machine.id, choice, resource);
      if (!receipt) { setFailed({ choice, baseline, finishEdit, addedKeys }); return false; }
      const share: StoredShare = { sharingMode: choice.mode, sharingPaused: choice.paused ?? false, sharedUserIds: choice.mode === "people" ? choice.userIds : [], sharedGroupIds: choice.mode === "people" ? choice.groupIds : [], pausedUserIds: choice.pausedUserIds ?? [], pausedGroupIds: choice.pausedGroupIds ?? [] };
      setSaved({ before: liveSignature, share });
      if (finishEdit) setDraft(null);
      if (addedKeys.length) {
        setChosenKeys(keys => keys.filter(key => !addedKeys.includes(key)));
        setQuery(""); setActive(-1); setSearching(false); setVisibleCount(5);
        setRevealKey(addedKeys[0]);
      }
      if (focus && startedHere && !root.current?.closest('[hidden], [inert], [aria-hidden="true"]') && (root.current?.contains(document.activeElement) || document.activeElement === document.body)) setFocusAfter(focus);
      return true;
    } finally { inFlight.current = false; setPending(""); }
  }

  useEffect(() => {
    if (!focusAfter) return;
    if (root.current?.closest('[hidden], [inert], [aria-hidden="true"]')) { setFocusAfter(null); return; }
    const target = Array.from(root.current?.querySelectorAll<HTMLElement>("[data-share-action]") ?? []).find((node) => node.dataset.shareAction === focusAfter);
    (target && !target.closest("[inert]") ? target : search.current)?.focus(); setFocusAfter(null);
    setSearching(false);
  }, [focusAfter, subjects]);

  useEffect(() => {
    if (!root.current?.closest('[hidden], [inert], [aria-hidden="true"]')) root.current?.querySelector<HTMLInputElement>(".os-card-draft input")?.focus();
  }, [draft?.subject.kind, draft?.subject.id]);

  function toggleCandidate(candidate: Candidate) {
    if (busy || draft) return;
    const key = subjectKey(candidate.kind, candidate.id);
    setChosenKeys(keys => keys.includes(key) ? keys.filter(item => item !== key) : [...keys, key]);
  }

  async function addSelection() {
    if (!selection.length || busy || draft) return;
    const added = candidates.filter(c => selection.includes(subjectKey(c.kind, c.id))).map(c => ({ kind: c.kind, id: c.id }));
    await persist(choiceFor("people", [...storedSubjects(currentRef.current), ...added]), "add", "search", false, selection);
  }

  function showMore() {
    if (!hasMore || busy) return;
    setVisibleCount(count => count + 5);
    setActive(results.length);
    search.current?.focus();
  }

  function closeSearch() { setSearching(false); setActive(-1); }

  function onSearchKeyDown(event: KeyboardEvent<HTMLInputElement>): void {
    if (event.nativeEvent.isComposing) return;
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault(); setSearching(true);
      if (searching && event.key === "ArrowDown" && activeIndex === results.length - 1 && hasMore) loadMore.current?.focus();
      else if (results.length) setActive(event.key === "ArrowDown" ? Math.min(activeIndex + 1, results.length - 1) : Math.max(activeIndex - 1, 0));
    } else if (event.key === "Enter" && searching && results[activeIndex]) {
      event.preventDefault(); toggleCandidate(results[activeIndex]);
    } else if (event.key === "Escape") { event.preventDefault(); event.stopPropagation(); closeSearch(); }
    else if (event.key === "Tab") closeSearch();
  }
  useEffect(() => {
    const list = popup.current, item = document.getElementById(`${listId}-${activeIndex}`);
    if (!searching || !list || !item) return;
    list.scrollTop = listScrollTop({ scrollTop: list.scrollTop, viewHeight: list.clientHeight }, { top: item.offsetTop, height: item.offsetHeight });
  }, [searching, activeIndex, visibleCount, listId]);

  const matches = subjects.filter(subject => {
    const name = describe(subject).name;
    const candidate = candidates.find(c => c.kind === subject.kind && sameEntityId(c.id, subject.id));
    return (searchBy === "all" || subject.kind === searchBy) && matchesQuery(query, name, candidate?.detail ?? "");
  });
  const canRetry = failed !== null && sharingSignature(current) === failed.baseline && (!failed.addedKeys.length || (failed.addedKeys.length === selection.length && failed.addedKeys.every(key => selection.includes(key))));
  const conflict = draft !== null && sharingSignature(current) !== draft.baseline;
  const locked = busy || draft !== null;

  function beginEdit(subject: Subject) {
    if (locked) return;
    setDraft({ subject, name: describe(subject).name, paused: subject.paused === true, baseline: sharingSignature(current) });
    setSearching(false); setFailed(null);
  }

  function cancelEdit() { setDraft(null); setFailed(null); setFocusAfter("search"); }

  async function saveDraft() {
    if (!draft || conflict || busy) return;
    const key = subjectKey(draft.subject.kind, draft.subject.id);
    await persist(choiceFor("people", subjects.map(s => subjectKey(s.kind, s.id) === key ? { ...s, paused: draft.paused } : s)), key, "search", true);
  }

  return <section ref={root} className="fleet-share-editor" aria-label={title} aria-busy={busy || undefined}>
    <AutoRefresh enabled={current.sharingMode !== "owner"} busy={busy || refreshing} onRefresh={retry} />
    <div className="fleet-share-body">
      <div className="fleet-share-toggle">
        <div><span>{toggleLabel}</span><Caption>{current.sharingPaused ? "Paused. Your audience is saved; running work can finish." : current.sharingMode === "owner" ? "Only you have access." : resource === "models" ? "Share inference. Machine control stays with you." : "Share isolated workbenches. Host access stays with you."}</Caption></div>
        <Switch checked={current.sharingMode !== "owner" && !current.sharingPaused} disabled={locked} onChange={enabled => void persist({ ...choiceFor(current.sharingMode === "owner" ? "people" : current.sharingMode, subjects), paused: !enabled }, "mode")}><span className="os-sr-only">{toggleLabel}</span></Switch>
      </div>
      {current.sharingMode === "cluster" ? <Notice tone="info" sentence="Currently shared with everyone in this cluster."><Button disabled={locked} onClick={() => void persist(choiceFor("people", []), "mode")}>Choose users and groups</Button></Notice> : null}
      {current.sharingMode === "people" ? <section className="fleet-machine-setting fleet-share-picker" aria-label="Users and groups">
        <span className="fleet-machine-setting-name">Users and groups</span>
        <div className="fleet-share-collection">
          <div ref={searchArea} className="fleet-share-search-area">
            <CardCollectionSearch label="Find or add users and groups" query={query} inputRef={search} disabled={locked || !directory}
              mode={{ label: "Search users and groups by", value: searchBy, options: [["all", "All"], ["person", "Users"], ["group", "Groups"]], onChange: value => { setSearchBy(value); setActive(-1); setVisibleCount(5); setRevealKey(undefined); } }}
              onQueryChange={value => { setQuery(value); setSearching(true); setActive(0); setVisibleCount(5); setRevealKey(undefined); }} clearLabel="Clear sharing search"
              add={{ label: "Add selected users and groups", enabled: selection.length > 0, onClick: () => { void addSelection(); } }}
              inputProps={{ id: searchId, "aria-label": "Search users and groups", role: "combobox", "aria-autocomplete": "list", "aria-expanded": searching && !locked,
                "aria-controls": searching && !locked ? listId : undefined, "aria-activedescendant": searching && !locked && activeIndex >= 0 ? `${listId}-${activeIndex}` : undefined,
                "aria-describedby": `${searchId}-selection`, autoComplete: "off", spellCheck: false, onFocus: () => setSearching(true), onClick: () => setSearching(true), onKeyDown: onSearchKeyDown }} />
            {searching && !locked ? <AnchoredPicker anchor={search} owner={searchArea} popupRef={popup} onClose={closeSearch}>
              <div id={listId} role="listbox" aria-multiselectable="true" aria-label="Users and groups to add">
                {results.map((candidate, index) => {
                  const key = subjectKey(candidate.kind, candidate.id), chosen = selection.includes(key);
                  return <div key={key} id={`${listId}-${index}`} role="option" aria-selected={chosen} aria-label={candidate.name} className="fleet-share-option" data-active={index === activeIndex || undefined} onMouseDown={event => event.preventDefault()} onClick={() => { setActive(index); toggleCandidate(candidate); }}>
                    {candidate.kind === "group" ? <Users size={14} aria-hidden className="fleet-share-option-icon" /> : <User size={14} aria-hidden className="fleet-share-option-icon" />}
                    <span className="fleet-share-option-name">{candidate.name}</span>
                    {candidate.detail ? <span className="fleet-share-option-detail">{candidate.detail}</span> : null}
                    <span className="fleet-share-option-check" aria-hidden>{chosen ? <Check size={12} /> : null}</span>
                  </div>;
                })}
              </div>
              {!results.length ? <p className="os-picker-empty">{query.trim() ? "No matches" : "No users or groups to add"}</p> : null}
              {hasMore ? <button ref={loadMore} type="button" className="os-picker-more" onMouseDown={event => event.preventDefault()} onClick={showMore}
                onKeyDown={event => {
                  if (event.key === "ArrowUp") { event.preventDefault(); search.current?.focus(); setActive(results.length - 1); }
                  if (event.key === "Escape") { event.preventDefault(); event.stopPropagation(); search.current?.focus(); closeSearch(); }
                  if (event.key === "Tab") { event.preventDefault(); search.current?.focus(); closeSearch(); searchArea.current?.querySelector<HTMLButtonElement>(".os-add-button")?.focus(); }
                }}>Load more</button> : null}
            </AnchoredPicker> : null}
          </div>
          <p className="os-caption fleet-share-selection-count" id={`${searchId}-selection`} role="status" aria-live="polite" aria-atomic="true">{selection.length} selected</p>
          {draft ? <div className="os-card-draft" role="group" aria-label={`Edit sharing with ${draft.name}`} onKeyDown={event => { if (event.key === "Escape" && !busy) { event.preventDefault(); event.stopPropagation(); cancelEdit(); } }}>
            <div className="os-card-draft-title">Sharing with <strong>{draft.name}</strong></div>
            <Switch checked={!draft.paused} disabled={busy} onChange={enabled => setDraft({ ...draft, paused: !enabled })}>Allow new calls</Switch>
            {conflict && !busy ? <Notice tone="warn" sentence="Sharing changed elsewhere." next="Cancel and reopen the card to edit current access." /> : null}
            <div className="os-panel-actions">
              <Button disabled={busy} onClick={cancelEdit}>Cancel</Button>
              <Button tone="primary" disabled={conflict || draft.paused === (draft.subject.paused === true)} busy={busy} busyLabel="Saving..." onClick={() => void saveDraft()}>Save</Button>
            </div>
          </div> : null}
          {read.state === "loading" ? <CardCollectionSkeleton label="Loading users and groups" /> : matches.length ? <CardCarousel key={`${searchBy}:${query}`} items={matches} itemKey={subject => subjectKey(subject.kind, subject.id)} revealKey={revealKey} minCardWidth={240}
            label="Sharing access cards" listLabel="Shared users and groups" noun="entry" plural="entries" renderItem={subject => {
              const key = subjectKey(subject.kind, subject.id), { name, stale } = describe(subject);
              const count = sharingActiveCalls(machine, subject);
              return <EditableCard label={`Edit sharing with ${name}`} disabled={locked} onEdit={() => beginEdit(subject)}
                meta={<span className="fleet-share-calls" title={count === null ? "Active calls unavailable" : undefined}>{count === null ? "— active calls" : `${count} active ${count === 1 ? "call" : "calls"}`}</span>} actions={<>
                <IconButton data-share-action={key} label={`${subject.paused ? "Resume" : "Pause"} sharing with ${name}`} disabled={locked} onClick={() => void persist(choiceFor("people", subjects.map(s => subjectKey(s.kind, s.id) === key ? { ...s, paused: !s.paused } : s)), key, key)}>{pending === key ? <LoaderCircle size={15} aria-hidden className="os-spin" /> : subject.paused ? <Play size={15} aria-hidden /> : <Pause size={15} aria-hidden />}</IconButton>
                <IconButton className="os-card-remove" label={`Remove sharing with ${name}`} disabled={locked} onClick={() => { setRevealKey(undefined); void persist(choiceFor("people", subjects.filter(s => subjectKey(s.kind, s.id) !== key)), `remove:${key}`, "search"); }}><Trash2 size={15} aria-hidden /></IconButton>
              </>}>
                <strong>{name}</strong>
                <span>{stale ? "Unavailable" : subject.kind === "group" ? "Group" : "User"}{subject.paused ? <> · <em>Paused</em></> : null}</span>
              </EditableCard>;
            }} /> : !draft && !searching && query.trim() && !available.length && read.state === "ready" ? <Caption>No matching users or groups.</Caption> : null}
          {read.state === "failed" || (read.state === "ready" && read.error) ? <Notice tone="warn" sentence="Users and groups could not be refreshed." detail={read.error}><Button disabled={refreshing} onClick={retry}>Try again</Button></Notice> : null}
        </div>
      </section> : null}
    </div>
    {failed ? <div className="fleet-share-error"><Notice tone="error" sentence="Sharing could not be updated." detail={writes.actionErrorScope === (resource === "models" ? "sharing" : "workbenchSharing") ? writes.actionError : undefined}>
      {canRetry ? <Button disabled={busy} onClick={() => void persist(failed.choice, "retry", failed.finishEdit || failed.addedKeys.length ? "search" : undefined, failed.finishEdit, failed.addedKeys)}>Try again</Button> : <Caption>{failed.addedKeys.length ? "Selection or access changed. Add the current selection." : "Sharing changed elsewhere. Choose the action again."}</Caption>}
    </Notice></div> : null}
  </section>;
}
