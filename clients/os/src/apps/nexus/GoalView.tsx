import { useEffect, useMemo, useState } from "react";
import { ArrowLeft, GitFork, History, RotateCcw } from "lucide-react";
import type { LiveState, Row } from "@znasllc-io/memql-sdk-core/client";

import {
  Caption,
  Chip,
  Chips,
  Fact,
  Facts,
  Input,
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
import { StepSpineRow } from "./StepSpine";
import type { CancelGoalState, DeriveRunState } from "./actions";
import {
  formatMoney,
  formatTokens,
  RUN_TERMINAL,
  idTail,
  kindBreakdown,
  stepFromRow,
  stepsInOrder,
  type GoalRow,
  type StepRow,
} from "./rows";
import { buildWorld } from "./world";
import { goalStatusWord, originWord, runModeWord, runStatusWord } from "./words";

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
  goal: GoalRow;
  /** The app root's raw feeds. The map narrows them itself; see `world.ts`. */
  goalRow: Row | null;
  runRows: readonly Row[];
  approvalRows: readonly Row[];
  stepRows: readonly Row[];
  stepsState: LiveState;
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
  goalRow,
  runRows,
  approvalRows,
  stepRows,
  stepsState,
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
  const [confirm, setConfirm] = useState("");

  const live = useMemo(
    () => buildWorld({ goalRow, runRows, stepRows, approvalRows, openRunId }),
    [goalRow, runRows, stepRows, approvalRows, openRunId],
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
  useEffect(() => setSelectedStepKey(""), [openRunId]);

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
  // MORE THAN ONE MOMENT, not more than zero. A goal with no runs still dates
  // its own creation, so `count > 0` offered Rewind on a goal there is nothing
  // to scrub through -- an act that redraws the picture it is already showing.
  // An illegal act is ABSENT (rule 12), and "does nothing" is illegal.
  if (bounds.count > 1) {
    acts.push({
      label: rewound ? "Back to now" : "Rewind",
      icon: <History size={13} aria-hidden />,
      ariaLabel: rewound
        ? "Leave rewind and draw the goal as it is now"
        : "Rewind: draw this goal as it stood at an earlier moment. Nothing runs and nothing is spent.",
      onAct: () => setAt(rewound ? "" : (bounds.to || "")),
    });
  }
  if (run !== null && terminal) {
    if (openStep !== null) {
      acts.push({
        label: `Fork from ${openStep.key}`,
        icon: <GitFork size={13} aria-hidden />,
        busy: derive.busy,
        ariaLabel: `Fork this run at step ${openStep.key}: the steps before it come from the journal, and this step onward runs live`,
        onAct: () => {
          void derive.fork(run.id, openStep.key).then((id) => {
            if (id !== "") onOpenRun(id);
          });
        },
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
  const confirmed = confirm.trim() === goal.statement.trim();
  if (closable) {
    acts.push({
      label: "Close this goal",
      tone: "danger",
      busy: cancel.busy,
      ariaLabel:
        "Close this goal and ask every run of it to stop. A run notices at its next step boundary.",
      onAct: () => {
        if (!confirmed) return;
        void cancel.cancel(goal.id, "Closed from MemQL OS").then((ok) => {
          if (ok) setConfirm("");
        });
      },
    });
  }

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
    <div className="os-nexus-goalview">
      <div className="os-head">
        <button type="button" className="os-nexus-back" onClick={onBack}>
          <ArrowLeft size={13} aria-hidden />
          Goals
        </button>
        <h3 className="os-settings-title">{goal.statement}</h3>
        <span className="os-head-meta">
          <Chip tone={goal.status === "closed" ? "muted" : "accent"}>
            {goalStatusWord(goal.status)}
          </Chip>
        </span>
      </div>

      <div className="os-nexus-goalview-body">
        <Facts>
          <Fact label="Asked by" value={originWord(goal.origin)} />
          <Fact
            label="Set"
            value={goal.createdAt === "" ? "" : formatFreshness(goal.createdAt, now)}
            title={goal.createdAt === "" ? undefined : formatMoment(goal.createdAt)}
          />
          <Fact label="Runs" value={world.runs.length === 0 ? "none yet" : String(world.runs.length)} />
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
              ? "No run has been opened for this goal yet."
              : "Pick a run to draw."}
          </Caption>
        ) : (
          <>
            <BeaconMap
              world={world}
              state={stepsState}
              selectedStepKey={selectedStepKey}
              onSelectStep={setSelectedStepKey}
              onOpenApproval={onOpenApproval}
              expandedColumns={expandedColumns}
              expandedFolds={expandedFolds}
              onToggleColumn={(depth) => setExpandedColumns((held) => toggled(held, depth))}
              onToggleFold={(depth) => setExpandedFolds((held) => toggled(held, depth))}
              at={at}
            />

            {rewound ? (
              <Scrubber moments={moments} at={at} onScrub={setAt} onLive={() => setAt("")} />
            ) : null}

            {card === null ? null : <ReceiptCard card={card} />}

            <Panel label="What this run is made of">
              <KindBand breakdown={breakdown} />
            </Panel>

            <section className="os-nexus-timeline" aria-label="What this run did, in order">
              <ol className="os-nexus-steps">
                {railSteps.map((step, index) => (
                  <li key={step.id || step.key} className="os-nexus-step-item">
                    <StepSpineRow
                      step={step}
                      position={index + 1}
                      last={index === railSteps.length - 1}
                      open={step.key === selectedStepKey}
                      onOpen={() =>
                        setSelectedStepKey(step.key === selectedStepKey ? "" : step.key)
                      }
                    />
                  </li>
                ))}
              </ol>
              {railSteps.length === 0 ? (
                <Caption>
                  {stepsState === "seeding"
                    ? "Loading the steps from the cluster"
                    : stepsState === "disconnected"
                      ? "Not connected to the cluster"
                      : progress.compiling
                        ? "It is still working out what to do. The steps appear as it decides them."
                        : "This run recorded no steps."}
                </Caption>
              ) : null}
            </section>

            <button type="button" className="os-nexus-link" onClick={() => onOpenRun(run.id)}>
              Open this run in full
            </button>
          </>
        )}

        {cancel.error === "" ? null : (
          <Notice tone="error" sentence="The goal was not closed." detail={cancel.error} />
        )}
      </div>

      <ActionBar
        state={run === null ? goalStatusWord(goal.status) : runStatusWord(run.status)}
        detail={
          rewound
            ? "rewound -- nothing here runs, and nothing is spent"
            : run === null
              ? "no run to act on yet"
              : waitingOnYou
                ? // The reason, in the classifier's own words where it gave
                  // one: "waiting" alone does not say whether somebody is
                  // needed or a timer is.
                  parked?.reason !== undefined && parked.reason !== ""
                    ? `parked on you: ${parked.reason}`
                    : "parked on you -- it does not move until you answer"
                : terminal
                  ? openStep === null
                    ? "select a step to fork from there"
                    : ""
                  : "replay and fork wait until the run finishes"
        }
        tone={tone}
        acts={acts}
      >
        {/* THE CONFIRMATION IS THE GOAL'S OWN WORDS. Closing asks every run of
            it to stop, so this is the one place on this page that asks somebody
            to type something -- and what it asks for is the statement, because
            that is what they would call it. */}
        {closable ? (
          <span className="os-nexus-confirm">
            <Input
              id="nexus-close-confirm"
              label={`Type the goal's own words to close it: ${goal.statement}`}
              placeholder="Type the goal to close it"
              value={confirm}
              onChange={setConfirm}
            />
          </span>
        ) : null}
        {derive.error === "" ? null : (
          <span className="os-nexus-act-error os-mono" role="alert">
            {derive.error}
          </span>
        )}
      </ActionBar>
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
      <Caption>
        Every tick is a moment a row recorded. A moment nothing dated has no tick -- this is read
        out of the rows, not out of a recording.
      </Caption>
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
function ReceiptCard({ card }: { card: NonNullable<ReturnType<typeof receipt>> }) {
  return (
    <Panel label="What it cost">
      <ul className="os-nexus-spend" aria-label="What this run cost">
        <Reading value={card.elapsedMs < 0 ? "--" : formatElapsed(card.elapsedMs)} label="took" />
        <Reading value={String(card.steps)} label={card.steps === 1 ? "step" : "steps"} />
        <Reading value={String(card.thought)} label="thought" />
        <Reading value={formatTokens(card.tokens)} label="tokens" />
        <Reading value={formatMoney(card.cost)} label="cost" />
        <Reading
          value={card.modelCalls === null ? "--" : String(card.modelCalls)}
          label="model calls"
        />
      </ul>
      {card.failure === "" ? null : (
        <Notice
          tone="error"
          sentence="It failed."
          next={
            card.lastRunningStep === ""
              ? "Nothing was running when it stopped."
              : `${card.lastRunningStep} was running when it stopped.`
          }
          detail={card.failure}
        />
      )}
      {card.cancelledBy === "" ? null : (
        <Caption>Cancelled by {card.cancelledBy}.</Caption>
      )}
      {card.subscriptionTokens === null ? (
        <Caption>
          This run's engine reported no spend, so these figures are unmeasured rather than zero.
        </Caption>
      ) : card.subscriptionTokens > 0 ? (
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
