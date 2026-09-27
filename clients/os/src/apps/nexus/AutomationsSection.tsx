import { useEffect, useMemo, useState, useSyncExternalStore } from "react";
import { FileCode2, Footprints, Repeat } from "lucide-react";
import type { LiveSnapshot, Row } from "@znasllc-io/memql-sdk-core/client";

import {
  Caption,
  EmptyState,
  Head,
  LiveList,
  Notice,
  Panel,
  Refine,
  Select,
  formatFreshness,
  formatMoment,
  useNow,
  useTwoFeedView,
  type LiveListSource,
} from "../../kit";
import { ActionBar, type Act, type ActionBarTone } from "../../kit/ActionBar";
import { RecordList, RecordRow, type RecordTone } from "../../kit/RecordRow";
import type { ArrivalKind } from "../../live/arrival";
import {
  automationBand,
  automationFingerprint,
  automationFromRow,
  automationMatches,
  constructOriginWord,
  isAutomation,
  rung,
  rungMeaning,
  rungRank,
  rungWord,
  statusMeaning,
  statusWord,
  type AutomationRow,
  type Origin,
} from "./automations";
import {
  evidenceLine,
  ladderRank,
  ladderWord,
  procedureBand,
  procedureFingerprint,
  procedureFromRow,
  procedureTitle,
  type LadderPolicy,
  type ProcedureRow,
} from "./ladder";
import { ProcedurePage } from "./ProcedurePage";
import {
  CATALOG_PAGE_BOUND,
  PROCEDURE_PAGE_BOUND,
  isLearnedProcedure,
  useSetAutomationStatus,
  type AutomationFeeds,
} from "./useAutomations";
import { idTail, type ApprovalRow, type RunRow } from "./rows";

// AUTOMATIONS: what this instance can replay without a model.
//
// ===========================================================================
// THIS IS WHERE THE PRODUCT'S CLAIM BECOMES CHECKABLE
// ===========================================================================
// Every other surface in this app is about one piece of work. This one is
// about what the instance has LEARNED: the templates a goal compiled to, the
// procedures it watched an app perform, how well each has done, and whether it
// serves. A person who wants to know whether the system is actually getting
// cheaper reads this list.
//
// ===========================================================================
// ONE LIST, AND WHERE A ROW CAME FROM IS A FACT ON IT
// ===========================================================================
// A learned procedure is an automation with a different origin, so it is a
// row here like any other, saying "Learned" where an authored one says
// "Authored" -- and the question "only the learned ones" is a facet in Refine
// (DESIGN.md, "a subset is a filter"), never a heading over half the list.
// The two open differently, and that is rule 11 applied twice rather than an
// inconsistency: an authored automation's detail is short and stands BESIDE
// the list; a procedure's is a ladder, its evidence and every step it runs, so
// its page REPLACES the list.
//
// ===========================================================================
// IT IS LIVE, AND IT ANNOUNCES ONLY WHAT A PERSON WOULD CALL NEWS
// ===========================================================================
// Both halves follow `v1:authoring:construct` (see useAutomations.ts for the
// routing rule), so a procedure that climbs while somebody is looking moves on
// screen by itself -- there is no "Look again", because there is nothing to
// look again for. The arrival cue fires on a rung change, a status flip or a
// promotion arriving; it stays silent on the evidence ticks a shadow
// comparison writes every time it runs, which the row's own line and
// freshness already show continuously.
//
// ===========================================================================
// THE LADDER IS A WORD, NEVER A PERCENTAGE
// ===========================================================================
// `reliability` is 0..1 and it is NOT a probability of success: it climbs when
// a run whose fingerprints matched succeeds and decays on mismatch and on
// disuse. "62%" invites a reader to treat it as odds. The rung a person
// actually cares about is whether this can be trusted to run unwatched, and
// that is a word -- with "not yet proven" kept distinct from "struggling",
// because a template nobody has run has earned nothing and one that has been
// run and kept missing has earned less than nothing. A learned procedure's
// rung is a word too, and its evidence is a count.

export interface AutomationsSectionProps {
  feeds: AutomationFeeds;
  selectedId: string;
  onSelect: (constructId: string) => void;
  /** The learned procedure whose page is open, or "". */
  openProcedureId: string;
  onOpenProcedure: (constructId: string) => void;
  onCloseProcedure: () => void;
  /** The pending approvals feed, so a page can tell an open promotion from a decided one. */
  approvals: readonly ApprovalRow[];
  approvalsKnown: boolean;
  runs: readonly RunRow[];
  onOpenApproval: (approvalId: string) => void;
  onOpenRun: (runId: string) => void;
}

/** One row of the merged list: an authored automation or a learned procedure. */
type Entry =
  | { origin: "authored"; key: string; band: number; standing: number; name: string; automation: AutomationRow }
  | { origin: "learned"; key: string; band: number; standing: number; name: string; procedure: ProcedureRow };

/** What rings: a rung change, a status flip, a promotion -- never an evidence tick. */
function entryFingerprint(entry: Entry): string {
  return entry.origin === "authored"
    ? `a|${automationFingerprint(entry.automation)}`
    : `p|${procedureFingerprint(entry.procedure)}`;
}

/**
 * The merged list, from the two feeds' raw rows.
 *
 * PROJECTED HERE, ON THE READ SIDE: a collection folds an event's payload as
 * the row, so it holds raw wire rows, and a projection is what makes a seeded
 * row and a folded one read alike.
 */
function entriesOf(
  catalogRows: readonly Row[],
  learnedRows: readonly Row[],
  search: string,
  origin: "" | Origin,
): Entry[] {
  const needle = search.trim().toLowerCase();
  const out: Entry[] = [];
  if (origin !== "learned") {
    for (const row of catalogRows) {
      // SAID ONCE. A learned procedure is listed by its ladder; were one ever
      // catalogued as well, it would not appear a second time as authored.
      if (!isAutomation(row) || isLearnedProcedure(row)) continue;
      const automation = automationFromRow(row);
      if (automation.id === "" || !automationMatches(automation, search)) continue;
      out.push({
        origin: "authored",
        key: `a:${automation.id}`,
        band: automationBand(automation),
        standing: rungRank(rung(automation)),
        name: automation.name,
        automation,
      });
    }
  }
  if (origin !== "authored") {
    for (const row of learnedRows) {
      const procedure = procedureFromRow(row);
      if (procedure.id === "") continue;
      const title = procedureTitle(procedure);
      const haystack = [title, procedure.name, ladderWord(procedure.ladder), "learned"].join(" ").toLowerCase();
      if (needle !== "" && !haystack.includes(needle)) continue;
      out.push({
        origin: "learned",
        key: `p:${procedure.id}`,
        band: procedureBand(procedure),
        standing: ladderRank(procedure.ladder),
        name: title,
        procedure,
      });
    }
  }
  // BANDS A PERSON CAN EXPLAIN, then how far each has climbed, then the name.
  // The one waiting on you first; then what serves; then what is still
  // earning it; then what serves nothing. "How far" is the RUNG, never the raw
  // reliability: that moves on every run, and a live list ordered by it would
  // reshuffle under somebody reading it. A total order, so two rows never swap.
  return out.sort((a, b) => {
    if (a.band !== b.band) return a.band - b.band;
    if (a.standing !== b.standing) return b.standing - a.standing;
    const byName = a.name.localeCompare(b.name);
    return byName !== 0 ? byName : a.key.localeCompare(b.key);
  });
}

const NO_ROWS: LiveSnapshot<never> = { rows: [], state: "disconnected", error: "", version: 0 };

/**
 * The list's source with its error said elsewhere.
 *
 * The notices above the list name WHICH feed was refused and what is still
 * shown; LiveList's own error line would say one of them a second time,
 * without saying which. Identity-stable against the upstream snapshot, for the
 * reason `useLiveView` records.
 */
function useErrorsSaidAbove<U>(source: LiveListSource<U> | null): LiveListSource<U> | null {
  return useMemo(() => {
    if (source === null) return null;
    let from: LiveSnapshot<U> | null = null;
    let quiet: LiveSnapshot<U> | null = null;
    return {
      subscribe: (listener: () => void) => source.subscribe(listener),
      get snapshot(): LiveSnapshot<U> {
        const upstream = source.snapshot;
        if (upstream !== from || quiet === null) {
          from = upstream;
          quiet = upstream.error === "" ? upstream : { ...upstream, error: "" };
        }
        return quiet;
      },
    };
  }, [source]);
}

export function AutomationsSection({
  feeds,
  selectedId,
  onSelect,
  openProcedureId,
  onOpenProcedure,
  onCloseProcedure,
  approvals,
  approvalsKnown,
  runs,
  onOpenApproval,
  onOpenRun,
}: AutomationsSectionProps) {
  const now = useNow(30_000);
  const [search, setSearch] = useState("");
  const [origin, setOrigin] = useState<"" | Origin>("");
  // A LINK THAT NAMES A PROCEDURE THIS CATALOG DOES NOT HOLD is answered once
  // and let go: holding the id open would pop the page open by itself the day
  // the feed happened to deliver it, which is navigation nobody asked for.
  const [missing, setMissing] = useState("");
  const status = useSetAutomationStatus();
  const catalogFeed = feeds.catalog.snapshot;
  const learnedFeed = feeds.procedures.snapshot;

  const openProcedure =
    openProcedureId === ""
      ? null
      : (feeds.procedureRows.find((p) => idTail(p.id) === idTail(openProcedureId)) ?? null);

  // NOT THERE is said only by a feed that has answered and is current: while
  // it seeds the procedure may be on its way, and a refused or disconnected
  // feed cannot say what the cluster holds.
  useEffect(() => {
    if (openProcedureId === "" || openProcedure !== null) return;
    if (learnedFeed.state !== "live" || learnedFeed.error !== "") return;
    setMissing(openProcedureId);
    onCloseProcedure();
  }, [openProcedureId, openProcedure, learnedFeed]);

  // KEYED ON THE QUESTION: a new search or facet re-baselines the arrival cue,
  // so revealing rows the browser already had is not announced as the cluster
  // sending them (README, "a resync is not an arrival").
  const viewKey = `${search.trim().toLowerCase()}|${origin}`;
  const view = useTwoFeedView<Row, Row, Entry>(
    feeds.catalog.source,
    feeds.procedures.source,
    viewKey,
    (catalogRows, learnedRows) => entriesOf(catalogRows, learnedRows, search, origin),
  );
  const listSource = useErrorsSaidAbove(view);
  const snapshot = useSyncExternalStore(
    useMemo(() => (view ? view.subscribe.bind(view) : () => () => {}), [view]),
    () => (view ? view.snapshot : (NO_ROWS as LiveSnapshot<Entry>)),
  );
  const entries = snapshot.rows;

  // Everything held, before the question narrows it: whether there is
  // anything to ask a question OF (rule 2), and what the bar should offer.
  const held = useMemo(() => {
    const authored = catalogFeed.rows.filter((row) => isAutomation(row) && !isLearnedProcedure(row)).length;
    return { authored, total: authored + learnedFeed.rows.length };
  }, [catalogFeed, learnedFeed]);

  const selected =
    entries.find(
      (e): e is Extract<Entry, { origin: "authored" }> =>
        e.origin === "authored" && idTail(e.automation.id) === idTail(selectedId),
    )?.automation ?? null;

  if (openProcedure !== null) {
    return (
      <ProcedurePage
        procedure={openProcedure}
        policy={feeds.ladderPolicy}
        policyKnown={feeds.policyKnown}
        policyError={feeds.policy.snapshot.error}
        feedState={learnedFeed.state}
        approvals={approvals}
        approvalsKnown={approvalsKnown}
        runs={runs}
        onBack={onCloseProcedure}
        onOpenApproval={onOpenApproval}
        onOpenRun={onOpenRun}
      />
    );
  }

  const acts: Act[] = [];
  if (selected !== null) {
    // AN ILLEGAL ACT IS ABSENT (rule 12). Arm is offered on anything that is
    // not already armed; Retire only on something that is. Neither is rendered
    // greyed out, because a control that cannot be used is a question about
    // why. The row moves by itself once the write lands: the feed carries it.
    if (selected.status !== "active") {
      acts.push({
        label: "Arm it",
        tone: "primary",
        busy: status.busy === selected.id,
        ariaLabel: `Arm ${selected.name}: register it in the shared runtime so a matching goal replays it without a model`,
        onAct: () => void status.set(selected.id, "active"),
      });
    }
    if (selected.status === "active") {
      acts.push({
        label: "Retire it",
        tone: "danger",
        busy: status.busy === selected.id,
        ariaLabel: `Retire ${selected.name}: it stays readable so runs that used it can be explained, and is never selected again`,
        onAct: () => void status.set(selected.id, "retired"),
      });
    }
  }

  // A COUNT ONLY FROM A SETTLED, WHOLE ANSWER (DESIGN.md rule 1). While a feed
  // seeds, or after one was refused, the list is not the collection, and a
  // number over it would be read as the total.
  const settled =
    catalogFeed.state === "live" &&
    learnedFeed.state === "live" &&
    catalogFeed.error === "" &&
    learnedFeed.error === "";
  // Nothing to say about an empty list that has not been answered in full --
  // still arriving, disconnected, or partly refused (the notices say which).
  const unanswered = entries.length === 0 && !settled;
  const refused = catalogFeed.error !== "" || learnedFeed.error !== "";
  const filtered = search.trim() !== "" || origin !== "";
  const tone: ActionBarTone =
    selected === null ? "none" : selected.status === "active" ? "live" : "paused";
  const chips =
    origin === ""
      ? []
      : [{ id: "origin", label: origin === "learned" ? "learned" : "authored", onRemove: () => setOrigin("") }];

  return (
    <div className="os-nexus-automations">
      <Head title="Automations" meta={settled ? entries.length : undefined}>
        {/* NO FILTER CHROME OVER NO CONTENT (rule 2): with nothing in the
            catalog there is no question to ask of it. */}
        {held.total === 0 && !filtered ? null : (
          <Refine search={search} onSearch={setSearch} placeholder="Search" label="Refine automations" chips={chips}>
            <Select
              id="automations-facet-origin"
              label="Origin"
              value={origin}
              onChange={(next) => setOrigin(next === "authored" || next === "learned" ? next : "")}
            >
              <option value="">Authored and learned</option>
              <option value="authored">Authored only</option>
              <option value="learned">Learned only</option>
            </Select>
          </Refine>
        )}
      </Head>

      <div className="os-nexus-split">
        <div className="os-nexus-column">
          {catalogFeed.error !== "" ? (
            <Notice tone="error" sentence="The catalog could not be read." detail={catalogFeed.error} />
          ) : null}
          {learnedFeed.error !== "" ? (
            <Notice
              tone="warn"
              sentence="The learned procedures could not be read."
              next={catalogFeed.error === "" ? "Every authored automation is still listed." : undefined}
              detail={learnedFeed.error}
            />
          ) : null}
          {missing === "" ? null : (
            <Notice
              tone="info"
              sentence="That procedure is not in your catalog."
              next={
                learnedFeed.rows.length >= PROCEDURE_PAGE_BOUND
                  ? `Only the first ${PROCEDURE_PAGE_BOUND} learned procedures are listed, so it may be beyond them -- or it belongs to another account.`
                  : "It may belong to another account, or its id has changed."
              }
              detail={missing}
            />
          )}

          <RecordList>
            <LiveList<Entry>
              key={viewKey}
              source={listSource}
              rowId={(entry) => entry.key}
              fingerprint={entryFingerprint}
              label="Automations this instance can replay"
              emptyText="No automation here matches that."
              emptyContent={
                refused ? (
                  // A REFUSED READ IS NOT AN EMPTY CATALOG: the notice above
                  // already said so, and "nothing here yet" would contradict it.
                  <></>
                ) : filtered ? (
                  <Caption>No automation here matches that.</Caption>
                ) : (
                  // AN EMPTY SCREEN IS AN INVITATION, and here the invitation is
                  // not a button -- nobody authors an automation by hand in this
                  // app. It says where they come from instead, and only after a
                  // read has answered that there are none.
                  <EmptyState title="Nothing here yet" icon={Repeat}>
                    <p>
                      An automation appears when a goal is worked out: the system compiles what it
                      decided into a template, and a template that keeps succeeding earns its way
                      up this list.
                    </p>
                    <p>
                      A procedure appears when the system has watched an app do the same work more
                      than once. It replays beside the app first, and runs without a model only
                      once it has earned it.
                    </p>
                  </EmptyState>
                )
              }
              renderRow={(entry, tick) =>
                entry.origin === "authored" ? (
                  <AuthoredRow
                    automation={entry.automation}
                    tick={tick}
                    open={idTail(entry.automation.id) === idTail(selectedId)}
                    onOpen={() => {
                      setMissing("");
                      onSelect(entry.automation.id);
                    }}
                  />
                ) : (
                  <LearnedRow
                    procedure={entry.procedure}
                    policy={feeds.ladderPolicy}
                    tick={tick}
                    now={now}
                    onOpen={() => {
                      setMissing("");
                      onOpenProcedure(entry.procedure.id);
                    }}
                  />
                )
              }
            />
          </RecordList>

          {catalogFeed.rows.length >= CATALOG_PAGE_BOUND || learnedFeed.rows.length >= PROCEDURE_PAGE_BOUND ? (
            // A PAGE IS NOT THE WHOLE. Said only when the list may be partial,
            // because then it is the one thing that changes how it is read.
            <Caption>
              {catalogFeed.rows.length >= CATALOG_PAGE_BOUND
                ? `Showing the first ${catalogFeed.rows.length} catalogued constructs; there may be more. `
                : ""}
              {learnedFeed.rows.length >= PROCEDURE_PAGE_BOUND
                ? `Showing the first ${learnedFeed.rows.length} learned procedures; there may be more.`
                : ""}
            </Caption>
          ) : null}
        </div>

        {selected === null ? null : (
          <div className="os-nexus-column os-nexus-detail">
            <Panel label={selected.name}>
              <p className="os-nexus-rung-line">
                <strong>{rungWord(rung(selected))}</strong> — {rungMeaning(selected)}
              </p>
              <dl className="os-facts">
                <dt>Registers as</dt>
                <dd className="os-mono">
                  {selected.targetNamespace === "" ? "—" : selected.targetNamespace}
                </dd>
                <dt>Status</dt>
                <dd title={statusMeaning(selected.status)}>{statusWord(selected.status)}</dd>
                <dt>Successful runs</dt>
                <dd className="os-mono">{selected.reinforceCount}</dd>
                <dt>Last succeeded</dt>
                <dd>
                  {selected.lastReinforced === ""
                    ? "never"
                    : formatFreshness(selected.lastReinforced, now)}
                </dd>
                <dt>In the catalog since</dt>
                <dd>
                  {selected.catalogedAt === ""
                    ? "—"
                    : formatMoment(selected.catalogedAt)}
                </dd>
                <dt>Compiled from a goal</dt>
                <dd>
                  {/* The signature is a hash, so it is shown as PRESENT or
                      ABSENT rather than printed: sixty-four hex characters
                      tell a reader nothing, and the fact they want is whether
                      this template answers a goal shape at all. */}
                  {selected.goalSignature === ""
                    ? "no — authored directly"
                    : "yes — it answers a goal shape"}
                </dd>
              </dl>
            </Panel>

            {selected.source === "" ? null : (
              <Panel label="What it does">
                <pre className="os-nexus-source os-mono">{selected.source}</pre>
              </Panel>
            )}
          </div>
        )}
      </div>

      <ActionBar
        // NOTHING IS SAID OVER AN UNANSWERED LIST: "nothing yet" over a feed
        // that has not answered would be a claim about the catalog.
        state={
          selected !== null
            ? statusWord(selected.status)
            : unanswered
              ? ""
              : entries.length === 0
                ? "Nothing yet"
                : "Nothing selected"
        }
        detail={
          selected !== null
            ? statusMeaning(selected.status)
            : unanswered
              ? undefined
              : entries.length === 0
                ? // "Select an automation" on an empty list is an instruction
                  // for a list that does not exist. Say what IS true instead.
                  "nothing to arm yet"
                : held.authored > 0
                  ? "select an automation to arm or retire it"
                  : "select one to see where it stands"
        }
        tone={tone}
        acts={acts}
      >
        {status.error === "" ? null : (
          <span className="os-nexus-act-error os-mono" role="alert">
            {status.error}
          </span>
        )}
      </ActionBar>
    </div>
  );
}

/** The arrival cue's words, for a row that has just appeared. */
function NewTick({ tick }: { tick: ArrivalKind | null }) {
  return tick === "added" ? <span className="os-livelist-tick">new</span> : null;
}

function AuthoredRow({
  automation,
  tick,
  open,
  onOpen,
}: {
  automation: AutomationRow;
  tick: ArrivalKind | null;
  open: boolean;
  onOpen: () => void;
}) {
  return (
    <RecordRow
      icon={<FileCode2 size={16} aria-hidden />}
      name={automation.name}
      secondary={
        <>
          {constructOriginWord("authored")}
          {automation.targetNamespace === "" ? null : (
            <>
              , registers as <span className="os-mono">{automation.targetNamespace}</span>
            </>
          )}
        </>
      }
      state={statusWord(automation.status)}
      tone={automation.status === "active" ? "accent" : "muted"}
      stateTitle={statusMeaning(automation.status)}
      stateExtra={
        <>
          <NewTick tick={tick} />
          <RungMark automation={automation} />
        </>
      }
      dim={automation.status === "retired"}
      open={open}
      onOpen={onOpen}
    >
      <span>{rungWord(rung(automation))}</span>
    </RecordRow>
  );
}

function LearnedRow({
  procedure,
  policy,
  tick,
  now,
  onOpen,
}: {
  procedure: ProcedureRow;
  policy: LadderPolicy | null;
  tick: ArrivalKind | null;
  now: Date;
  onOpen: () => void;
}) {
  const line = evidenceLine(procedure, policy);
  const runs = procedure.recordedFrom.runIds.length;
  // THE STATE WORD IS THE RUNG, IN THE RUNG'S TONE: the accent for the two
  // that serve. A promotion waiting on the person is said once, by the
  // evidence line beside it -- the one real event in the list, and the only
  // place the accent means "yours to do".
  const tone: RecordTone = procedure.ladder === "canary" || procedure.ladder === "trusted" ? "accent" : "muted";
  return (
    <RecordRow
      icon={<Footprints size={16} aria-hidden />}
      name={procedureTitle(procedure)}
      secondary={
        runs === 0
          ? constructOriginWord("learned")
          : `${constructOriginWord("learned")} from ${runs} ${runs === 1 ? "run" : "runs"}`
      }
      state={ladderWord(procedure.ladder)}
      tone={tone}
      stateExtra={
        <>
          <NewTick tick={tick} />
          <span className="os-nexus-procedure-fresh" title={procedure.lastReplayAt === "" ? undefined : formatMoment(procedure.lastReplayAt)}>
            {procedure.lastReplayAt === "" ? "never replayed" : `replayed ${formatFreshness(procedure.lastReplayAt, now)}`}
          </span>
        </>
      }
      dim={procedure.ladder === "retired" || procedure.ladder === ""}
      onOpen={onOpen}
    >
      <span className="os-nexus-evidence-line" data-tone={line.tone}>
        {line.text}
      </span>
    </RecordRow>
  );
}

/**
 * The ladder, as a mark.
 *
 * FOUR TICKS, filled to where this template stands. It is a shape rather than
 * a colour, so it survives greyscale and every theme pack, and the accessible
 * name carries the word -- a reader who cannot see the ticks gets "Proven",
 * which is the whole reading.
 */
function RungMark({ automation }: { automation: AutomationRow }) {
  const level = rung(automation);
  const filled =
    level === "proven" ? 4 : level === "good" ? 3 : level === "fair" ? 2 : level === "poor" ? 1 : 0;
  return (
    <span
      className="os-nexus-rung"
      data-rung={level}
      role="img"
      aria-label={`${rungWord(level)}. ${rungMeaning(automation)}`}
      title={rungMeaning(automation)}
    >
      {[0, 1, 2, 3].map((i) => (
        <span key={i} className="os-nexus-rung-tick" data-on={i < filled || undefined} />
      ))}
    </span>
  );
}
