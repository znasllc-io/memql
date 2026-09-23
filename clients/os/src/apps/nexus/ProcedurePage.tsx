import type { CSSProperties, ReactNode } from "react";
import { Check, RefreshCw } from "lucide-react";

import {
  Button,
  Caption,
  Chip,
  CopyValue,
  Fact,
  Facts,
  Head,
  Notice,
  Panel,
  Subhead,
  formatFreshness,
  formatMoment,
  useNow,
} from "../../kit";
import { ActionBar, type Act, type ActionBarTone } from "../../kit/ActionBar";
import { rungFrom, rungWord as reliabilityWord } from "./automations";
import {
  appWord,
  climbingRung,
  countFigure,
  countSentence,
  currentStateWords,
  evidenceCounts,
  freeParametersOf,
  holeById,
  ladderWord,
  nextRungSentence,
  parameterIsNamed,
  parameterName,
  procedureTitle,
  promotionWaiting,
  rungMeaning,
  rungStates,
  sentence,
  stepArgs,
  toolMeaning,
  toolWord,
  zeroCountWord,
  type ArgToken,
  type EvidenceCount,
  type LadderPolicy,
  type ProcedureRow,
} from "./ladder";
import { idTail, runTitle, type ApprovalRow, type RunRow } from "./rows";

// ONE LEARNED PROCEDURE: where it stands, what it has proven, and the one
// decision it may be waiting on (epic memql#5408, #5412).
//
// ===========================================================================
// IT REPLACES THE LIST (DESIGN.md rule 11)
// ===========================================================================
// A procedure is a ladder, its evidence, where it was recorded, what it checks
// before it starts, and every step it would run -- taller than the authored
// automation's aside, so this is the `<- Automations` form of the rule. The
// window's trail row draws the way back; this page only publishes it.
//
// ===========================================================================
// THE LADDER IS THE ONE LOUD THING ON THE PAGE
// ===========================================================================
// Four rungs drawn as a stair: the part it has climbed in the accent, the rest
// quiet, and the rung it stands on saying in words what is happening there.
// Under that rung, the evidence that moves it, COUNTED -- marks a person can
// check against "5 matches in a row", never a percentage -- and one sentence
// of what the next rung needs, computed from the cluster's own policy row.
// Everything else on the page is Panel, Subhead and Facts, so the eye lands on
// the ladder first and finds the record where it expects it.
//
// ===========================================================================
// ONE ACT, AND ONLY WHEN IT EXISTS (rule 12)
// ===========================================================================
// A procedure climbs and falls on its own evidence. The one decision a person
// makes about it is the promotion from shadow to canary, and that decision is
// made on the approval, beside its evidence -- so the only act here is the way
// to it, offered only while one is open. Nothing on this page arms, retires or
// promotes: `procedureStep` refuses outside a replay, and a learned procedure
// is served through the ladder and nothing else.

export interface ProcedurePageProps {
  procedure: ProcedureRow;
  /** The cluster's ladder values, or null when it published none. */
  policy: LadderPolicy | null;
  /** The policy read's own refusal, verbatim. */
  policyError: string;
  /** The pending approvals feed, to tell an open promotion from a decided one. */
  approvals: readonly ApprovalRow[];
  /** Whether that feed has answered at all -- until it has, it proves nothing. */
  approvalsKnown: boolean;
  runs: readonly RunRow[];
  readAt: string;
  reading: boolean;
  onBack: () => void;
  onLookAgain: () => void;
  onOpenApproval: (approvalId: string) => void;
  onOpenRun: (runId: string) => void;
}

export function ProcedurePage({
  procedure: p,
  policy,
  policyError,
  approvals,
  approvalsKnown,
  runs,
  readAt,
  reading,
  onBack,
  onLookAgain,
  onOpenApproval,
  onOpenRun,
}: ProcedurePageProps) {
  const now = useNow(30_000);
  const waiting = promotionWaiting(p);
  // THE CATALOG IS A READ AND THE APPROVALS ARE A FEED, so they can disagree:
  // a promotion decided a minute ago leaves the feed at once, while this row
  // still names it until the catalog is read again. An act that opened an
  // approval nobody is waiting on would be a door to an empty room, so once
  // the feed has answered it is what says whether the proposal is still open.
  const approvalOpen =
    waiting &&
    (!approvalsKnown || approvals.some((a) => idTail(a.id) === idTail(p.promotionApprovalId)));
  const decidedSinceRead = waiting && !approvalOpen;

  const acts: Act[] = [];
  if (approvalOpen) {
    acts.push({
      label: "Review promotion",
      tone: "primary",
      ariaLabel: "Review promotion: open the approval that would move this procedure to canary",
      onAct: () => onOpenApproval(p.promotionApprovalId),
    });
  }

  const tone: ActionBarTone = approvalOpen
    ? "paused"
    : p.ladder === "canary" || p.ladder === "trusted"
      ? "live"
      : "none";

  return (
    <div className="os-nexus-automations os-nexus-procedure">
      <Head title={procedureTitle(p)} meta={ladderWord(p.ladder)} back={{ label: "Automations", onSelect: onBack }}>
        <Button onClick={onLookAgain} busy={reading}>
          <RefreshCw size={13} aria-hidden />
          Look again
        </Button>
      </Head>

      <div className="os-nexus-procedure-body">
        <Panel label="Where it stands">
          <Subhead>Where it stands</Subhead>
          {climbingRung(p.ladder) === null ? (
            <OffTheLadder procedure={p} now={now} />
          ) : (
            <Ladder procedure={p} policy={policy} decidedSinceRead={decidedSinceRead} />
          )}
          {policyError === "" ? null : (
            <Notice
              tone="warn"
              sentence="The ladder's values could not be read."
              next="The counts above are this procedure's own; how many each rung needs is not shown."
              detail={policyError}
            />
          )}
        </Panel>

        <div className="os-nexus-procedure-grid">
          <EvidencePanel procedure={p} now={now} />
          <RecordedFromPanel procedure={p} runs={runs} onOpenRun={onOpenRun} />
          <PreconditionsPanel procedure={p} />
        </div>

        <StepsPanel procedure={p} />

        <Caption>
          {readAt === "" ? "Not read yet." : `Read ${formatFreshness(readAt, now)}.`} Not live: the
          catalog broadcasts nothing, so this page is as of that read. The ladder moves on its own
          evidence between reads.
        </Caption>
      </div>

      <ActionBar
        state={ladderWord(p.ladder)}
        detail={barDetail(p, approvalOpen, decidedSinceRead)}
        tone={tone}
        acts={acts}
      />
    </div>
  );
}

/** What the bar says the state MEANS -- and why nothing else is offered. */
function barDetail(p: ProcedureRow, approvalOpen: boolean, decidedSinceRead: boolean): string {
  if (approvalOpen) return "promotion to canary is waiting for your decision";
  if (decidedSinceRead) return "the promotion was decided; look again to see where it stands";
  switch (p.ladder) {
    case "candidate":
      return "learned from your runs; it climbs on its own evidence";
    case "shadow":
      return "replays beside the app, which still answers every goal";
    case "canary":
      return "runs for real, with the app standing by to take over";
    case "trusted":
      return "runs without a model; the app takes over if a replay diverges";
    case "retired":
      return "serves nothing; a changed procedure comes back as a new candidate";
    default:
      return "never served";
  }
}

// ---------------------------------------------------------------------------
// The ladder
// ---------------------------------------------------------------------------

function Ladder({
  procedure: p,
  policy,
  decidedSinceRead,
}: {
  procedure: ProcedureRow;
  policy: LadderPolicy | null;
  decidedSinceRead: boolean;
}) {
  const states = rungStates(p.ladder);
  const at = states.findIndex((s) => s.state === "current");
  const waiting = promotionWaiting(p) && !decidedSinceRead;
  const counts = evidenceCounts(p, policy);
  const next = decidedSinceRead
    ? "The promotion it was waiting on has been decided. Look again to see which way."
    : nextRungSentence(p, policy);
  const noParameter = p.ladder === "shadow" && freeParametersOf(p).length === 0;

  return (
    // THE FRAME IS THE CONTAINER, so the ladder's own grid can answer its width.
    <div className="os-nexus-ladder-frame">
      <div className="os-nexus-ladder" data-waiting={waiting || undefined}>
        <ol className="os-nexus-ladder-rungs" aria-label="The certification ladder, lowest rung first">
          {states.map(({ rung, state }, i) => (
            <li
              key={rung}
              className="os-nexus-ladder-rung"
              data-state={state}
              data-rung={rung}
              aria-current={state === "current" ? "step" : undefined}
              style={{ "--os-ladder-step": i, "--os-ladder-row": uprightRow(i, at, states.length) } as CSSProperties}
            >
              {i === 0 ? null : <span className="os-nexus-ladder-riser" aria-hidden />}
              <span className="os-nexus-ladder-tread" aria-hidden />
              <span className="os-nexus-ladder-mark" aria-hidden>
                {state === "passed" ? <Check size={11} strokeWidth={2.5} /> : null}
              </span>
              <span className="os-nexus-ladder-name">{ladderWord(rung)}</span>
              <span className="os-nexus-ladder-meaning">{rungMeaning(rung)}</span>
              {state === "current" ? (
                <span className="os-nexus-ladder-now">{currentStateWords(p)}</span>
              ) : (
                // The state in words for a reader who cannot see the marks:
                // the check and the empty ring are the only other difference.
                <span className="os-visually-hidden">{state === "passed" ? "Passed" : "Not reached"}</span>
              )}
            </li>
          ))}
        </ol>

        <div
          className="os-nexus-ladder-evidence"
          role="group"
          aria-label="What moves it"
          // HUNG FROM THE RUNG IT STANDS ON, on the side with room: to the right
          // of every rung but the top one, whose column is the last -- that one
          // hangs to its left, so its sentence is not folded into a quarter of
          // the panel and its thread still drops from its own mark.
          data-side={at === states.length - 1 ? "left" : "right"}
          data-rungs-below={at > 0 || undefined}
          style={
            {
              "--os-ladder-from": Math.max(at, 0) + 1,
              "--os-ladder-evidence-row": at < 0 ? states.length + 1 : states.length + 1 - at,
            } as CSSProperties
          }
        >
          {counts.length === 0 ? null : (
            <ul className="os-nexus-ladder-counts">
              {counts.map((c) => (
                <li key={`${c.unit}:${c.label}`}>
                  <Count count={c} />
                </li>
              ))}
            </ul>
          )}
          {noParameter ? (
            <p className="os-nexus-ladder-aside">No parameter to vary: every run passes it the same values.</p>
          ) : null}
          <p className="os-nexus-ladder-next">{next}</p>
        </div>
      </div>
    </div>
  );
}

/**
 * A rung's row when the ladder stands upright: highest rung on top, and the
 * evidence in the row just under the rung it hangs from, so every rung below
 * that one moves down a row. Laid flat, the rungs fill their columns and this
 * is not read.
 */
function uprightRow(i: number, at: number, rungs: number): number {
  return at >= 0 && i < at ? rungs + 1 - i : rungs - i;
}

/** One count, as marks beside its figure. */
function Count({ count: c }: { count: EvidenceCount }) {
  // The marks: filled to the count, empty up to the threshold. With NO
  // threshold published the count alone is drawn -- every mark filled, none
  // empty -- which says how many there are and invents nothing about how many
  // are needed. Past a dozen, marks stop being countable at a glance.
  const slots = c.need === null ? c.count : c.need;
  const drawn = slots > 0 && slots <= 12;
  return (
    <div className="os-nexus-ladder-count" data-tone={c.tone} role="img" aria-label={countSentence(c)}>
      <span className={c.labelIsData ? "os-nexus-ladder-count-label os-mono" : "os-nexus-ladder-count-label"}>
        {c.label}
      </span>
      {drawn ? (
        <span className="os-nexus-ladder-pips" aria-hidden>
          {Array.from({ length: slots }, (_, i) => (
            <span key={i} className="os-nexus-ladder-pip" data-on={i < c.count || undefined} />
          ))}
        </span>
      ) : (
        <span aria-hidden />
      )}
      <span className="os-nexus-ladder-count-figure" aria-hidden>
        {countFigure(c)}
      </span>
    </div>
  );
}

/**
 * Retired, or never on the ladder: a quiet account, not a fifth rung.
 *
 * THE RUNG IT LAST STOOD ON IS NOT NAMED, because the row does not keep it:
 * retirement writes `retired` over the rung and leaves counters that do not
 * say which rung they were earned on. So the notice says what IS recorded --
 * why it retired and when -- rather than a guess drawn as a fact.
 */
function OffTheLadder({ procedure: p, now }: { procedure: ProcedureRow; now: Date }) {
  if (p.ladder === "retired") {
    const when = p.ladderChangedAt === "" ? "" : ` ${formatFreshness(p.ladderChangedAt, now)}`;
    return (
      <div className="os-nexus-ladder-off">
        <p className="os-nexus-ladder-off-line">
          Retired{when}. It serves nothing now, on any rung.
        </p>
        {p.ladderReason === "" ? null : (
          <p className="os-caption" title={p.ladderChangedAt === "" ? undefined : formatMoment(p.ladderChangedAt)}>
            {sentence(p.ladderReason)}
          </p>
        )}
        <p className="os-caption">
          A retired procedure does not climb back. If what it learned changes, the next recording
          that shows it brings it back as a new candidate.
        </p>
      </div>
    );
  }
  return (
    <div className="os-nexus-ladder-off">
      <p className="os-nexus-ladder-off-line">
        {p.ladder === ""
          ? "It is not on the certification ladder, so nothing serves it."
          : `Its rung reads "${p.ladder}", which this window does not know, so it is shown as not served.`}
      </p>
      <p className="os-caption">
        A procedure is served only from the canary or trusted rung, and this one stands on neither.
      </p>
    </div>
  );
}

// ---------------------------------------------------------------------------
// The record
// ---------------------------------------------------------------------------

/**
 * Distinct bindings per parameter: how many, then WHICH parameter, by the goal
 * input that supplies it. The count leads so "month 2" is never read as a
 * value of month.
 */
export function BindingCounts({
  counts,
  inputMap,
}: {
  counts: ReadonlyArray<readonly [string, number]>;
  inputMap: Record<string, string>;
}) {
  return (
    <span className="os-nexus-procedure-lines">
      {counts.map(([id, n]) => (
        <span key={id}>
          {n} for <span className="os-mono">{parameterName(inputMap, id)}</span>
        </span>
      ))}
    </span>
  );
}

/** A streak, or what its zero says -- null when the fact is to be left out. */
function inARow(n: number, zero: string | null, one: string, many: string): string | null {
  return n === 0 ? zero : `${n} ${n === 1 ? one : many} in a row`;
}

/**
 * The record, in words.
 *
 * SAID ONCE. The counts the ladder draws as marks under the rung it stands on
 * are not repeated here -- a shadow procedure's matches and bindings are up
 * there, a canary's clean replays, a falling procedure's failures -- so this
 * panel holds the rest: why it last moved, when it last replayed, the counts
 * that belong to other rungs, and the reliability word.
 */
function EvidencePanel({ procedure: p, now }: { procedure: ProcedureRow; now: Date }) {
  const parameters = freeParametersOf(p);
  const onLadder = climbingRung(p.ladder) !== null;
  const shownAbove = (what: "shadow" | "canary" | "failures" | "insufficient") => {
    if (what === "shadow") return p.ladder === "shadow";
    if (what === "canary") return p.ladder === "canary";
    const falling = p.ladder === "canary" || p.ladder === "trusted";
    return falling && (what === "failures" ? p.failures > 0 : p.insufficient > 0);
  };
  // A ZERO SAYS WHERE IT STANDS: "none yet" below the rung a count is earned
  // on, "none" off the ladder, and nothing at all past it -- a passed rung's
  // counter may have been reset on the way up, and "none" would be false.
  const shadowZero = zeroCountWord(p.ladder, "shadow");
  const shadowMatches = inARow(p.shadowMatches, shadowZero, "match", "matches");
  const bindings = parameters.map((id) => [id, (p.distinctBindings[id] ?? []).length] as const);
  const noBindings = bindings.every(([, n]) => n === 0);
  const hideBindings = parameters.length > 0 && noBindings && shadowZero === null;
  const canaryReplays =
    p.canaryMatches === 0 ? zeroCountWord(p.ladder, "canary") : `${p.canaryMatches} clean in a row`;
  return (
    <Panel label="Evidence">
      <Subhead>Evidence</Subhead>
      {/* WHY IT LAST MOVED IS A SENTENCE, NOT A VALUE, so it leads the panel
          at a sentence's measure rather than squeezing into the value column
          beside six short facts. Off the ladder the notice above carries it
          as its headline instead. */}
      {!onLadder || p.ladderReason === "" ? null : (
        <div className="os-nexus-procedure-group">
          <p className="os-nexus-procedure-group-name">Why it last moved</p>
          <p className="os-nexus-procedure-reason">
            {sentence(p.ladderReason)}
            {p.ladderChangedAt === "" ? null : (
              <>
                {" "}
                <span className="os-nexus-procedure-when" title={formatMoment(p.ladderChangedAt)}>
                  {formatFreshness(p.ladderChangedAt, now)}
                </span>
              </>
            )}
          </p>
        </div>
      )}
      <Facts>
        <Fact
          label="Last replay"
          value={p.lastReplayAt === "" ? "never" : formatFreshness(p.lastReplayAt, now)}
          title={p.lastReplayAt === "" ? undefined : formatMoment(p.lastReplayAt)}
        />
        {shownAbove("shadow") || shadowMatches === null ? null : (
          <Fact label="Shadow matches" value={shadowMatches} />
        )}
        {shownAbove("shadow") || hideBindings ? null : (
          <Fact
            label="Distinct bindings"
            value={
              parameters.length === 0 ? (
                "no parameter to vary"
              ) : noBindings ? (
                shadowZero
              ) : (
                <BindingCounts counts={bindings} inputMap={p.inputMap} />
              )
            }
          />
        )}
        {shownAbove("canary") || canaryReplays === null ? null : (
          <Fact label="Canary replays" value={canaryReplays} />
        )}
        {shownAbove("failures") ? null : (
          <Fact label="Failed replays" value={p.failures === 0 ? "none" : `${p.failures} in a row`} />
        )}
        {shownAbove("insufficient") ? null : (
          <Fact
            label="Failed after passing checks"
            value={p.insufficient === 0 ? "none" : `${p.insufficient} ${p.insufficient === 1 ? "replay" : "replays"}`}
          />
        )}
        {/* A WORD, AND ONLY WHEN WRITTEN. `reliability` is 0..1 and it is not
            odds; a procedure the ladder never reinforced has no reading at
            all, which is different from a poor one. */}
        {p.reliability === null ? null : (
          <Fact label="Reliability" value={reliabilityWord(rungFrom(p.reliability, p.reinforceCount))} />
        )}
      </Facts>
    </Panel>
  );
}

function RecordedFromPanel({
  procedure: p,
  runs,
  onOpenRun,
}: {
  procedure: ProcedureRow;
  runs: readonly RunRow[];
  onOpenRun: (runId: string) => void;
}) {
  const from = p.recordedFrom;
  return (
    <Panel label="Recorded from">
      <Subhead>Recorded from</Subhead>
      <Facts>
        <Fact label="App" value={appWord(from.app)} />
        <Fact label="Model" value={from.model} mono />
        <Fact label="Effort" value={from.effort} />
        <Fact
          label={from.sessionIds.length === 1 ? "Session" : "Sessions"}
          value={
            from.sessionIds.length === 0 ? (
              ""
            ) : (
              <span className="os-nexus-procedure-lines">
                {from.sessionIds.map((id) => (
                  <CopyValue key={id} value={id} label="session id" />
                ))}
              </span>
            )
          }
        />
        <Fact
          label={from.runIds.length === 1 ? "Recording" : "Recordings"}
          value={
            from.runIds.length === 0 ? (
              ""
            ) : (
              <span className="os-nexus-procedure-lines">
                {from.runIds.map((runId) => {
                  const run = runs.find((r) => idTail(r.id) === idTail(runId)) ?? null;
                  // A RUN THIS WINDOW DOES NOT HOLD IS TEXT, not a link that
                  // would land on the Runs list and select nothing.
                  return run === null ? (
                    <span key={runId} className="os-mono">
                      {idTail(runId)}
                    </span>
                  ) : (
                    <button
                      key={runId}
                      type="button"
                      className="os-nexus-link"
                      onClick={() => onOpenRun(run.id)}
                    >
                      {runTitle(run)}
                    </button>
                  );
                })}
              </span>
            )
          }
        />
        <Fact label="Name" value={p.name} mono />
        <Fact
          label="Version"
          value={
            <span className="os-nexus-procedure-lines">
              <CopyValue value={p.procedureHash} label="construct version" />
            </span>
          }
        />
      </Facts>
    </Panel>
  );
}

/**
 * The learned initiation set, split by what a replay does with it.
 *
 * TWO GROUPS, BECAUSE THEY ARE TWO PROMISES. The tools and the workspace are
 * CHECKED before every replay, and one that does not hold sends the goal to
 * the app instead. The variables are recorded and NEVER checked (coordinator
 * decision, Task 2): a replay runs in the worker's or the workbench's
 * environment, never the app's, so they describe somewhere the replay is not
 * -- and the platform is compared only when the replay runs on the machine it
 * was recorded on. One list with a paragraph under it made a person work out
 * which line was a gate; the split says it where the line is.
 */
function PreconditionsPanel({ procedure: p }: { procedure: ProcedureRow }) {
  const pre = p.preconditions;
  const platform = [pre.os, pre.arch].filter((s) => s !== "").join(" ");
  const platformChecked = platform !== "" && p.target === "machine";
  const nothing =
    platform === "" && pre.tools.length === 0 && pre.variables.length === 0 && pre.emptyWorkspace === null;
  const checked = pre.tools.length > 0 || pre.emptyWorkspace !== null || platformChecked;
  const recorded = pre.variables.length > 0 || (platform !== "" && !platformChecked);
  const platformFact = <Fact label="Platform" value={platform} mono />;
  return (
    <Panel label="Preconditions">
      <Subhead>Preconditions</Subhead>
      {nothing ? (
        <Caption>
          Nothing learned. The recordings agreed on nothing to check before a replay starts.
        </Caption>
      ) : (
        <>
          {!checked ? null : (
            <div className="os-nexus-procedure-group">
              <p className="os-nexus-procedure-group-name">Checked before every replay</p>
              <Facts>
                {pre.tools.length === 0 ? null : (
                  <Fact
                    label={pre.tools.length === 1 ? "Tool" : "Tools"}
                    value={
                      <span className="os-nexus-procedure-lines">
                        {pre.tools.map(([name, version]) => (
                          <span key={name} className="os-mono">
                            {name} {version}
                          </span>
                        ))}
                      </span>
                    }
                  />
                )}
                {pre.emptyWorkspace === null ? null : (
                  <Fact label="Workspace" value={pre.emptyWorkspace ? "Starts empty" : "Starts with files already in it"} />
                )}
                {platformChecked ? platformFact : null}
              </Facts>
            </div>
          )}
          {!recorded ? null : (
            <div className="os-nexus-procedure-group">
              <p className="os-nexus-procedure-group-name">Recorded, not checked</p>
              <Facts>
                {platform !== "" && !platformChecked ? platformFact : null}
                {pre.variables.length === 0 ? null : (
                  <Fact
                    label={pre.variables.length === 1 ? "Variable" : "Variables"}
                    value={
                      <span className="os-nexus-procedure-lines">
                        {pre.variables.map(([name, value]) => (
                          <span key={name}>
                            <span className="os-mono">{name}</span> {value === "unset" ? "not set" : "set"}
                          </span>
                        ))}
                      </span>
                    }
                  />
                )}
              </Facts>
            </div>
          )}
          <Caption>
            {checked
              ? "A check that does not hold sends the goal to the app instead. What is only recorded describes where the app ran, not where a replay runs."
              : "What is only recorded describes where the app ran, not where a replay runs."}
          </Caption>
        </>
      )}
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// The steps
// ---------------------------------------------------------------------------

function targetWord(target: string): string {
  switch (target) {
    case "workbench":
      return "The workbench, a sandbox in the cluster";
    case "machine":
      return "Your machine";
    default:
      return target;
  }
}

function footprintWords(p: ProcedureRow): string {
  const f = p.footprint;
  const out: string[] = [];
  if (f.files) out.push("writes files");
  if (f.machine) out.push("acts on your machine");
  if (f.external) out.push("reaches outside the cluster");
  if (f.spend) out.push("costs money");
  if (f.concepts.length > 0) out.push("writes to the graph");
  if (out.length === 0) return "Nothing outside its own workspace";
  const text = out.join(", ");
  return text.charAt(0).toUpperCase() + text.slice(1);
}

function StepsPanel({ procedure: p }: { procedure: ProcedureRow }) {
  const hasHoles = p.holes.length > 0;
  return (
    <Panel label="Steps">
      <Subhead>Steps</Subhead>
      <Facts>
        <Fact label="Runs on" value={targetWord(p.target)} />
        <Fact label="Touches" value={footprintWords(p)} />
      </Facts>
      {p.steps.length === 0 ? (
        <Caption>No steps were recorded with this procedure.</Caption>
      ) : (
        <ol className="os-nexus-procedure-steps" aria-label="Steps, in the order they run">
          {p.steps.map((step, i) => {
            const args = stepArgs(step.args);
            return (
              <li key={i} className="os-nexus-procedure-step">
                <span className="os-nexus-procedure-step-n" aria-hidden>
                  {i + 1}
                </span>
                <span className="os-nexus-procedure-step-tool" title={toolMeaning(step.tool) || undefined}>
                  {toolWord(step.tool)}
                </span>
                <div className="os-nexus-procedure-step-args">
                  {args.primary.length === 0 ? null : (
                    <code className="os-nexus-procedure-arg">
                      <Tokens tokens={args.primary} procedure={p} />
                    </code>
                  )}
                  {args.rest.map((arg) => (
                    <div key={arg.key} className="os-nexus-procedure-arg-rest">
                      <span className="os-nexus-procedure-arg-key">{arg.key}</span>
                      <code className="os-nexus-procedure-arg">
                        <Tokens tokens={arg.tokens} procedure={p} />
                      </code>
                    </div>
                  ))}
                </div>
              </li>
            );
          })}
        </ol>
      )}
      {hasHoles ? (
        <Caption>
          A value in a chip changes from goal to goal. Its name is the goal input that supplies it;
          &quot;from step&quot; is taken from an earlier step&apos;s result.
        </Caption>
      ) : null}
    </Panel>
  );
}

function Tokens({ tokens, procedure: p }: { tokens: ArgToken[]; procedure: ProcedureRow }) {
  return (
    <>
      {tokens.map((token, i) =>
        token.kind === "text" ? (
          <span key={i}>{token.text}</span>
        ) : (
          <HoleChip key={i} holeId={token.holeId} procedure={p} />
        ),
      )}
    </>
  );
}

/**
 * A position the recordings disagreed on, drawn as what explains it.
 *
 * A FREE parameter is named by the goal input that binds it -- the word the
 * person will recognise from their own goal -- in the accent, because it is
 * the part a goal supplies. A constant is simply its value: every recording
 * agreed, so it is not a question. A data-flow value names the step it comes
 * from. One nothing explains says so; it never pretends to be a literal.
 */
function HoleChip({ holeId, procedure: p }: { holeId: string; procedure: ProcedureRow }): ReactNode {
  const hole = holeById(p, holeId);
  const cls = hole?.cls ?? "";
  if (cls === "constant" && hole !== null && hole.constValue !== "") return <span>{hole.constValue}</span>;
  if (cls === "dataflow" && hole?.ref) {
    const path = hole.ref.path.join(".");
    return (
      <Chip tone="neutral" title={`Taken from step ${hole.ref.stepIndex + 1}'s result${path === "" ? "" : ` at ${path}`}`}>
        from step {hole.ref.stepIndex + 1}
      </Chip>
    );
  }
  const free = cls === "free" || p.freeParameters.includes(holeId);
  if (free) {
    const named = parameterIsNamed(p.inputMap, holeId);
    return (
      <Chip
        tone="accent"
        title={named ? `Supplied by the goal's ${parameterName(p.inputMap, holeId)}` : "A value the goal supplies"}
      >
        {parameterName(p.inputMap, holeId)}
      </Chip>
    );
  }
  return (
    <Chip tone="muted" title="The recordings disagreed here and nothing explains it yet">
      {holeId}
    </Chip>
  );
}
