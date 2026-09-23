import { useEffect, useMemo, useState } from "react";
import { FileCode2, Footprints, RefreshCw, Repeat } from "lucide-react";

import {
  Button,
  Caption,
  EmptyState,
  Head,
  Notice,
  Panel,
  Refine,
  Select,
  formatFreshness,
  formatMoment,
  useNow,
} from "../../kit";
import { ActionBar, type Act, type ActionBarTone } from "../../kit/ActionBar";
import { RecordList, RecordRow, type RecordTone } from "../../kit/RecordRow";
import {
  automationBand,
  automationMatches,
  constructOriginWord,
  rung,
  rungMeaning,
  rungWord,
  statusMeaning,
  statusWord,
  type AutomationRow,
  type Origin,
} from "./automations";
import { evidenceLine, ladderWord, procedureBand, procedureTitle, type ProcedureRow } from "./ladder";
import { ProcedurePage } from "./ProcedurePage";
import { useSetAutomationStatus, type AutomationsRead, PROCEDURE_PAGE_BOUND } from "./useAutomations";
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
// IT IS A READ AND IT SAYS WHEN IT LOOKED
// ===========================================================================
// `v1:authoring:construct` carries no broadcast routing rule, so there is
// nothing to subscribe to -- see useAutomations.ts, which records the check
// rather than the assumption. A live-looking list that silently never moves is
// worse than a read that dates itself.
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
  catalog: AutomationsRead;
  selectedId: string;
  onSelect: (constructId: string) => void;
  /** The learned procedure whose page is open, or "". */
  openProcedureId: string;
  /**
   * When that procedure was asked for, when the catalog in hand did not hold
   * it -- "" otherwise. Only a read that FINISHED after it may say the
   * procedure is not there.
   */
  openProcedureSince: string;
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
  | { origin: "authored"; key: string; band: number; reliability: number; name: string; automation: AutomationRow }
  | { origin: "learned"; key: string; band: number; reliability: number; name: string; procedure: ProcedureRow };

export function AutomationsSection({
  catalog,
  selectedId,
  onSelect,
  openProcedureId,
  openProcedureSince,
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
  // a later read happened to find it, which is navigation nobody asked for.
  const [missing, setMissing] = useState("");
  const status = useSetAutomationStatus();

  const openProcedure =
    openProcedureId === ""
      ? null
      : (catalog.procedures.find((p) => idTail(p.id) === idTail(openProcedureId)) ?? null);

  useEffect(() => {
    if (openProcedureId === "" || openProcedure !== null) return;
    if (catalog.state !== "ready" && catalog.state !== "error") return;
    // A read that finished BEFORE the ask cannot answer it.
    if (openProcedureSince !== "" && catalog.readAt < openProcedureSince) return;
    setMissing(openProcedureId);
    onCloseProcedure();
  }, [openProcedureId, openProcedureSince, openProcedure, catalog.state, catalog.readAt]);

  const entries = useMemo(() => {
    const needle = search.trim().toLowerCase();
    const out: Entry[] = [];
    if (origin !== "learned") {
      for (const automation of catalog.automations) {
        if (!automationMatches(automation, search)) continue;
        out.push({
          origin: "authored",
          key: `a:${automation.id}`,
          band: automationBand(automation),
          reliability: automation.reliability,
          name: automation.name,
          automation,
        });
      }
    }
    if (origin !== "authored") {
      for (const procedure of catalog.procedures) {
        const title = procedureTitle(procedure);
        const haystack = [title, procedure.name, ladderWord(procedure.ladder), "learned"]
          .join(" ")
          .toLowerCase();
        if (needle !== "" && !haystack.includes(needle)) continue;
        out.push({
          origin: "learned",
          key: `p:${procedure.id}`,
          band: procedureBand(procedure),
          reliability: procedure.reliability ?? 0,
          name: title,
          procedure,
        });
      }
    }
    // BANDS A PERSON CAN EXPLAIN, then how far each has climbed, then the name.
    // The one waiting on you first; then what serves; then what is still
    // earning it; then what serves nothing. A total order: two rows with the
    // same band, reliability and name would otherwise swap between reads.
    return out.sort((a, b) => {
      if (a.band !== b.band) return a.band - b.band;
      if (a.reliability !== b.reliability) return b.reliability - a.reliability;
      const byName = a.name.localeCompare(b.name);
      return byName !== 0 ? byName : a.key.localeCompare(b.key);
    });
  }, [catalog.automations, catalog.procedures, search, origin]);

  const selected =
    entries.find(
      (e): e is Extract<Entry, { origin: "authored" }> =>
        e.origin === "authored" && idTail(e.automation.id) === idTail(selectedId),
    )?.automation ?? null;

  if (openProcedure !== null) {
    return (
      <ProcedurePage
        procedure={openProcedure}
        policy={catalog.policy}
        policyError={catalog.policyError}
        approvals={approvals}
        approvalsKnown={approvalsKnown}
        runs={runs}
        readAt={catalog.readAt}
        reading={catalog.state === "loading"}
        onBack={onCloseProcedure}
        onLookAgain={catalog.read}
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
    // why.
    if (selected.status !== "active") {
      acts.push({
        label: "Arm it",
        tone: "primary",
        busy: status.busy === selected.id,
        ariaLabel: `Arm ${selected.name}: register it in the shared runtime so a matching goal replays it without a model`,
        onAct: () => {
          void status.set(selected.id, "active").then((ok) => {
            if (ok) catalog.read();
          });
        },
      });
    }
    if (selected.status === "active") {
      acts.push({
        label: "Retire it",
        tone: "danger",
        busy: status.busy === selected.id,
        ariaLabel: `Retire ${selected.name}: it stays readable so runs that used it can be explained, and is never selected again`,
        onAct: () => {
          void status.set(selected.id, "retired").then((ok) => {
            if (ok) catalog.read();
          });
        },
      });
    }
  }

  const tone: ActionBarTone =
    selected === null ? "none" : selected.status === "active" ? "live" : "paused";
  const total = catalog.automations.length + catalog.procedures.length;
  const anyAuthored = catalog.automations.length > 0;
  const chips =
    origin === ""
      ? []
      : [{ id: "origin", label: origin === "learned" ? "learned" : "authored", onRemove: () => setOrigin("") }];

  return (
    <div className="os-nexus-automations">
      <Head title="Automations" meta={`${entries.length} ${entries.length === 1 ? "automation" : "automations"}`}>
        {/* NO FILTER CHROME OVER NO CONTENT (rule 2): with nothing in the
            catalog there is no question to ask of it. */}
        {total === 0 ? null : (
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
        <Button onClick={catalog.read} busy={catalog.state === "loading"}>
          <RefreshCw size={13} aria-hidden />
          Look again
        </Button>
      </Head>

      <div className="os-nexus-split">
        <div className="os-nexus-column">
          {catalog.error !== "" ? (
            <Notice
              tone="error"
              sentence="The catalog could not be read."
              next="Look again once the cluster answers."
              detail={catalog.error}
            />
          ) : null}
          {catalog.proceduresError !== "" ? (
            <Notice
              tone="warn"
              sentence="The learned procedures could not be read."
              next="Every authored automation is still listed. Look again once the cluster answers."
              detail={catalog.proceduresError}
            />
          ) : null}
          {missing === "" ? null : (
            <Notice
              tone="info"
              sentence="That procedure is not in your catalog."
              next="It may have been learned on another account's runs, or its id has changed. Every procedure you own is listed below."
              detail={missing}
            />
          )}

          {entries.length === 0 ? null : (
            <RecordList label="Automations this instance can replay">
              {entries.map((entry) =>
                entry.origin === "authored" ? (
                  <AuthoredRow
                    key={entry.key}
                    automation={entry.automation}
                    open={idTail(entry.automation.id) === idTail(selectedId)}
                    onOpen={() => {
                      setMissing("");
                      onSelect(entry.automation.id);
                    }}
                  />
                ) : (
                  <LearnedRow
                    key={entry.key}
                    procedure={entry.procedure}
                    catalog={catalog}
                    now={now}
                    onOpen={() => {
                      setMissing("");
                      onOpenProcedure(entry.procedure.id);
                    }}
                  />
                ),
              )}
            </RecordList>
          )}

          {entries.length === 0 ? (
            catalog.state === "loading" ? (
              <Caption>Reading the catalog</Caption>
            ) : search.trim() !== "" || (origin !== "" && total > 0) ? (
              <Caption>No automation here matches that.</Caption>
            ) : catalog.error !== "" ? null : (
              // AN EMPTY SCREEN IS AN INVITATION, and here the invitation is
              // not a button -- nobody authors an automation by hand in this
              // app. It says where they come from instead. Settled emptiness
              // only: a refused read already said so above.
              <EmptyState title="Nothing here yet" icon={Repeat}>
                <p>
                  An automation appears when a goal is worked out: the system compiles what it
                  decided into a template, and a template that keeps succeeding earns its way up
                  this list.
                </p>
                <p>
                  A procedure appears when the system has watched an app do the same work more than
                  once. It replays beside the app first, and runs without a model only once it has
                  earned it.
                </p>
              </EmptyState>
            )
          ) : null}

          <Caption>
            {catalog.readAt === ""
              ? "Not read yet."
              : `Read ${formatFreshness(catalog.readAt, now)}.`}{" "}
            This list is not live -- the catalog broadcasts nothing, so it is a read that dates
            itself rather than a list that would silently never move.
            {catalog.bounded
              ? ` Showing the first ${catalog.scanned} catalogued constructs; there may be more.`
              : ""}
            {catalog.proceduresBounded
              ? ` Showing the first ${PROCEDURE_PAGE_BOUND} learned procedures; there may be more.`
              : ""}
          </Caption>
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
        state={
          selected !== null
            ? statusWord(selected.status)
            : entries.length === 0
              ? "Nothing yet"
              : "Nothing selected"
        }
        detail={
          selected !== null
            ? statusMeaning(selected.status)
            : entries.length === 0
              ? // "Select an automation" on an empty list is an instruction for
                // a list that does not exist. Say what IS true instead.
                catalog.state === "loading"
                  ? "reading the catalog"
                  : "nothing to arm yet"
              : anyAuthored
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

function AuthoredRow({
  automation,
  open,
  onOpen,
}: {
  automation: AutomationRow;
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
      stateExtra={<RungMark automation={automation} />}
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
  catalog,
  now,
  onOpen,
}: {
  procedure: ProcedureRow;
  catalog: AutomationsRead;
  now: Date;
  onOpen: () => void;
}) {
  const line = evidenceLine(procedure, catalog.policy);
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
        <span className="os-nexus-procedure-fresh" title={procedure.lastReplayAt === "" ? undefined : formatMoment(procedure.lastReplayAt)}>
          {procedure.lastReplayAt === "" ? "never replayed" : `replayed ${formatFreshness(procedure.lastReplayAt, now)}`}
        </span>
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
