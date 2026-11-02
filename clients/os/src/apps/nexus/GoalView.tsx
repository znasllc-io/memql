import { summarizeUsage, usageLabel } from "./modelUsage";
import { UsageDetails } from "./UsageDetails";
import type { ModelUsageState } from "./useModelUsage";
import { stepTitle, stepOutcome } from "../../nexus/presentation";
import { StepSummary } from "./StepSummary";
import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { GitBranch, History, RotateCcw } from "lucide-react";
import type { LiveState, Row } from "@znasllc-io/memql-sdk-core/client";

import {
  Caption,
  Chip,
  Chips,
  Fact,
  Facts,
  Head,
  Notice,
  Panel,
  formatFreshness,
  formatMoment,
  useNow,
} from "../../kit";
import { ActionBar, type Act, type ActionBarTone } from "../../kit/ActionBar";
import { events, timelineBounds, type SceneEvent } from "../../nexus/scene/events";
import { NOW, existedAt, goalProgress, scene, stepStatusAt } from "../../nexus/scene/scene";
import { formatElapsed, receipt } from "../../nexus/scene/receipt";
import { BeaconMap } from "./BeaconMap";
import { KindBand } from "./KindBand";
import { GoalJourney } from "./GoalJourney";
import { ComposerHost, type ComposerRequest } from "./ComposerHost";
import type { CancelGoalState, DeriveRunState } from "./actions";
import {
  formatMoney,
  formatTokens,
  RUN_TERMINAL,
  idTail,
  goalTitle,
  kindBreakdown,
  runFromRow,
  runSpend,
  stepFromRow,
  stepsInOrder,
  type GoalRow,
  type StepRow,
} from "./rows";
import type { ComposerDraft } from "./versions";
import { buildWorld } from "./world";
import { goalStatusWord, originWord, runModeWord, runStatusWord, runWithoutStepsSummary } from "./words";

// THE GOAL VIEW: one goal, as a place the work arrives at.
//
// ===========================================================================
// IT REPLACES THE LIST (DESIGN.md rule 11, the `<- Goals` form)
// ===========================================================================
// A map plus a rail plus a detail is tall -- taller than the run page, which
// already took this form -- so the list goes and this comes back with a way
// back to it. Two Heads in one scroller is the tell that neither happened.
//
// ===========================================================================
// ONE SELECTION, HELD AS A STEP KEY
// ===========================================================================
// The map and the rail are two drawings of one run and they share a selection,
// which is held as a KEY rather than as a node or a row: the layout re-derives
// on every event, and a retry writes a new row, so anything else would go
// stale the moment the thing it named moved. Clicking a node frames its step
// in the rail; clicking a rail row lights its node.
//
// ===========================================================================
// REWIND IS A MODE, NOT A SECTION
// ===========================================================================
// Same map, same rail, same selection -- a scrubber appears and the world is
// drawn AT a moment instead of now. `events()` produces the moments from the
// rows' own timestamps and invents none, so the ticks are evidence.
//
// IT IS NOT `replayRun`, AND THE WORDS ARE DIFFERENT ON PURPOSE. Rewind reads
// rows this browser already has and costs nothing; Replay is an act that opens
// a NEW run served from the journal. A surface that used one word for both
// would have people spending money by dragging a slider.

export interface GoalViewProps {
  usage?: ModelUsageState;
  goal: GoalRow;
  /** The app root's raw feeds. The map narrows them itself; see `world.ts`. */
  goalRow: Row | null;
  runRows: readonly Row[];
  approvalRows: readonly Row[];
  stepRows: readonly Row[];
  stepsState: LiveState;
  /** What this run produced. Live, keyed on the same run as the steps. */
  artifactRows: readonly Row[];
  openRunId: string;
  onPickRun: (runId: string) => void;
  cancel: CancelGoalState;
  derive: DeriveRunState;
  onBack: () => void;
  onOpenRun: (runId: string) => void;
  onOpenApproval: (approvalId: string) => void;
  /** A moment handed in by an opener, so a rewound goal is shareable. */
  openAt?: string;
}

export function GoalView({
  goal,
  usage,
  goalRow,
  runRows,
  approvalRows,
  stepRows,
  stepsState,
  artifactRows,
  openRunId,
  onPickRun,
  cancel,
  derive,
  onBack,
  onOpenRun,
  onOpenApproval,
  openAt = "",
}: GoalViewProps) {
  const now = useNow(30_000);
  const [selectedStepKey, setSelectedStepKey] = useState("");
  const [expandedColumns, setExpandedColumns] = useState<ReadonlySet<number>>(new Set());
  const [expandedFolds, setExpandedFolds] = useState<ReadonlySet<number>>(new Set());
  const [at, setAt] = useState(openAt);
  const [confirm, setConfirm] = useState(false);
  const page = useRef<HTMLDivElement>(null);
  const body = useRef<HTMLDivElement>(null);
  const inspector = useRef<HTMLElement>(null);
  const revealSelection = useRef(false);
  const returnCloseFocus = useRef(false);
  // "Branch from here" opens the same composer the run page does, and its
  // drafts are kept the same way: Escape keeps what was typed.
  const [composer, setComposer] = useState<ComposerRequest | null>(null);
  const [drafts, setDrafts] = useState<Record<string, ComposerDraft>>({});

  const live = useMemo(
    () => buildWorld({ goalRow, runRows, stepRows, approvalRows, artifactRows, openRunId }),
    [goalRow, runRows, stepRows, approvalRows, artifactRows, openRunId],
  );
  const moments = useMemo(() => events(live), [live]);
  const bounds = useMemo(() => timelineBounds(moments), [moments]);
  const world = useMemo(() => scene(live, at === "" ? NOW : at), [live, at]);
  const progress = useMemo(() => goalProgress(world), [world]);
  const card = useMemo(() => receipt(world), [world]);

  // The rail reads the app's own step projection, which carries the readings
  // the spine draws in ink; the map reads the scene library's. Both are folded
  // from the same rows -- see `world.ts` on why there are two narrowings.
  //
  // AND BOTH FOLLOW THE SAME MOMENT. A rewound map beside a live rail is a page
  // showing two moments at once, which is worse than not offering rewind: the
  // map says three steps have landed and the list beside it says eleven. The
  // narrowing is the scene library's own, applied to this projection.
  const railSteps: StepRow[] = useMemo(() => {
    const mine = stepRows
      .map(stepFromRow)
      .filter((step) => step.key !== "" && idTail(step.runId) === idTail(openRunId));
    const moment = at === "" ? NOW : at;
    const shown =
      moment === NOW
        ? mine
        : mine
            .filter((step) => existedAt(step.createdAt, moment))
            .map((step) => ({ ...step, status: stepStatusAt(step, moment) }));
    return stepsInOrder(shown);
  }, [stepRows, openRunId, at]);
  const breakdown = useMemo(() => kindBreakdown(railSteps), [railSteps]);

  const run = world.run;
  const openStep = railSteps.find((step) => step.key === selectedStepKey) ?? null;
  // THE THING THIS PAGE MOST NEEDS TO OFFER. A run parked on a person does not
  // move until somebody answers it, and a goal view that drew the pause without
  // offering the answer would be a picture of being stuck.
  const parked = world.approvals.find((approval) => approval.decision === "") ?? null;
  const waitingOnYou = run !== null && run.status === "waiting" && parked !== null;

  // A run this window cannot draw is a selection the person did not make.
  // Reset the step when the run changes rather than carrying a key that names
  // a step in a different run.
  useEffect(() => { setSelectedStepKey(""); revealSelection.current = false; }, [openRunId]);
  useEffect(() => { setConfirm(false); returnCloseFocus.current = false; }, [goal.id]);
  useEffect(() => {
    if (!confirm && returnCloseFocus.current) {
      returnCloseFocus.current = false;
      page.current?.querySelector<HTMLButtonElement>('button[aria-label="Close goal"]')?.focus();
    }
  }, [confirm]);

  const selectStep = (key: string) => {
    revealSelection.current = key !== "";
    setSelectedStepKey(key);
  };
  useLayoutEffect(() => {
    if (!revealSelection.current) return;
    revealSelection.current = false;
    const scroller = body.current;
    const heading = inspector.current?.querySelector<HTMLElement>(".os-subhead");
    if (!scroller || !heading) return;
    // Reveal the chosen heading inside this page only. Keep keyboard focus on
    // the action, and leave scrolling alone when live evidence changes.
    const view = scroller.getBoundingClientRect();
    const target = heading.getBoundingClientRect();
    if (target.top < view.top) scroller.scrollTop += target.top - view.top;
    else if (target.bottom > view.bottom) scroller.scrollTop += target.bottom - view.bottom;
  }, [selectedStepKey]);

  const keepOpen = () => {
    if (cancel.busy) return;
    returnCloseFocus.current = true;
    setConfirm(false);
  };

  // The scene library's run row, not the app's, so the shared TERMINAL list is
  // read directly rather than through `runIsTerminal` -- a cast between two
  // row types to reuse a one-line predicate is a cast that outlives the reason
  // for it.
  const terminal = run !== null && RUN_TERMINAL.includes(run.status);
  const rewound = at !== "";

  // -------------------------------------------------------------------------
  // The acts, and an illegal one is ABSENT rather than disabled (rule 12)
  // -------------------------------------------------------------------------
  const acts: Act[] = [];
  if (waitingOnYou && parked !== null) {
    acts.push({
      label: "Answer it",
      tone: "primary",
      ariaLabel: `Answer the ${parked.kind === "" ? "approval" : parked.kind} this run is parked on`,
      onAct: () => onOpenApproval(parked.id),
    });
  }
  if (run !== null && terminal) {
    // BRANCH, NOT FORK (epic memql#5414). `forkRun` re-executed every step
    // from the first; `branchRun` reuses everything before the step by
    // reference and takes what the person changes, through the one composer
    // the run page opens for the same act.
    if (openStep !== null) {
      const key = openStep.key;
      acts.push({
        label: "Branch from here",
        icon: <GitBranch size={13} aria-hidden />,
        ariaLabel: `Branch from ${key}: a new run that reuses the steps before it and runs from here, with whatever you change`,
        onAct: () => setComposer({ mode: "branch", stepKey: key }),
      });
    }
    acts.push({
      label: "Replay",
      icon: <RotateCcw size={13} aria-hidden />,
      busy: derive.busy,
      ariaLabel:
        "Replay this run as a NEW run: every model call is served from the journal, so no provider is reached",
      onAct: () => {
        void derive.replay(run.id).then((id) => {
          if (id !== "") onOpenRun(id);
        });
      },
    });
  }
  // CLOSING THE GOAL LIVES HERE AND NOWHERE ELSE. It asks every run of the
  // goal to stop, so it belongs on the page about the goal rather than on a
  // page about one of its runs -- the shape the Deployables recomposition
  // removed. It is absent once the goal is closed, not disabled.
  const closable = goal.status !== "closed";
  if (closable) {
    if (confirm) {
      // Only the explicit question's actions remain. The shared bar holds
      // presses after the question appears, including keyboard presses.
      acts.splice(0, acts.length,
        { label: "Keep open", text: true, busy: cancel.busy, onAct: keepOpen },
        { label: "Close goal", tone: "danger", busy: cancel.busy,
          ariaLabel: "Confirm closing this goal and ask all its runs to stop",
          onAct: () => { void cancel.cancel(goal.id, "Closed from MemQL OS").then(ok => {
            if (ok) setConfirm(false);
          }); } },
      );
    } else {
      acts.push({ label: "Close goal", ariaLabel: "Close goal", tone: "danger", onAct: () => setConfirm(true) });
    }
  }

  // The APP's projection of the drawn run, for what the scene library does not
  // carry: its head, and whether a step's child run is an app session.
  const appRuns = useMemo(() => runRows.map(runFromRow), [runRows]);
  const appRun = run === null ? null : (appRuns.find((candidate) => idTail(candidate.id) === idTail(run.id)) ?? null);
  const usageSummary = usage ? summarizeUsage(usage.calls, usage.complete) : null;
  const tokenUsage = appRun ? runSpend(appRun).find(figure => figure.as === "tokens")?.value ?? null : null;
  const actionsUnavailable = railSteps.length === 0 && stepsState !== "live";
  const emptySummary = actionsUnavailable
    ? stepsState === "seeding" ? "Loading recorded actions." : "The action history is unavailable. Reconnect to read this attempt."
    : run === null ? "" : runWithoutStepsSummary({
    status: run.status, errorCode: appRun?.errorCode, errorMessage: run.errorMessage, waitingOnKind: appRun?.waitingOnKind,
  });
  const emptyJourneyLabel = actionsUnavailable
    ? stepsState === "seeding" ? "Loading recorded actions" : "Action history unavailable"
    : run?.status === "compiling" || run?.status === "running"
    ? "The work is taking shape" : run?.status === "waiting" ? "Waiting before the first action" : "No actions recorded";
  const currentStep = railSteps.find(step => ["waiting", "running", "failed"].includes(step.status)) ?? railSteps.at(-1);
  const composerStep = composer === null ? null : (railSteps.find((step) => step.key === composer.stepKey) ?? null);
  const composerSession =
    composerStep !== null &&
    composerStep.childRunId !== "" &&
    appRuns.find((candidate) => idTail(candidate.id) === idTail(composerStep.childRunId))?.automationName === "appSession";

  const tone: ActionBarTone =
    run === null
      ? "none"
      : run.status === "running" || run.status === "compiling"
        ? "busy"
        : run.status === "waiting"
          ? "paused"
          : run.status === "succeeded"
            ? "live"
            : "none";

  return (
    <div className="os-nexus-goalview" ref={page} onKeyDown={event => {
      if (event.key !== "Escape" || event.defaultPrevented || !confirm) return;
      event.preventDefault();
      event.stopPropagation();
      keepOpen();
    }}>
      {/* THE KIT'S HEAD, which publishes the trail to the window's one trail
          row rather than drawing a back button of its own (DESIGN.md, "One
          trail row"). */}
      <Head
        title={goalTitle(goal)}
        navigationKey={goal.id}
        meta={<Chip tone={goal.status === "closed" ? "muted" : "accent"}>{goal.status === "active" ? "Active goal" : goalStatusWord(goal.status)}</Chip>}
        back={{ label: "Goals", onSelect: onBack }}
      />

      <div className="os-nexus-goalview-body" ref={body}>
        {goal.statement.trim() !== "" && goalTitle(goal) !== goal.statement.trim() ? (
          <details className="nexus-technical"><summary>Request</summary><p style={{ whiteSpace: "pre-wrap" }}>{goal.statement}</p></details>
        ) : null}
        <Facts>
          <Fact label="Asked by" value={originWord(goal.origin)} />
          <Fact
            label="Set"
            value={goal.createdAt === "" ? "" : formatFreshness(goal.createdAt, now)}
            title={goal.createdAt === "" ? undefined : formatMoment(goal.createdAt)}
          />
          <Fact label="Attempts in recent activity" value={world.runs.length === 0 ? "Not loaded" : String(world.runs.length)} />
          {goal.closeReason === "" ? null : <Fact label="Closed because" value={goal.closeReason} />}
        </Facts>

        {world.runs.length > 1 ? (
          <div className="os-nexus-runpicker" role="radiogroup" aria-label="Which run to draw">
            <span className="os-caption">
              This goal has been attempted more than once. The map draws one attempt.
            </span>
            <Chips>
              {world.runs.map((candidate) => (
                <button
                  key={candidate.id}
                  type="button"
                  role="radio"
                  aria-checked={idTail(candidate.id) === idTail(openRunId)}
                  className="os-choice"
                  onClick={() => onPickRun(candidate.id)}
                >
                  {runModeWord(candidate.mode)} · {runStatusWord(candidate.status)}
                </button>
              ))}
            </Chips>
          </div>
        ) : null}

        {run === null ? (
          <Caption>
            {world.runs.length === 0
              ? "No attempt is available in the loaded activity."
              : "Pick a run to draw."}
          </Caption>
        ) : (
          <>
            <section className="nexus-now" aria-label="Current progress"><div><p className="nexus-now-title">{waitingOnYou ? "Waiting for your decision" : currentStep ? stepTitle(currentStep) : runStatusWord(run.status)}</p><Caption>{waitingOnYou ? parked?.question || "Review the request to continue." : currentStep ? stepOutcome(currentStep) : emptySummary}</Caption></div><span className="nexus-usage" title={usageSummary?.total !== null && usageSummary?.total !== undefined ? `${usageSummary.total.toLocaleString()} tokens${usageSummary.estimated ? " (includes estimates)" : " reported"}` : "Token usage has not been reported"}>{usageSummary && usageSummary.calls > 0 ? usageLabel(usageSummary) : tokenUsage === null ? "Usage not reported" : `${formatTokens(tokenUsage)} tokens reported`}</span></section>
            <div className="nexus-workspace"><div className="nexus-workspace-spine">
              <GoalJourney key={run.id} steps={railSteps} selected={selectedStepKey} onSelect={selectStep} completed={progress.completed} total={progress.total} emptyLabel={emptyJourneyLabel} stale={stepsState === "degraded" || stepsState === "disconnected"} />
              {railSteps.length === 0 ? (stepsState === "seeding" ? <RecordListSkeleton label="Loading the steps from the cluster" /> : stepsState === "live" ? <Caption>{progress.compiling ? "The steps appear as the work is planned." : "This run recorded no steps."}</Caption> : null) : null}
            </div>{openStep ? <aside className="nexus-workspace-inspector" aria-live="polite" ref={inspector}><StepSummary step={openStep} usage={usage ? summarizeUsage(usage.calls.filter(call => call.stepKey === openStep.key), usage.complete) : undefined} /></aside> : null}</div>

            {/* Rewind is read-only view navigation beside its time control.
                Lifecycle actions stay together in the three-action footer. */}
            {bounds.count > 1 && !rewound ? <button type="button" className="os-nexus-link"
              aria-label="Rewind: draw this goal as it stood at an earlier moment. Nothing runs and nothing is spent."
              onClick={() => setAt(bounds.to || "")}><History size={13} aria-hidden /> Rewind</button> : null}
            {rewound ? (
              <Scrubber moments={moments} at={at} onScrub={setAt} onLive={() => setAt("")} />
            ) : null}

            <details className="nexus-technical"><summary>Usage and execution details</summary>
              {card === null ? null : <ReceiptCard card={card} usage={usage} stepsKnown={!actionsUnavailable} />}
              <KindBand breakdown={breakdown} runStatus={run.status} stepsKnown={!actionsUnavailable} />
            </details>

            <details className="nexus-technical"><summary>Dependencies and execution map</summary>{actionsUnavailable ? <Caption>The execution map is unavailable until recorded actions are read.</Caption> : <BeaconMap
              world={world} state={stepsState} selectedStepKey={selectedStepKey} onSelectStep={selectStep}
              onOpenApproval={onOpenApproval} expandedColumns={expandedColumns} expandedFolds={expandedFolds}
              onToggleColumn={depth => setExpandedColumns(held => toggled(held, depth))}
              onToggleFold={depth => setExpandedFolds(held => toggled(held, depth))} at={at} />}
            </details>

            <button type="button" className="os-nexus-link" onClick={() => onOpenRun(run.id)}>
              Activity and advanced controls
            </button>
          </>
        )}

        {cancel.error === "" ? null : (
          <Notice tone="error" sentence="Closing the goal needs attention." detail={cancel.error} />
        )}
      </div>

      <ActionBar
        state={run === null ? goalStatusWord(goal.status) : runStatusWord(run.status)}
        detail={
          rewound
            ? "Reviewing saved history; no work is being started"
            : run === null
              ? "No attempt is available yet"
              : waitingOnYou
                ? // The reason, in the classifier's own words where it gave
                  // one: "waiting" alone does not say whether somebody is
                  // needed or a timer is.
                  parked?.reason !== undefined && parked.reason !== ""
                    ? parked.reason
                    : "Waiting for your decision before continuing"
                : terminal
                  ? railSteps.length === 0
                    ? actionsUnavailable ? emptyJourneyLabel : "No recorded actions to review"
                    : openStep === null
                    ? "Select an action to review its result"
                    : ""
                  : railSteps.length === 0 ? emptyJourneyLabel : "Follow the actions above to see what happens next"
        }
        tone={tone}
        acts={acts}
        scope={goal.id}
      >
        {closable && confirm ? <span>Close this goal and ask all its runs to stop? A step already running may finish first.</span> : null}
        {derive.error === "" ? null : (
          <span className="os-nexus-act-error os-mono" role="alert">
            {derive.error}
          </span>
        )}
      </ActionBar>

      <ComposerHost
        request={composer}
        runId={run?.id ?? ""}
        stepOrder={run?.stepOrder ?? []}
        head={appRun?.head ?? {}}
        timelineKeys={railSteps.map((step) => step.key)}
        step={composerStep}
        versions={[]}
        current={composerStep?.version ?? null}
        session={composerSession}
        ownLevel={composerStep?.override.level ?? ""}
        passedOn={null}
        drafts={drafts}
        onDrafts={setDrafts}
        onClose={() => setComposer(null)}
        onRerun={() => setComposer(null)}
        onBranched={(reply) => {
          setComposer(null);
          onOpenRun(reply.runId);
        }}
      />
    </div>
  );
}

/**
 * The scrubber: the run's own moments, in order.
 *
 * TICKS ARE EVENTS, NOT A CONTINUOUS AXIS. A slider over wall-clock time would
 * put most of its travel in the gaps between steps -- a run that waited nine
 * hours on a person would be nine hours of nothing and four seconds of work.
 * Stepping through the moments puts every position on something that happened,
 * which is also what makes the arrow keys useful.
 */
function Scrubber({
  moments,
  at,
  onScrub,
  onLive,
}: {
  moments: readonly SceneEvent[];
  at: string;
  onScrub: (at: string) => void;
  onLive: () => void;
}) {
  // The LAST moment at or before `at`. Written as a reverse walk rather than
  // `findLastIndex`, which the OS's TypeScript target does not carry.
  const index = lastIndexAtOrBefore(moments, at);
  const current = moments[index];

  return (
    <div className="os-nexus-scrubber">
      <input
        className="os-nexus-scrubber-track"
        type="range"
        min={0}
        max={Math.max(0, moments.length - 1)}
        value={index}
        aria-label="Scrub through what happened"
        aria-valuetext={current === undefined ? "" : `${current.label}, ${formatMoment(current.at)}`}
        onChange={(e) => {
          const next = moments[Number(e.target.value)];
          if (next !== undefined) onScrub(next.at);
        }}
      />
      <div className="os-nexus-scrubber-read">
        <span className="os-nexus-scrubber-label">{current?.label ?? "Nothing dated"}</span>
        <span className="os-nexus-scrubber-when os-mono">
          {current === undefined ? "" : formatMoment(current.at)}
        </span>
        <button type="button" className="os-nexus-link" onClick={onLive}>
          Back to now
        </button>
      </div>
    </div>
  );
}

/**
 * The receipt.
 *
 * A FAILED RUN GETS THE SAME CARD. Not a consolation screen and not silence:
 * the same readings, plus the failure and the step that was running when it
 * stopped. Hiding the cost because the outcome was bad is the version of this
 * that flatters instead of informing.
 */
function ReceiptCard({ card, usage, stepsKnown }: { card: NonNullable<ReturnType<typeof receipt>>; usage?: ModelUsageState; stepsKnown: boolean }) {
  return (
    <Panel label="What it cost">
      <ul className="os-nexus-spend" aria-label="What this run cost">
        <Reading value={card.elapsedMs < 0 ? "--" : formatElapsed(card.elapsedMs)} label="took" />
        <Reading value={stepsKnown ? String(card.steps) : "Not available"} label={card.steps === 1 ? "step" : "steps"} />
        {usage ? null : <Reading value={stepsKnown ? String(card.thought) : "Not available"} label="steps with model reasoning" />}
        {usage ? null : <Reading value={card.tokens === null ? "Not reported" : formatTokens(card.tokens)} label="tokens" />}
        <Reading value={formatMoney(card.cost)} label="cost" />
        {usage ? null : <Reading
          value={card.modelCalls === null ? "--" : String(card.modelCalls)}
          label="model calls"
        />}
      </ul>
      {usage ? <UsageDetails usage={usage} /> : null}
      {card.failure === "" ? null : (
        <Notice
          tone="error"
          sentence="It failed."
          next={
            card.lastRunningStep === ""
              ? stepsKnown ? "No action was recorded as running when it stopped." : "The action history is unavailable."
              : `${card.lastRunningStep} was running when it stopped.`
          }
          detail={card.failure}
        />
      )}
      {card.cancelledBy === "" ? null : (
        <Caption>Cancelled by {card.cancelledBy}.</Caption>
      )}
      {!usage && card.subscriptionTokens === null ? (
        <Caption>
          This run's engine reported no spend, so these figures are unmeasured rather than zero.
        </Caption>
      ) : card.subscriptionTokens !== null && card.subscriptionTokens > 0 ? (
        <Caption>
          {formatTokens(card.subscriptionTokens)} of those tokens were covered by a subscription and
          did not count against the dollar ceiling.
        </Caption>
      ) : null}
    </Panel>
  );
}

function Reading({ value, label }: { value: string; label: string }) {
  return (
    <li className="os-nexus-spend-item">
      <span className="os-nexus-spend-value os-mono">{value}</span>
      <span className="os-nexus-spend-label">{label}</span>
    </li>
  );
}

/** The last index whose moment is at or before `at`; 0 when none is. */
function lastIndexAtOrBefore(moments: readonly SceneEvent[], at: string): number {
  if (moments.length === 0) return 0;
  for (let i = moments.length - 1; i >= 0; i -= 1) {
    if ((moments[i] as SceneEvent).at <= at) return i;
  }
  return 0;
}

/**
 * A set with one member flipped.
 *
 * ALWAYS APPLIED THROUGH AN UPDATER, never as `setX(toggled(x, n))`. Two
 * expanders opened against ONE render both read the same set from that
 * render's closure, so the second write lands over the first and one of them
 * silently does nothing. Every test in this suite clicked once per render,
 * which is exactly why a suite is green over it -- the failing case is two
 * clicks inside one `act`, which is what the test beside this pins.
 *
 * (The same defect was found independently in `apps/files/Rail.tsx` by the
 * files-places session, on the same shape of state.)
 */
function toggled(set: ReadonlySet<number>, value: number): ReadonlySet<number> {
  const next = new Set(set);
  if (next.has(value)) next.delete(value);
  else next.add(value);
  return next;
}
