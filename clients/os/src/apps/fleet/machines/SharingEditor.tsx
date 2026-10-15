import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { LoaderCircle, Pause, Play, Plus, Trash2, User, Users } from "lucide-react";
import { sameEntityId } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, LiveList, Notice, RecordRow, Subhead, Switch } from "../../../kit";
import { IconButton } from "../../../kit/IconButton";
import { AutoRefresh } from "../../../kit/AutoRefresh";
import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import { isWorkerOnline } from "../online";
import type { MachineRow } from "../rows";
import { groupSize, matchesQuery, sharingSignature, standInNames, storedSubjects, subjectKey, type StoredShare, type Subject, type SubjectKind } from "./sharing";
import type { MachineWrites, ShareChoice } from "./useMachineWrites";
import { useShareDirectory } from "./useShareDirectory";

interface Candidate { kind: SubjectKind; id: string; name: string; detail: string }

function choiceFor(mode: StoredShare["sharingMode"], subjects: Subject[]): ShareChoice {
  return {
    mode,
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

export function SharingEditor({ machine, writes }: { machine: MachineRow; writes: MachineWrites }) {
  const searchId = useId(), listId = useId();
  const root = useRef<HTMLElement>(null), search = useRef<HTMLInputElement>(null);
  const inFlight = useRef(false);
  const [pending, setPending] = useState("");
  const [saved, setSaved] = useState<{ before: string; share: StoredShare } | null>(null);
  const liveSignature = sharingSignature(machine);
  const current = saved && liveSignature === saved.before ? saved.share : machine;
  useEffect(() => { if (saved && liveSignature !== saved.before) setSaved(null); }, [liveSignature, saved]);
  const currentRef = useRef(current); currentRef.current = current;
  const [failed, setFailed] = useState<{ choice: ShareChoice; baseline: string } | null>(null);
  const [query, setQuery] = useState("");
  const [searching, setSearching] = useState(false);
  const [active, setActive] = useState(-1);
  const [focusAfter, setFocusAfter] = useState<string | null>(null);
  const subjects = useMemo(() => storedSubjects(current), [current]);
  const [directoryNeeded, setDirectoryNeeded] = useState(current.sharingMode !== "owner");
  useEffect(() => { if (current.sharingMode !== "owner") setDirectoryNeeded(true); }, [current.sharingMode]);
  const { read, refreshing, retry } = useShareDirectory(machine.id, directoryNeeded);
  const directory = read.state === "ready" ? read.directory : null;
  const busy = pending !== "" || writes.busyId === machine.id;
  const candidates: Candidate[] = directory ? [
    ...directory.groups.map((g): Candidate => ({ kind: "group", id: g.id, name: g.name, detail: groupSize(g.members) })),
    ...directory.people.map((p): Candidate => ({ kind: "person", id: p.id, name: p.name, detail: p.detail })),
  ] : [];
  const selected = new Set(subjects.map((s) => subjectKey(s.kind, s.id)));
  const results = searching ? candidates.filter((c) => !selected.has(subjectKey(c.kind, c.id)) && matchesQuery(query, c.name, c.detail)) : [];
  const activeIndex = active >= 0 && active < results.length ? active : -1;
  const standIns = standInNames(subjects, current);

  function describe(subject: Subject): { name: string; stale: boolean } {
    const matches = (s: { id: string }) => sameEntityId(s.id, subject.id);
    const stored = (subject.kind === "group" ? directory?.current.groups : directory?.current.people)?.find(matches);
    const offered = candidates.find((c) => c.kind === subject.kind && matches(c));
    return { name: offered?.name ?? (stored?.known ? stored.name : undefined) ?? (directory ? `Unknown ${subject.kind === "group" ? "group" : "user"}` : standIns.get(subjectKey(subject.kind, subject.id)) ?? ""), stale: !!directory && !offered && !stored?.inDirectory };
  }

  async function persist(choice: ShareChoice, key: string, focus?: string): Promise<boolean> {
    if (inFlight.current) return false;
    const baseline = sharingSignature(currentRef.current);
    inFlight.current = true; setPending(key); setFailed(null);
    try {
      const receipt = await writes.setSharing(machine.id, choice);
      if (!receipt) { setFailed({ choice, baseline }); return false; }
      const share: StoredShare = { sharingMode: choice.mode, sharedUserIds: choice.mode === "people" ? choice.userIds : [], sharedGroupIds: choice.mode === "people" ? choice.groupIds : [], pausedUserIds: choice.pausedUserIds ?? [], pausedGroupIds: choice.pausedGroupIds ?? [] };
      setSaved({ before: liveSignature, share });
      if (focus && root.current?.contains(document.activeElement)) setFocusAfter(focus);
      return true;
    } finally { inFlight.current = false; setPending(""); }
  }

  useEffect(() => {
    if (!focusAfter || root.current?.closest('[hidden], [inert], [aria-hidden="true"]')) return;
    const target = Array.from(root.current?.querySelectorAll<HTMLElement>("[data-share-action]") ?? []).find((node) => node.dataset.shareAction === focusAfter);
    (target ?? search.current)?.focus(); setFocusAfter(null);
  }, [focusAfter, subjects]);

  async function pick(candidate: Candidate): Promise<void> {
    if (await persist(choiceFor("people", [...subjects, { kind: candidate.kind, id: candidate.id }]), subjectKey(candidate.kind, candidate.id))) {
      setQuery(""); setActive(-1); setSearching(false);
      if (!root.current?.closest('[hidden], [inert], [aria-hidden="true"]') && root.current?.contains(document.activeElement)) search.current?.focus();
    }
  }

  function onSearchKeyDown(event: KeyboardEvent<HTMLInputElement>): void {
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault(); setSearching(true);
      if (results.length) setActive(event.key === "ArrowDown" ? Math.min(activeIndex + 1, results.length - 1) : Math.max(activeIndex - 1, 0));
    } else if (event.key === "Enter" && results[activeIndex]) {
      event.preventDefault(); void pick(results[activeIndex]);
    } else if (event.key === "Escape") { event.preventDefault(); event.stopPropagation(); setSearching(false); setActive(-1); }
  }
  useEffect(() => { if (activeIndex >= 0) document.getElementById(`${listId}-${activeIndex}`)?.scrollIntoView?.({ block: "nearest" }); }, [activeIndex, listId]);

  const revision = useRef({ key: "", version: 0 });
  const listKey = sharingSignature(current) + JSON.stringify(directory);
  if (revision.current.key !== listKey) revision.current = { key: listKey, version: revision.current.version + 1 };
  const listVersion = revision.current.version;
  const source = useMemo(() => ({ subscribe: () => () => {}, snapshot: { rows: subjects, state: "live" as const, error: "", version: listVersion } }), [subjects, listVersion]);
  const canRetry = failed !== null && sharingSignature(current) === failed.baseline;

  return <section ref={root} className="fleet-share-editor" aria-label="Model sharing" aria-busy={busy || undefined}>
    <AutoRefresh enabled={current.sharingMode !== "owner"} busy={refreshing || busy} onRefresh={retry} />
    <div className="fleet-share-body">
      <div className="fleet-share-toggle">
        <Switch checked={current.sharingMode !== "owner"} disabled={busy} onChange={(enabled) => void persist(choiceFor(enabled ? "people" : "owner", enabled ? subjects : []), "mode")}>Share models</Switch>
        <Caption>{current.sharingMode === "owner" ? "Only you can use this machine’s models." : "Share model inference. Machine control stays with you."}</Caption>
      </div>
      {current.sharingMode === "cluster" ? <Notice tone="info" sentence="Currently shared with everyone in this cluster."><Button disabled={busy} onClick={() => void persist(choiceFor("people", []), "mode")}>Choose users and groups</Button></Notice> : null}
      {current.sharingMode === "people" ? <section className="fleet-share-picker" aria-label="Users and groups">
        <Subhead>Users and groups</Subhead>
        <div className="fleet-share-search-area" onBlur={(event) => { if (!event.currentTarget.contains(event.relatedTarget as Node | null)) { setSearching(false); setActive(-1); } }}>
          <label className="os-sr-only" htmlFor={searchId}>Search users and groups</label>
          <input ref={search} id={searchId} className="os-input fleet-share-search" role="combobox" aria-autocomplete="list" aria-expanded={results.length > 0} aria-controls={results.length ? listId : undefined} aria-activedescendant={activeIndex >= 0 ? `${listId}-${activeIndex}` : undefined} autoComplete="off" spellCheck={false} placeholder="Search users and groups" value={query} disabled={busy || !directory} onFocus={() => setSearching(true)} onChange={(event) => { setQuery(event.target.value); setSearching(true); setActive(0); }} onKeyDown={onSearchKeyDown} />
          {results.length > 0 ? <div id={listId} role="listbox" aria-label="Users and groups to add" className="fleet-share-results">
            {results.map((candidate, index) => <div key={subjectKey(candidate.kind, candidate.id)} id={`${listId}-${index}`} role="option" aria-selected={index === activeIndex} aria-label={candidate.name} className="fleet-share-option" data-active={index === activeIndex || undefined} onMouseDown={(event) => event.preventDefault()} onClick={() => { if (!busy) void pick(candidate); }}>
              {candidate.kind === "group" ? <Users size={14} aria-hidden className="fleet-share-option-icon" /> : <User size={14} aria-hidden className="fleet-share-option-icon" />}
              <span className="fleet-share-option-name">{candidate.name}</span>
              {candidate.detail ? <span className="fleet-share-option-detail">{candidate.detail}</span> : null}
              <Plus size={14} aria-hidden className="fleet-share-option-add" />
            </div>)}
          </div> : searching && query.trim() && directory ? <Caption>No matches</Caption> : null}
        </div>
        {read.state === "loading" ? <RecordListSkeleton label="Loading users and groups" /> : <LiveList source={source} label="Shared users and groups" rowId={(s) => subjectKey(s.kind, s.id)} fingerprint={(s) => `${describe(s).name}:${s.paused === true}`} renderRow={(subject) => {
          const key = subjectKey(subject.kind, subject.id), { name, stale } = describe(subject);
          const count = sharingActiveCalls(machine, subject);
          return <RecordRow name={name} secondary={stale ? "Unavailable" : subject.kind === "group" ? "Group" : "User"} state={subject.paused ? "Paused" : undefined} dim={subject.paused} trailing={<span className="fleet-share-calls" title={count === null ? "Active calls unavailable" : undefined}>{count === null ? "—" : `${count} active ${count === 1 ? "call" : "calls"}`}</span>} actionLayout="compact" actions={<>
            <IconButton data-share-action={key} label={`${subject.paused ? "Resume" : "Pause"} sharing with ${name}`} disabled={busy} onClick={() => void persist(choiceFor("people", subjects.map((s) => subjectKey(s.kind, s.id) === key ? { ...s, paused: !s.paused } : s)), key, key)}>{pending === key ? <LoaderCircle size={16} aria-hidden className="os-spin" /> : subject.paused ? <Play size={16} aria-hidden /> : <Pause size={16} aria-hidden />}</IconButton>
            <IconButton label={`Remove sharing with ${name}`} disabled={busy} onClick={() => { const remaining = subjects.filter((s) => subjectKey(s.kind, s.id) !== key); void persist(choiceFor("people", remaining), `remove:${key}`, remaining[0] ? subjectKey(remaining[0].kind, remaining[0].id) : "search"); }}><Trash2 size={16} aria-hidden /></IconButton>
          </>} />;
        }} />}
        {read.state === "failed" || (read.state === "ready" && read.error) ? <Notice tone="warn" sentence="Users and groups could not be refreshed." detail={read.error}><Button disabled={refreshing} onClick={retry}>Try again</Button></Notice> : null}
      </section> : null}
    </div>
    {failed ? <div className="fleet-share-error"><Notice tone="error" sentence="Sharing could not be updated." detail={writes.actionError}>
        {canRetry ? <Button disabled={busy} onClick={() => void persist(failed.choice, "retry")}>Try again</Button> : <Caption>Sharing changed elsewhere. Choose the action again.</Caption>}
      </Notice></div> : null}
  </section>;
}
