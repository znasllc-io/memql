import { useMemo, useState } from "react";
import { ArrowLeft, GitFork, RotateCcw } from "lucide-react";

import {
  Caption,
  Chip,
  Chips,
  Fact,
  Facts,
  Notice,
  Panel,
  formatDuration,
  formatFreshness,
  formatMoment,
  useNow,
} from "../../kit";
import { ActionBar, type Act, type ActionBarTone } from "../../kit/ActionBar";
import { JournalPanel } from "./Journal";
import { KindBand } from "./KindBand";
import { StepSpineRow } from "./StepSpine";
import type { DeriveRunState } from "./actions";
import {
  decisionsByStep,
  formatSpend,
  kindBreakdown,
  runIsTerminal,
  runSpend,
  runTitle,
  spendLabel,
  runWaitsOnYou,
  type ApprovalRow,
  type GoalRow,
  type RunRow,
  type StepRow,
} from "./rows";
import {
  runModeDetail,
  runModeWord,
  runStatusDetail,
  runStatusWord,
  stepKindMeaning,
  stepKindWord,
  stepStatusWord,
  symptomMeaning,
  symptomWord,
  waitingWord,
} from "./words";
import type { Journal } from "./useNexus";

// ONE RUN, TOP TO BOTTOM.
//
// ===========================================================================
// IT REPLACES THE LIST RATHER THAN SITTING UNDER IT (DESIGN.md rule 11)
// ===========================================================================
// A run timeline is tall -- a real one is dozens of steps plus a journal --
// so this is the `<- Runs` form of the rule rather than the beside-the-list
// form. Two Heads in one scroller is the tell that neither happened, and the
// Deployables app measured what that costs: 5,069px over 5.9 viewports.
//
// ===========================================================================
// THE ORDER OF THE PAGE IS THE ORDER OF THE QUESTIONS
// ===========================================================================
// What is this and how did it go (the head) -> how much of it had to think
// (the band) -> what happened, in order (the spine) -> what the model was
// actually asked (the journal, on demand). Somebody who came to check one
// fact finds it before scrolling; somebody debugging keeps reading.
//
// ===========================================================================
// THE ACTS ARE ON ONE BAR AND AN ILLEGAL ONE IS ABSENT (rule 12)
// ===========================================================================
// Replay and Fork are offered on a TERMINAL run only, and that is a real rule
// rather than caution: a replay serves every model call from the journal, so
// replaying a run that is still writing that journal is a run that will miss
// and raise a divergence at whatever step it happened to reach. Offering it
// would be offering a failure.
//
// THERE IS NO CANCEL ON THIS PAGE, AND THE BAR SAYS WHY. The verb is
// `cancelGoal`, which closes the goal and asks EVERY run of it to stop -- so
// a Cancel button here would destroy this run's siblings from a page about
// this one, which is the exact shape the Deployables recomposition removed
// (a cascade that archived every app, sitting on a page about one of them).
// It lives on the goal, where its blast radius is the thing you are looking
// at.

export interface RunPageProps {
  run: RunRow;
  goal: GoalRow | null;
  steps: readonly StepRow[];
  stepsState: string;
  approvals: readonly ApprovalRow[];
  journal: Journal;
  derive: DeriveRunState;
  onBack: () => void;
  onOpenGoal: (goalId: string) => void;
  onOpenApprovals: (approvalId: string) => void;
  onOpenRun: (runId: string) => void;
}

export function RunPage({
  run,
  goal,
  steps,
  stepsState,
  approvals,
  journal,
  derive,
  onBack,
  onOpenGoal,
  onOpenApprovals,
  onOpenRun,
}: RunPageProps) {
  const now = useNow(15_000);
  const [openStepKey, setOpenStepKey] = useState("");

  const breakdown = useMemo(() => kindBreakdown(steps), [steps]);
  const spend = useMemo(() => runSpend(run), [run]);
  // WHICH DOOR ANSWERED, PER STEP -- from the journal this page already holds,
  // so the timeline costs no extra read. The journal is read on demand
  // (`useJournal` does not read on open, and a test pins that), so these lines
  // appear when somebody asks for it and the map is empty until then. That is
  // the honest state: before the read, this window does not know which door
  // answered, and a row cannot say what it has not been told.
  const decisions = useMemo(() => decisionsByStep(journal.modelCalls), [journal.modelCalls]);
  const openStep = steps.find((step) => step.key === openStepKey) ?? null;

  const terminal = runIsTerminal(run);
  const waiting = runWaitsOnYou(run);
  const pending = approvals[0] ?? null;

  const tone: ActionBarTone =
    run.status === "running" || run.status === "compiling"
      ? "busy"
      : run.status === "waiting"
        ? "paused"
        : run.status === "succeeded"
          ? "live"
          : "none";

  const acts: Act[] = [];
  if (waiting && pending !== null) {
    acts.push({
      label: "Answer it",
      tone: "primary",
      onAct: () => onOpenApprovals(pending.id),
    });
  }
  if (terminal) {
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
      tone: "primary",
      icon: <RotateCcw size={13} aria-hidden />,
      busy: derive.busy,
      ariaLabel: "Replay this run: every model call is served from the journal, so no provider is reached",
      onAct: () => {
        void derive.replay(run.id).then((id) => {
          if (id !== "") onOpenRun(id);
        });
      },
    });
  }

  const barDetail = waiting
    ? waitingWord(run.waitingOnKind).toLowerCase()
    : terminal
      ? runStatusDetail(run.status) ||
        (openStep === null ? "select a step to fork from there" : "")
      : // A non-terminal run offers no Replay and no Fork, so the bar says
        // what IS true rather than leaving two absent controls unaccounted
        // for -- an absent control with no account of itself reads as
        // something nobody got round to building.
        runStatusDetail(run.status) || "replay and fork wait until it finishes";

  return (
    <div className="os-nexus-run">
      <div className="os-head">
        <button type="button" className="os-nexus-back" onClick={onBack}>
          <ArrowLeft size={13} aria-hidden />
          Runs
        </button>
        <h3 className="os-settings-title">{runTitle(run)}</h3>
        <span className="os-head-meta">
          <Chip tone={run.mode === "replay" ? "accent" : "muted"} title={runModeDetail(run.mode)}>
            {runModeWord(run.mode)}
          </Chip>
        </span>
      </div>

      <div className="os-nexus-run-body">
        {/* WHAT THIS RUN IS FOR, FIRST. A run's own name is an automation
            name; the reason it exists is the goal's statement, and that is
            what somebody arriving from a notification needs. A run with no
            goal says so rather than leaving a blank -- most runs in epic A1
            have none, being ordinary automation executions. */}
        <p className="os-nexus-run-lede">
          {goal === null ? (
            run.goalId === "" ? (
              <>No goal asked for this run -- it is an automation execution.</>
            ) : (
              <>
                For a goal this window cannot read (
                <span className="os-mono">{run.goalId}</span>).
              </>
            )
          ) : (
            <>
              For{" "}
              <button type="button" className="os-nexus-link" onClick={() => onOpenGoal(goal.id)}>
                {goal.statement}
              </button>
            </>
          )}
        </p>

        {/* A DIVERGED REPLAY IS NOT A FAILED RUN, and it took its own notice
            to stop reading as one (memql#4999). Nothing broke: the compile
            ran, the model seam found no journaled answer for a request, and
            it refused to substitute a live call -- which is the whole of the
            strict policy. The run even names where the two parted. Under the
            generic failure notice it read as "this run failed" with a
            sentence about a classifier that never saw it, and the reader went
            looking for a fault. */}
        {run.status === "failed" && run.errorCode === "replay_diverged" ? (
          <Notice
            tone="warn"
            sentence="This replay diverged."
            next="A request had no match in the journal, so it stopped rather than call a model and report a reproduction that did not happen. The step it parted at is below."
            detail={run.errorMessage}
          />
        ) : run.status === "failed" && run.errorCode === "automation_not_runnable" ? (
          /* THE MACHINE IS FINE AND THE READER MUST NOT GO LOOKING AT IT
             (memql#5054). Compile chose a template by name and the node that
             picked the run up cannot resolve that name -- a bundle that did
             not ship it, or a node type that does not build it in. Under the
             generic notice this said "this run failed" and pointed at a step,
             and there is no step: nothing executed. Retrying resolves the
             same nothing, so the notice says so rather than implying a
             retry. */
          <Notice
            tone="error"
            sentence="Nothing here could run this."
            next="It was compiled to a template no node in this cluster can resolve, so no step ran and retrying finds the same nothing. The name is below -- it wants a deploy, not another attempt."
            detail={run.errorMessage}
          />
        ) : run.status === "failed" && run.errorCode === "run_refused" ? (
          /* REFUSED IS NOT BROKEN (memql#5054). The automation's own gate
             turned this down on its arguments -- the executor reported a skip
             -- so the answer is in what was asked for, not in the run. Kept
             out of the error tone for that reason: an error tone sends people
             to the logs. */
          <Notice
            tone="warn"
            sentence="This run was turned down before it started."
            next="Its own gate refused the arguments it was given, so no step ran and nothing was changed. The reason is below; it is about what was asked for."
            detail={run.errorMessage}
          />
        ) : run.status === "failed" && run.errorCode === "composition_failed" ? (
          /* THE DOCUMENT DOES NOT EXIST, AND THAT IS THE HEADLINE. The
             Materializer wrote its own terminal record and then reported the
             failure, so by the time this run closed there was a `failed`
             composition and no file. This used to park at `waiting` on a
             retry instead -- the error said "deadline exceeded", the symptom
             table called it a blip, and a person watched a spinner over work
             the database had already given up on. The notice names the
             absent file and says why another attempt is not the answer. */
          <Notice
            tone="error"
            sentence="The document was not made."
            next="The composition recorded its own failure, so there is no file and no partial one. Another attempt reads the same failed record -- start it again from the goal instead. The reason is below."
            detail={run.errorMessage}
          />
        ) : run.status === "failed" && run.errorCode === "self_timeout" ? (
          /* A LIMIT WE CHOSE, SAID AS OURS. Nothing on the far side timed
             out: a deadline this system set for itself expired, and the work
             was not given longer. Saying so keeps the reader out of the
             network logs, and keeps the operator's own number visible as the
             thing to change. A goal carries no duration ceiling, so seeing
             this at all means somebody set one deliberately. */
          <Notice
            tone="error"
            sentence="This stopped on a deadline this system set for itself."
            next="Nothing on the far side failed and nothing was overrunning -- the work simply was not given longer. Goals carry no time limit of their own, so this is a configured one. Another attempt gets the same deadline."
            detail={run.errorMessage}
          />
        ) : run.status === "failed" && run.errorMessage !== "" ? (
          <Notice
            tone="error"
            sentence="This run failed."
            next="The step it stopped at is marked below, with what the classifier made of it."
            detail={run.errorMessage}
          />
        ) : null}

        {/* WHAT `abandoned` MEANS CHANGED, SO THIS SENTENCE HAD TO
            (memql#5054). The sweep now offers a silent run to another replica
            before closing it, so reaching this state means the offer was made
            and not taken -- worth saying, because it is the difference between
            "try again" and "look at why nothing took it".

            The old copy also promised more than the system does. "Nothing was
            left half-done" is not true of a step that was in flight when the
            node went away: the journal holds it at `running` with no receipt,
            which is exactly what makes it findable. And "a resume picks up
            from it" named an action with no button behind it. */}
        {run.status === "abandoned" ? (
          <Notice
            tone="warn"
            sentence="The node running this went away."
            next="Another replica was offered it before it was closed, and did not take it. Every step that finished is in the journal below; the one still marked running is where it stopped."
          />
        ) : null}

        {/* THE CAPTION MUST NOT OUTLIVE ITS TRUTH (memql#4999). It used to
            print unconditionally, so a diverged replay carried the notice
            above -- "this replay diverged" -- and, two lines below it,
            "Every model call was served from the journal, so this run reached
            no provider". Both sentences on one page, and only one of them
            true of this run. On a divergence the notice has already said what
            happened; the caption's job here is only to name the policy that
            decided it. */}
        {run.mode === "replay" && run.errorCode !== "replay_diverged" ? (
          <Caption>
            A replay. Every model call was served from the journal, so this run reached no provider
            -- under the {run.replayPolicy || "strict"} policy, a request with no journaled match
            {run.replayPolicy === "permissive"
              ? " is made fresh and journaled."
              : " raises a divergence at the first step that differs."}
          </Caption>
        ) : run.mode === "replay" ? (
          <Caption>
            A replay under the {run.replayPolicy || "strict"} policy. Everything before the step
            above was served from the journal; nothing reached a provider.
          </Caption>
        ) : null}
        {run.mode === "fork" && run.forkAtStepKey !== "" ? (
          <Caption>
            Forked from{" "}
            <button
              type="button"
              className="os-nexus-link"
              onClick={() => onOpenRun(run.forkedFromRunId)}
            >
              its source run
            </button>{" "}
            at <span className="os-mono">{run.forkAtStepKey}</span>. Everything before that step
            came from the journal.
          </Caption>
        ) : null}

        <Panel label="What this run is made of">
          <KindBand breakdown={breakdown} />
          {/* THE SPEND IS THE BAND'S LEGEND LINE, not four stat cards beside
              it: it is the same question the band asks, answered in figures.
              Every value can be ABSENT and renders as an em dash -- epic A1
              writes none of them, and "0 model calls" on a run that made three
              is the single most damaging thing this surface could say. */}
          <ul className="os-nexus-spend" aria-label="What this run spent">
            {spend.map((figure) => (
              <li key={figure.many} className="os-nexus-spend-item">
                <span className="os-nexus-spend-value os-mono">{formatSpend(figure)}</span>
                <span className="os-nexus-spend-label">{spendLabel(figure)}</span>
              </li>
            ))}
            <li className="os-nexus-spend-item">
              <span className="os-nexus-spend-value os-mono">
                {run.startedAt === "" ? "--" : formatFreshness(run.startedAt, now)}
              </span>
              <span className="os-nexus-spend-label">started</span>
            </li>
          </ul>
          {breakdown.unclassified > 0 ? (
            <Caption>
              {breakdown.unclassified} of these steps{" "}
              {breakdown.unclassified === 1 ? "is" : "are"} unclassified: this build cannot yet say
              whether they called a model, so they are counted separately rather than assumed free.
            </Caption>
          ) : null}
        </Panel>

        <section className="os-nexus-timeline" aria-label="What this run did, in order">
          <ol className="os-nexus-steps">
            {steps.map((step, index) => (
              <li key={step.id || step.key} className="os-nexus-step-item">
                <StepSpineRow
                  step={step}
                  position={index + 1}
                  last={index === steps.length - 1}
                  open={step.key === openStepKey}
                  onOpen={() => setOpenStepKey(step.key === openStepKey ? "" : step.key)}
                  decision={decisions.get(step.key) ?? null}
                />
                {step.key === openStepKey ? <StepDetail step={step} onOpenRun={onOpenRun} /> : null}
              </li>
            ))}
          </ol>
          {steps.length === 0 ? (
            <Caption>
              {stepsState === "seeding"
                ? "Loading the steps from the cluster"
                : stepsState === "disconnected"
                  ? "Not connected to the cluster"
                  : run.status === "compiling"
                    ? "It is still working out what to do. The steps appear as it decides them."
                    : "This run recorded no steps."}
            </Caption>
          ) : null}
        </section>

        <JournalPanel journal={journal} />
      </div>

      <ActionBar
        state={runStatusWord(run.status)}
        detail={barDetail}
        tone={tone}
        acts={acts}
      >
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
 * One step, opened.
 *
 * A DISCLOSURE UNDER THE ROW, NOT A SECOND PANEL. Rule 11 is about a list and
 * a DETAIL PAGE sharing a scroller -- the tell is two Heads. This carries no
 * Head and no acts of its own; it is the row saying more about itself, which
 * is the same shape the Deployables rail's open stop has.
 */
function StepDetail({
  step,
  onOpenRun,
}: {
  step: StepRow;
  onOpenRun: (runId: string) => void;
}) {
  return (
    <div className="os-nexus-step-detail">
      <p className="os-nexus-step-meaning">
        <strong>{stepKindWord(step.kind)}</strong> -- {stepKindMeaning(step.kind)}
      </p>
      <Facts>
        <Fact label="Step key" value={step.key} mono />
        <Fact label="Type" value={step.stepType} mono />
        <Fact label="Calls" value={step.callName} mono />
        <Fact label="Status" value={stepStatusWord(step.status)} />
        <Fact label="Attempt" value={step.attempt} />
        <Fact
          label="Duration"
          value={step.durationMs === null ? "" : formatDuration(step.durationMs)}
          mono
        />
        <Fact
          label="Started"
          value={step.startedAt === "" ? "" : formatMoment(step.startedAt)}
          title={step.startedAt}
        />
        <Fact
          label="Finished"
          value={step.finishedAt === "" ? "" : formatMoment(step.finishedAt)}
          title={step.finishedAt}
        />
        {/* A POSTCONDITION HAS THREE ANSWERS AND THE THIRD IS NOT "false".
            Absent means this step declares none, which epic A1 leaves true of
            every step; rendering that as "did not pass" would mark every run
            in the cluster failed on the strength of a field nobody wrote. */}
        <Fact
          label="Postcondition"
          value={
            step.postconditionPassed === null
              ? "none declared"
              : step.postconditionPassed
                ? `passed (${step.postconditionKind || "check"})`
                : `did not hold -- ${step.postconditionMessage || "no message"}`
          }
        />
        {/* THE KEY A SIDE EFFECT RAN UNDER. On a resume the executor asks
            the far side whether it already holds a receipt for this exact
            key, which is what turns "never retry a mutation" into "retried
            when idempotent by key, parked otherwise". */}
        <Fact label="Idempotency key" value={step.idempotencyKey} mono />
      </Facts>

      {step.dependsOn.length === 0 ? null : (
        <Chips label="Steps this one waited for">
          {step.dependsOn.map((key) => (
            <Chip key={key} tone="muted">
              {key}
            </Chip>
          ))}
        </Chips>
      )}

      {step.symptom === "" ? null : (
        <Notice
          tone="warn"
          sentence={symptomWord(step.symptom)}
          next={symptomMeaning(step.symptom)}
          detail={step.errorMessage || undefined}
        />
      )}

      {step.childRunId === "" ? null : (
        <p className="os-caption">
          It opened{" "}
          <button type="button" className="os-nexus-link" onClick={() => onOpenRun(step.childRunId)}>
            a run of its own
          </button>{" "}
          and waited for it.
        </p>
      )}
    </div>
  );
}
