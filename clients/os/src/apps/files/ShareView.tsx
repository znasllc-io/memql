import { useEffect, useId, useRef, useState, type KeyboardEvent } from "react";
import { Check, Ellipsis, FileText, User, Users } from "lucide-react";
import { sameEntityId } from "@znasllc-io/memql-sdk-core/client";

import { ContextMenu } from "../../chrome/ContextMenu";
import { Caption, Head, NextPage, Notice, Select, Button } from "../../kit";
import { AnchoredPicker } from "../../kit/AnchoredPicker";
import { CalendarMonth, addDays, dayOf, type CalendarDay } from "../../kit/CalendarMonth";
import { CardCarousel } from "../../kit/CardCarousel";
import { CardCollectionSearch, CardCollectionSkeleton } from "../../kit/CardCollectionEditor";
import { listScrollTop } from "../../kit/controls";
import { IconButton } from "../../kit/IconButton";
import { useNow } from "../../kit/useNow";
import { useOsConnection } from "../../live/connection";
import { groupSize, matchesQuery, subjectKey, type SubjectKind } from "../fleet/machines/sharing";
import { artifactName, type ArtifactRow } from "./rows";
import { shareFileCall, timeLeftLabel, useFileShares, useShareDirectory, type FileShare, type ShareEnd } from "./useSharing";

// THE SHARE VIEW (Jesús's picks for item 6 of José's Nov 19 call, Nov 20 2026).
//
// Pressing Share opens this IN PLACE OF THE TILES -- "another screen here in
// the middle", José, 17:21 -- not a modal: the rail stays, the rest of the OS
// stays usable, and the trail's back arrow returns to the files. It replaces
// the small "Share with" box that sat at the foot of the rail ("There's too
// much going on. It's too small.").
//
// Two settings, on Fleet's setting rows (a name on the left, the control on
// the right):
//
//   Expires          ONE drop-down (draft B): 1 day, 7 days, 30 days and
//                    "Pick a date…", which opens the OS's own calendar with
//                    past days disabled; the end in words beside it ("until
//                    Thu, Nov 26"). 7 days to start. The lengths and the
//                    default are still a guess (the old box offered 1, 7, 10
//                    and 30).
//   Users and groups FLEET'S PICKER, as it is (apps/fleet/machines/
//                    SharingEditor.tsx): All / Users / Groups, Search, "+" to
//                    add what is ticked, the list loading more by itself as it
//                    scrolls, and the people and groups who have the file as
//                    cards. Built from the same kit parts,
//                    classes and helpers rather than the component itself,
//                    because that one saves a machine's whole audience at once
//                    and this saves one share per person or group.
//
// Adding someone shares the file with them until the end chosen above. Who
// already has it is not offered again: each card's "⋯" menu (Jesús's pick,
// draft p18 Option C, Nov 20: "C. I like C.") holds "Renew for <the Expires
// length>" and, under a hairline in the danger colour, "Remove sharing" (what
// the card's bin did). Renew is a re-share with that person or group: the
// engine renews an existing share in place rather than adding a second one,
// and for a group reaches the people who joined since and drops those who
// left (integrations/library/share.go).

const LENGTHS = [1, 7, 30] as const;
const PICK = "date";
/** The furthest a share may end (integrations/library/share.go, shareMaxDays). */
const MAX_DAYS = 90;

interface Candidate { kind: SubjectKind; id: string; name: string; detail: string }

/** The end of a picked day in the sharer's own time zone: the share runs through it. */
function endOfDay(day: CalendarDay): Date {
  return new Date(day.year, day.month, day.day, 23, 59, 59, 999);
}

function inWords(date: Date): string {
  return date.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric" });
}

function shareKey(share: FileShare): string {
  return share.recipientGroupId ? subjectKey("group", share.recipientGroupId) : subjectKey("person", share.recipientUserId);
}

export function ShareView({ file, backLabel, onBack }: { file: ArtifactRow; backLabel: string; onBack: () => void }) {
  const connection = useOsConnection();
  const name = artifactName(file);
  const now = useNow(60_000);

  // ---- Expires ------------------------------------------------------------
  const [length, setLength] = useState<string>("7");
  const [picked, setPicked] = useState<CalendarDay | null>(null);
  const [calendar, setCalendar] = useState(false);
  const expiry = useRef<HTMLDivElement>(null);
  const today = dayOf(now);
  // The last day whose END is still within the ninety days the engine allows.
  const lastDay = addDays(today, MAX_DAYS - 1);
  const end: ShareEnd | null = length === PICK ? (picked ? { until: endOfDay(picked) } : null) : { days: Number(length) };
  const endsAt = end === null ? null : "days" in end ? new Date(now.getTime() + end.days * 86_400_000) : end.until;
  const closeCalendar = () => {
    setCalendar(false);
    // Dismissed with no day ever picked: back to the length it replaced.
    if (picked === null) setLength("7");
  };
  useEffect(() => {
    if (!calendar) return;
    const outside = (event: MouseEvent) => {
      if (!expiry.current?.contains(event.target as Node)) closeCalendar();
    };
    document.addEventListener("mousedown", outside);
    return () => document.removeEventListener("mousedown", outside);
  });

  // ---- Users and groups (Fleet's picker) ------------------------------------
  const directory = useShareDirectory(true);
  const shares = useFileShares(file.id);
  const searchId = useId(), listId = useId();
  const search = useRef<HTMLInputElement>(null);
  const searchArea = useRef<HTMLDivElement>(null), popup = useRef<HTMLDivElement>(null);
  const [query, setQuery] = useState("");
  const [searchBy, setSearchBy] = useState("all");
  const [searching, setSearching] = useState(false);
  const [active, setActive] = useState(-1);
  const [visibleCount, setVisibleCount] = useState(5);
  const [chosenKeys, setChosenKeys] = useState<string[]>([]);
  const [revealKey, setRevealKey] = useState<string>();
  const [pending, setPending] = useState("");
  const [failure, setFailure] = useState("");
  const [menu, setMenu] = useState<{ share: FileShare; anchor: HTMLElement; x: number; y: number } | null>(null);
  const ready = directory.people !== null;
  const candidates: Candidate[] = ready ? [
    ...directory.groups.map((g): Candidate => ({ kind: "group", id: g.groupId, name: g.displayName, detail: groupSize(g.members) })),
    ...(directory.people ?? []).map((p): Candidate => ({ kind: "person", id: p.userId, name: p.displayName, detail: "" })),
  ].sort((a, b) => a.name.localeCompare(b.name) || subjectKey(a.kind, a.id).localeCompare(subjectKey(b.kind, b.id))) : [];
  const taken = new Set(shares.map(shareKey));
  const available = candidates.filter((c) => !taken.has(subjectKey(c.kind, c.id)) && (searchBy === "all" || c.kind === searchBy) && matchesQuery(query, c.name, c.detail));
  const results = available.slice(0, visibleCount);
  const hasMore = available.length > results.length;
  const eligible = new Set(candidates.filter((c) => !taken.has(subjectKey(c.kind, c.id))).map((c) => subjectKey(c.kind, c.id)));
  const selection = chosenKeys.filter((key) => eligible.has(key));
  const activeIndex = active >= 0 && active < results.length ? active : -1;
  const locked = pending !== "";

  function toggleCandidate(candidate: Candidate) {
    if (locked) return;
    const key = subjectKey(candidate.kind, candidate.id);
    setChosenKeys((keys) => (keys.includes(key) ? keys.filter((item) => item !== key) : [...keys, key]));
  }

  async function addSelection() {
    const query_ = connection?.query ?? null;
    if (!selection.length || locked) return;
    if (end === null) {
      setFailure("Pick the day the share ends first.");
      return;
    }
    if (query_ === null) {
      setFailure("Not connected to the cluster, so nothing was shared.");
      return;
    }
    const picks = candidates.filter((c) => selection.includes(subjectKey(c.kind, c.id)));
    setPending("add");
    setFailure("");
    const done: string[] = [];
    try {
      for (const pick of picks) {
        await query_.executeNamed("libraryShareFile", shareFileCall(file.id, pick.kind === "group" ? { groupId: pick.id } : { userId: pick.id }, end));
        done.push(subjectKey(pick.kind, pick.id));
      }
      setQuery(""); setActive(-1); setSearching(false); setVisibleCount(5);
    } catch (err: unknown) {
      setFailure(err instanceof Error ? err.message : String(err));
    } finally {
      // Whoever was shared with leaves the selection, even when a later one
      // failed, so trying again does not share with them twice.
      setChosenKeys((keys) => keys.filter((key) => !done.includes(key)));
      if (done[0]) setRevealKey(done[0]);
      setPending("");
    }
  }

  async function renew(share: FileShare) {
    const query_ = connection?.query ?? null;
    if (end === null || locked) return;
    if (query_ === null) {
      setFailure("Not connected to the cluster, so nothing was renewed.");
      return;
    }
    setPending(`renew:${share.shareId}`);
    setFailure("");
    try {
      await query_.executeNamed("libraryShareFile", shareFileCall(file.id, share.recipientGroupId ? { groupId: share.recipientGroupId } : { userId: share.recipientUserId }, end));
    } catch (err: unknown) {
      setFailure(err instanceof Error ? err.message : String(err));
    } finally {
      setPending("");
    }
  }

  // The card's "⋯" menu, on the shell's one menu (chrome/ContextMenu), opened
  // under the button as Ask's message menu is. Focus goes back to the button
  // it came from, and a scroll or resize shuts it rather than leave it behind.
  const closeMenu = () => {
    menu?.anchor.focus({ preventScroll: true });
    setMenu(null);
  };
  useEffect(() => {
    if (!menu) return;
    const dismiss = () => setMenu(null);
    window.addEventListener("resize", dismiss);
    window.addEventListener("scroll", dismiss, true);
    return () => {
      window.removeEventListener("resize", dismiss);
      window.removeEventListener("scroll", dismiss, true);
    };
  }, [menu]);
  function openMenu(share: FileShare, anchor: HTMLElement) {
    const rect = anchor.getBoundingClientRect();
    setMenu({ share, anchor, x: rect.right, y: rect.bottom });
  }
  /** "Renew for 7 days", or "Renew until Fri, Nov 27" for a picked day. */
  const renewLabel = end === null ? "Renew: pick the day it ends first" : "days" in end
    ? `Renew for ${end.days === 1 ? "1 day" : `${end.days} days`}` : `Renew until ${inWords(end.until)}`;

  async function remove(share: FileShare) {
    const query_ = connection?.query ?? null;
    if (query_ === null || locked) return;
    setPending(`remove:${share.shareId}`);
    setFailure("");
    setRevealKey(undefined);
    try {
      await query_.libraryShareStop({ shareId: share.shareId });
    } catch (err: unknown) {
      setFailure(err instanceof Error ? err.message : String(err));
    } finally {
      setPending("");
    }
  }

  function showMore(fromKeyboard = false) {
    if (!hasMore || locked) return;
    setVisibleCount((count) => count + 5);
    if (fromKeyboard) setActive(results.length);
  }

  function closeSearch() { setSearching(false); setActive(-1); }

  function onSearchKeyDown(event: KeyboardEvent<HTMLInputElement>): void {
    if (event.nativeEvent.isComposing) return;
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault(); setSearching(true);
      if (searching && event.key === "ArrowDown" && activeIndex === results.length - 1 && hasMore) showMore(true);
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

  // The cards follow the search too, as Fleet's do.
  const matches = shares.filter((share) => (searchBy === "all" || searchBy === (share.recipientGroupId ? "group" : "person")) &&
    matchesQuery(query, share.recipientDisplayName));
  const nameOf = (share: FileShare) => {
    const offered = share.recipientGroupId
      ? directory.groups.find((g) => sameEntityId(g.groupId, share.recipientGroupId))?.displayName
      : directory.people?.find((p) => sameEntityId(p.userId, share.recipientUserId))?.displayName;
    return offered || share.recipientDisplayName || (share.recipientGroupId ? "Unnamed group" : "Unnamed person");
  };

  return (
    <section className="os-files-share" aria-label={`Share ${name}`}>
      <Head
        navigationKey={`share:${file.id}`}
        title="Share"
        meta={<span className="os-files-share-file"><FileText size={14} aria-hidden /> {name}</span>}
        breadcrumbs={[{ label: backLabel, onSelect: onBack }, { label: "Share" }]}
        back={{ label: backLabel, onSelect: onBack }}
      />
      <div className="os-files-share-body">
        <section className="fleet-machine-setting" aria-label="Expires">
          <span className="fleet-machine-setting-name">Expires</span>
          <div
            ref={expiry}
            className="os-files-share-expiry"
            onKeyDown={(event) => {
              if (event.key === "Escape" && calendar) { event.preventDefault(); event.stopPropagation(); closeCalendar(); }
            }}
          >
            <Select
              id={`${searchId}-expires`}
              label="How long the file is shared"
              value={length}
              onChange={(next) => {
                setLength(next);
                setCalendar(next === PICK);
              }}
              onCommit={() => { if (length === PICK) setCalendar(true); }}
            >
              {LENGTHS.map((days) => <option key={days} value={String(days)}>{days === 1 ? "1 day" : `${days} days`}</option>)}
              <option value={PICK}>{picked ? inWords(endOfDay(picked)) : "Pick a date…"}</option>
            </Select>
            <span className="os-files-share-until" aria-live="polite">{endsAt ? `until ${inWords(endsAt)}` : "Pick the day it ends"}</span>
            {calendar ? (
              <div className="os-files-share-calendar" role="dialog" aria-label="Pick the day the share ends">
                <CalendarMonth
                  value={picked}
                  min={today}
                  max={lastDay}
                  today={today}
                  label="The day the share ends"
                  onPick={(day) => { setPicked(day); setLength(PICK); setCalendar(false); }}
                />
              </div>
            ) : null}
          </div>
        </section>

        <section className="fleet-machine-setting fleet-share-picker" aria-label="Users and groups">
          <span className="fleet-machine-setting-name">Users and groups</span>
          <div className="fleet-share-collection">
            <div ref={searchArea} className="fleet-share-search-area">
              <CardCollectionSearch label="Find or add users and groups" query={query} inputRef={search} disabled={locked || !ready}
                mode={{ label: "Search users and groups by", value: searchBy, options: [["all", "All"], ["person", "Users"], ["group", "Groups"]], onChange: (value) => { setSearchBy(value); setActive(-1); setVisibleCount(5); setRevealKey(undefined); } }}
                onQueryChange={(value) => { setQuery(value); setSearching(true); setActive(0); setVisibleCount(5); setRevealKey(undefined); }} clearLabel="Clear sharing search"
                add={{ label: "Add selected users and groups", enabled: selection.length > 0 && end !== null, onClick: () => { void addSelection(); } }}
                inputProps={{ id: `${searchId}-search`, "aria-label": "Search users and groups", role: "combobox", "aria-autocomplete": "list", "aria-expanded": searching && !locked,
                  "aria-controls": searching && !locked ? listId : undefined, "aria-activedescendant": searching && !locked && activeIndex >= 0 ? `${listId}-${activeIndex}` : undefined,
                  "aria-describedby": `${searchId}-selection`, autoComplete: "off", spellCheck: false, onFocus: () => setSearching(true), onClick: () => setSearching(true), onKeyDown: onSearchKeyDown }} />
              {searching && !locked ? <AnchoredPicker anchor={search} owner={searchArea} popupRef={popup} onClose={closeSearch}>
                <div id={listId} role="listbox" aria-multiselectable="true" aria-label="Users and groups to add">
                  {results.map((candidate, index) => {
                    const key = subjectKey(candidate.kind, candidate.id), chosen = selection.includes(key);
                    return <div key={key} id={`${listId}-${index}`} role="option" aria-selected={chosen} aria-label={candidate.name} className="fleet-share-option" data-active={index === activeIndex || undefined} onMouseDown={(event) => event.preventDefault()} onClick={() => { setActive(index); toggleCandidate(candidate); }}>
                      {candidate.kind === "group" ? <Users size={14} aria-hidden className="fleet-share-option-icon" /> : <User size={14} aria-hidden className="fleet-share-option-icon" />}
                      <span className="fleet-share-option-name">{candidate.name}</span>
                      {candidate.detail ? <span className="fleet-share-option-detail">{candidate.detail}</span> : null}
                      <span className="fleet-share-option-check" aria-hidden>{chosen ? <Check size={12} /> : null}</span>
                    </div>;
                  })}
                </div>
                {!results.length ? <p className="os-picker-empty">{query.trim() ? "No matches" : "No users or groups to add"}</p> : null}
                <NextPage key={`${query}:${searchBy}`} rows={1} label="Showing more users and groups" feed={{
                  hasMore, loadingMore: false, loadMoreError: "", progress: results.length,
                  enabled: ready && !locked, loadMore: () => showMore(),
                }} />
              </AnchoredPicker> : null}
            </div>
            <p className="os-caption fleet-share-selection-count" id={`${searchId}-selection`} role="status" aria-live="polite" aria-atomic="true">{selection.length} selected</p>
            {!ready ? <CardCollectionSkeleton label="Loading users and groups" /> : matches.length ? <CardCarousel key={`${searchBy}:${query}`} items={matches} itemKey={shareKey} revealKey={revealKey} minCardWidth={240}
              label="Sharing access cards" listLabel="Shared users and groups" noun="entry" plural="entries" renderItem={(share) => {
                const who = nameOf(share);
                return <div className="os-editable-card os-files-share-card">
                  <div className="os-editable-card-open">
                    <strong>{share.recipientGroupId ? <Users size={13} aria-hidden /> : <User size={13} aria-hidden />} {who}</strong>
                    <span>{share.recipientGroupId ? "Group" : "User"} · {timeLeftLabel(share.expiresAt, now)}</span>
                  </div>
                  <div className="os-editable-card-actions">
                    <IconButton label={`Actions for ${who}`} aria-haspopup="menu" aria-expanded={menu?.share.shareId === share.shareId} disabled={locked}
                      onClick={(event) => { if (menu) closeMenu(); else openMenu(share, event.currentTarget); }}><Ellipsis size={15} aria-hidden /></IconButton>
                  </div>
                </div>;
              }} /> : !searching && query.trim() && !available.length && ready ? <Caption>No matching users or groups.</Caption> : null}
            {directory.error ? <Notice tone="warn" sentence="Users and groups could not be read." detail={directory.error} /> : null}
          </div>
        </section>
        {menu ? <ContextMenu x={menu.x} y={menu.y} label={`Actions for ${nameOf(menu.share)}`} anchor={menu.anchor} onClose={closeMenu} entries={[
          { id: "renew", label: renewLabel, disabled: end === null, onSelect: () => renew(menu.share) },
          { id: "remove", label: "Remove sharing", separated: true, tone: "danger", onSelect: () => remove(menu.share) },
        ]} /> : null}
        {failure ? (
          <div className="fleet-share-error">
            <Notice tone="error" sentence="Sharing could not be updated." detail={failure}>
              <Button onClick={() => setFailure("")}>Dismiss</Button>
            </Notice>
          </div>
        ) : null}
      </div>
    </section>
  );
}
