import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { useEffect, useMemo, useState, type ReactNode } from "react";
import { GitBranch, Pin, Redo2, RotateCcw } from "lucide-react";

import { AttentionMarker } from "../../attention/Attention";
import { useSession } from "../../chrome/access";
import {
  Button,
  Caption,
  Chip,
  Head,
  Notice,
  Panel,
  formatFreshness,
  useNow,
} from "../../kit";
import { ActionBar, type Act, type ActionBarTone } from "../../kit/ActionBar";
import { JournalPanel } from "./Journal";
import { KindBand } from "./KindBand";
import { StepSpineRow } from "./StepSpine";
import { StepDetail, STEP_VERSIONS_TARGET } from "./StepDetail";
import { ComposerHost, type ComposerRequest } from "./ComposerHost";
import { ValidatorLine, VerdictBelow, VerdictChoices, type DislikeDraft, type VerdictProps } from "./Verdict";
import { NEXUS_APP_ID } from "./concepts";
import { useMoveRunHead, useRecordFeedback, type DeriveRunState, type RerunReply } from "./actions";
import {
  RUN_TARGET,
  disagrees,
  newestVerdict,
  targetKey,
  validatorFor,
  type Axes,
  type FeedbackTarget,
} from "./feedback";
import {
  decisionsByStep,
  formatSpend,
  idTail,
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
  type ValidationSummary,
} from "./rows";
import { useRunVerdicts, useStepVersions } from "./useInterventions";
import {
  currentVersionOf,
  versionCount,
  versionsByKey,
  versionsOf,
  versionsSignature,
  type ComposerDraft,
  type StepVersion,
} from "./versions";
import {
  kindCalledAModel,
  rerunInFlightWords,
  runModeDetail,
  runModeWord,
  runStatusDetail,
  runStatusWord,
  waitingWord,
  type Verdict,
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
// The heading is the kit's `Head`, which PUBLISHES the trail to the window's
// one trail row rather than drawing a back button of its own (DESIGN.md, "One
// trail row"). The trail is where the run sits -- Runs, then this run -- and
// Back is where the person came from, which for a branch or a replay opened
// from here is the run it came from.
//
// ===========================================================================
// THE ORDER OF THE PAGE IS THE ORDER OF THE QUESTIONS
// ===========================================================================
// What is this and how did it go (the head) -> how much of it had to think
// (the band) -> what did I think of it (the verdict) -> what happened, in
// order (the spine) -> what the model was actually asked (the journal, on
// demand). Somebody who came to check one fact finds it before scrolling;
// somebody debugging keeps reading.
//
// ===========================================================================
// THE ACTS ARE ON ONE BAR, THEY FOLLOW THE SELECTION, AND AN ILLEGAL ONE IS
// ABSENT (rule 12)
// ===========================================================================
// With no step selected the bar offers the RUN's acts: Answer it while it is
// parked on you, Replay once it has finished. Select a step of a finished run
// and it offers that step's: Make current (only for a version that is not
// current), Branch from here, and Run again, primary last -- three, which is
// the cap. While a re-run is in flight no step act is offered, and the bar
// says in words what is running.
//
// Replay and the step acts wait for a TERMINAL run, and that is a real rule
// rather than caution: a replay serves every model call from the journal, and
// a re-run of a run still writing is refused (`run_not_finished`) because it
// would race the live execution.
//
// THERE IS NO CANCEL ON THIS PAGE, AND THE BAR SAYS WHY. The verb is
// `cancelGoal`, which closes the goal and asks EVERY run of it to stop -- so
// a Cancel button here would destroy this run's siblings from a page about
// this one, which is the exact shape the Deployables recomposition removed
// (a cascade that archived every app, sitting on a page about one of them).
// It lives on the goal, where its blast radius is the thing you are looking
// at.

/**
 * What a run page remembers while somebody is away from it -- to a branch it
 * opened, and Back again. Held by the app root per run, so returning lands
 * the person on the step and version they had open, with every draft intact.
 */
export interface RunPageMemory {
  openStepKey: string;
  selectedVersions: Readonly<Record<string, number>>;
  composerDrafts: Readonly<Record<string, ComposerDraft>>;
  dislikeDrafts: Readonly<Record<string, DislikeDraft>>;
}

export const EMPTY_RUN_MEMORY: RunPageMemory = {
  openStepKey: "",
  selectedVersions: {},
  composerDrafts: {},
  dislikeDrafts: {},
};

export interface RunPageProps {
  run: RunRow;
  goal: GoalRow | null;
  steps: readonly StepRow[];
  stepsState: string;
  approvals: readonly ApprovalRow[];
  journal: Journal;
  derive: DeriveRunState;
  /** Every run this person owns: a step's child run says whether an app session answered it. */
  runs?: readonly RunRow[];
  /** Where Back goes, by name: the run this one was opened from, or the list. */
  backLabel?: string;
  onBack: () => void;
  /** The trail's own "Runs" crumb, which always goes to the list. */
  onBackToList?: () => void;
  onOpenGoal: (goalId: string) => void;
  onOpenApprovals: (approvalId: string) => void;
  /** Open another run FROM this one, so Back returns here. */
  onOpenRun: (runId: string) => void;
  memory?: RunPageMemory;
  onRemember?: (memory: RunPageMemory) => void;
}

export function RunPage({
  run,
  goal,
  steps,
  stepsState,
  approvals,
  journal,
  derive,
  runs = [],
  backLabel = "Runs",
  onBack,
  onBackToList,
  onOpenGoal,
  onOpenApprovals,
  onOpenRun,
  memory = EMPTY_RUN_MEMORY,
  onRemember,
}: RunPageProps) {
  const now = useNow(15_000);
  const { access } = useSession();
  const viewerId = access?.userId ?? "";

  const [openStepKey, setOpenStepKey] = useState(memory.openStepKey);
  const [selectedVersions, setSelectedVersions] = useState(memory.selectedVersions);
  const [composerDrafts, setComposerDrafts] = useState(memory.composerDrafts);
  const [dislikeDrafts, setDislikeDrafts] = useState(memory.dislikeDrafts);
  const [composer, setComposer] = useState<ComposerRequest | null>(null);
  const [lastRerun, setLastRerun] = useState<RerunReply | null>(null);

  // Whatever the page holds is handed back to the app root as it changes, so
  // a person who opens a branch from here and comes Back finds this page as
  // they left it.
  useEffect(() => {
    onRemember?.({ openStepKey, selectedVersions, composerDrafts, dislikeDrafts });
  }, [openStepKey, selectedVersions, composerDrafts, dislikeDrafts]);

  const head = useMoveRunHead();
  const feedback = useRecordFeedback();

  const breakdown = useMemo(() => kindBreakdown(steps), [steps]);
  const spend = useMemo(() => runSpend(run), [run]);
  // WHICH DOOR ANSWERED, PER STEP -- from the journal this page already holds,
  // so the timeline costs no extra read. The journal is read on demand
  // (`useJournal` does not read on open, and a test pins that), so these lines
  // appear when somebody asks for it and the map is empty until then. That is
  // the honest state: before the read, this window does not know which door
  // answered, and a row cannot say what it has not been told.
  const decisions = useMemo(() => decisionsByStep(journal.modelCalls), [journal.modelCalls]);

  // THE VERSIONS, read on open and again when a new one appears or the head
  // moves -- never on a status flip, which the steps feed already carries.
  const versionsRead = useStepVersions(run.id, versionsSignature(run, steps));
  const versionIndex = useMemo(() => versionsByKey(versionsRead.versions), [versionsRead.versions]);

  // THE VERDICTS, read on open and again when the run finishes or the
  // validator speaks: both change what the verdicts beside the steps say.
  const verdicts = useRunVerdicts(run.id, `${run.status}|${run.validation?.observationId ?? ""}`);
  const validatorEntry = useMemo(() => validatorFor(verdicts.validator, run.validation), [verdicts.validator, run.validation]);
  const validation: ValidationSummary | null =
    run.validation ??
    (validatorEntry === null
      ? null
      : {
          verdict: validatorEntry.verdict,
          stepKey: validatorEntry.target.stepKey,
          version: validatorEntry.target.version,
          observationId: validatorEntry.id,
          level: validatorEntry.level,
          at: validatorEntry.createdAt,
        });

  const terminal = runIsTerminal(run);
  const waiting = runWaitsOnYou(run);
  const pending = approvals[0] ?? null;
  // The step acts are legal on a finished run with no re-run of its own in
  // flight. A re-run turns the run back to `running`, so this is mostly the
  // terminal test -- but a request left on a closed run still hides them.
  const stepActsLegal = terminal && run.rerun === null;
  // A run that has finished once can be judged -- including while a re-run of
  // one of its steps is in flight, when hiding the verdicts would move the
  // whole timeline up under the person watching it.
  const judgeable = terminal || run.rerun !== null;

  const timelineKeys = useMemo(() => steps.map((step) => step.key), [steps]);

  // WHERE THE UNSEEN-CHANGE MARKER GOES. Every row opens the version area, but
  // a dot on each of forty rows is a strobe, not a pointer: the marker sits on
  // the steps a model or an app answered -- where running again with another
  // intelligence and saying what was wrong matter most -- and on every row
  // only when a run has none, so the change is always reachable.
  const modelledStep = (step: StepRow) => kindCalledAModel(step.kind) !== false || step.childRunId !== "";
  const anyModelled = steps.some(modelledStep);

  function stepVersions(step: StepRow): {
    versions: StepVersion[];
    current: number | null;
    count: number;
  } {
    const versions = versionsOf(versionIndex, step, step.key);
    const current = currentVersionOf(run, versions, step.key);
    return { versions, current, count: versionCount(versions, run.head, step, step.key) };
  }

  function isSession(step: StepRow | StepVersion | null): boolean {
    if (step === null || step.childRunId === "") return false;
    const child = runs.find((candidate) => idTail(candidate.id) === idTail(step.childRunId));
    return child?.automationName === "appSession";
  }

  const openStep = steps.find((step) => step.key === openStepKey) ?? null;
  const open = openStep === null ? null : stepVersions(openStep);
  const selected =
    openStep === null || open === null
      ? null
      : (selectedVersions[openStep.key] ?? open.current ?? openStep.version);
  const selectedVersion =
    open === null || selected === null
      ? null
      : (open.versions.find((version) => version.version === selected) ?? null);

  // A refusal belongs to the version it was about: picking another step or
  // another version clears it rather than leaving it under acts it is not
  // about.
  useEffect(() => {
    head.reset();
  }, [openStepKey, selected]);

  function toggleStep(key: string): void {
    setOpenStepKey((held) => (held === key ? "" : key));
  }

  function openComposer(mode: "rerun" | "branch", stepKey: string, seed?: string): void {
    setComposer({ mode, stepKey, ...(seed === undefined ? {} : { seed }) });
  }

  // -------------------------------------------------------------------------
  // Verdicts
  // -------------------------------------------------------------------------

  function recordVerdict(target: FeedbackTarget, verdict: Verdict, axes?: Axes, reason?: string): void {
    const key = targetKey(target);
    void feedback
      .record(key, {
        runId: run.id,
        ...(target.stepKey === "" ? {} : { stepKey: target.stepKey }),
        ...(target.version === null ? {} : { version: target.version }),
        verdict,
        ...(verdict === "dislike" && axes !== undefined
          ? {
              ...(axes.product ? { product: true } : {}),
              ...(axes.process ? { process: true } : {}),
              ...(axes.performance ? { performance: true } : {}),
            }
          : {}),
        ...(reason !== undefined && reason !== "" ? { reason } : {}),
      })
      .then((reply) => {
        if (reply === null) return;
        verdicts.add({
          id: reply.observationId,
          verdict,
          axes: verdict === "dislike" && axes !== undefined ? axes : { product: false, process: false, performance: false },
          reason: reason ?? "",
          target,
          validatorDisagrees: reply.validatorDisagrees,
          createdAt: new Date().toISOString(),
        });
        setDislikeDrafts((held) => {
          if (!(key in held)) return held;
          const next = { ...held };
          delete next[key];
          return next;
        });
      });
  }

  /** The props one target's verdict shares between its two halves. */
  function verdictProps(target: FeedbackTarget, scope: "step" | "run", label: string): VerdictProps {
    const key = targetKey(target);
    return {
      id: `nexus-verdict-${key.replace(/[^A-Za-z0-9_-]/g, "-")}`,
      label,
      scope,
      saved: newestVerdict(verdicts.feedback, target),
      state: verdicts.state,
      readError: scope === "run" && verdicts.state === "error" ? verdicts.error : "",
      draft: dislikeDrafts[key] ?? null,
      onDraft: (next) =>
        setDislikeDrafts((held) => {
          const copy = { ...held };
          if (next === null) delete copy[key];
          else copy[key] = next;
          return copy;
        }),
      busy: feedback.recording === key,
      error: feedback.errors[key] ?? "",
      onRecord: (verdict, axes, reason) => recordVerdict(target, verdict, axes, reason),
    };
  }

  function verdictFor(target: FeedbackTarget, scope: "step" | "run", label: string, validatorLine: ReactNode) {
    const props = verdictProps(target, scope, label);
    return {
      choices: <VerdictChoices {...props} />,
      below: <VerdictBelow {...props}>{validatorLine}</VerdictBelow>,
    };
  }

  /** The validator's line for a target, when the validator judged it. */
  function validatorLineFor(target: FeedbackTarget, nameTheStep: boolean) {
    if (validation === null) return null;
    const judgedHere =
      target.stepKey === "" || (target.stepKey === validation.stepKey && target.version === validation.version);
    if (!judgedHere) return null;
    const saved = newestVerdict(verdicts.feedback, target);
    const disagreesWithYou = saved !== null && (saved.validatorDisagrees || disagrees(saved.verdict, validation.verdict));
    const flaggedStep = steps.find((step) => step.key === validation.stepKey) ?? null;
    return (
      <ValidatorLine
        verdict={validation.verdict}
        entry={validatorEntry}
        stepKey={nameTheStep ? validation.stepKey : ""}
        disagreesWithYou={disagreesWithYou}
        onRunAgain={
          stepActsLegal && flaggedStep !== null
            ? () => openComposer("rerun", validation.stepKey, validatorEntry?.reason.trim() || undefined)
            : undefined
        }
      />
    );
  }

  // -------------------------------------------------------------------------
  // The bar
  // -------------------------------------------------------------------------

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
  if (stepActsLegal && openStep !== null && open !== null && selected !== null) {
    const key = openStep.key;
    // MAKE CURRENT is offered for a finished version that is not the current
    // one -- going back, or forward again after going back. A version still
    // running or one that failed is never made current.
    if (selected !== open.current && selectedVersion !== null && selectedVersion.status === "done") {
      acts.push({
        label: "Make current",
        icon: <Pin size={13} aria-hidden />,
        busy: head.busy,
        ariaLabel: `Make version ${selected} of ${key} current. The steps after it that were made from it come back without running; any others run again, and anything they change outside the run is done again.`,
        onAct: () => void head.act({ runId: run.id, stepKey: key, version: selected }),
      });
    }
    acts.push({
      label: "Branch from here",
      icon: <GitBranch size={13} aria-hidden />,
      ariaLabel: `Branch from ${key}: a new run that reuses the steps before it and runs from here, with whatever you change`,
      onAct: () => openComposer("branch", key),
    });
    acts.push({
      label: "Run again",
      tone: "primary",
      icon: <Redo2 size={13} aria-hidden />,
      ariaLabel: `Run ${key} again as a new version, with whatever you change. Every earlier version is kept.`,
      onAct: () => openComposer("rerun", key),
    });
  } else if (stepActsLegal) {
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

  const rerunVersion = (() => {
    if (run.rerun === null) return null;
    if (lastRerun !== null && lastRerun.stepKey === run.rerun.stepKey && lastRerun.version !== null) return lastRerun.version;
    const target = steps.find((step) => step.key === run.rerun?.stepKey);
    return target !== undefined && target.status === "running" ? target.version : null;
  })();

  const barDetail =
    run.rerun !== null
      ? rerunInFlightWords(run.rerun.reason, run.rerun.stepKey, rerunVersion)
      : waiting
        ? waitingWord(run.waitingOnKind).toLowerCase()
        : terminal && openStep !== null && open !== null && selected !== null
          ? selectionWords(openStep.key, selected, Math.max(open.count, selected), open.current)
          : terminal
            ? runStatusDetail(run.status) || "select a step to run it again or branch from it"
            : // A non-terminal run offers no Replay and no step acts, so the
              // bar says what IS true rather than leaving absent controls
              // unaccounted for.
              runStatusDetail(run.status) || "replay and branching wait until it finishes";

  const composerStep = composer === null ? null : (steps.find((step) => step.key === composer.stepKey) ?? null);
  const composerVersions = composerStep === null ? null : stepVersions(composerStep);
  const composerCurrent =
    composerVersions === null || composerVersions.current === null
      ? null
      : (composerVersions.versions.find((version) => version.version === composerVersions.current) ?? null);
  const composerDislike =
    composer === null || composerVersions === null
      ? null
      : newestVerdict(verdicts.feedback, { stepKey: composer.stepKey, version: composerVersions.current });

  const title = runTitle(run);

  return (
    <div className="os-nexus-run">
      <Head
        title={title}
        meta={
          <Chip tone={run.mode === "replay" ? "accent" : "muted"} title={runModeDetail(run.mode)}>
            {runModeWord(run.mode)}
          </Chip>
        }
        breadcrumbs={[{ label: "Runs", onSelect: onBackToList ?? onBack }, { label: title }]}
        back={{ label: backLabel, onSelect: onBack }}
      />

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
            Branched from{" "}
            <button
              type="button"
              className="os-nexus-link"
              onClick={() => onOpenRun(run.forkedFromRunId)}
            >
              the run it came from
            </button>{" "}
            at <span className="os-mono">{run.forkAtStepKey}</span>. The steps before it were reused
            from that run, not run again.
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

        {/* THE RUN'S OWN VERDICT, where the summary is -- and the validator's
            beside it, naming the step it checked, because this is where a
            person looks first and a flagged answer is the one fact on the page
            that might change what they do next. */}
        {judgeable ? (
          <Panel label="Your verdict on this run">
            {(() => {
              const verdict = verdictFor(RUN_TARGET, "run", "Your verdict on this run", validatorLineFor(RUN_TARGET, true));
              return (
                <>
                  {verdict.choices}
                  {verdict.below}
                </>
              );
            })()}
          </Panel>
        ) : null}

        <section className="os-nexus-timeline" aria-label="What this run did, in order">
          <ol className="os-nexus-steps">
            {steps.map((step, index) => {
              const info = stepVersions(step);
              const isOpen = step.key === openStepKey;
              const picked = isOpen && selected !== null ? selected : (info.current ?? step.version);
              // A version just asked for can be picked before its first row
              // arrives, and "version 4 of 3" is a sentence nobody should read.
              const known = Math.max(info.count, picked);
              const pickedRow = isOpen ? selectedVersion : null;
              // A version can be judged once it has finished -- done or failed
              // -- and only when a model or an app took part in it: the
              // framework's three questions are about work an AI did, and a
              // query that ran a filter has no approach to dislike.
              const finished = pickedRow !== null && (pickedRow.status === "done" || pickedRow.status === "failed");
              const judgeableStep = step.kind !== "human" && (kindCalledAModel(step.kind) !== false || step.childRunId !== "");
              const target: FeedbackTarget = { stepKey: step.key, version: picked };
              return (
                <li key={step.id || step.key} className="os-nexus-step-item">
                  <StepSpineRow
                    step={step}
                    position={index + 1}
                    last={index === steps.length - 1}
                    open={isOpen}
                    onOpen={() => toggleStep(step.key)}
                    decision={decisions.get(step.key) ?? null}
                    versions={{ count: info.count, current: info.current }}
                    stale={run.staleSteps.includes(step.key)}
                    marker={
                      terminal && (!anyModelled || modelledStep(step)) ? (
                        <AttentionMarker appId={NEXUS_APP_ID} sectionId="runs" target={STEP_VERSIONS_TARGET} />
                      ) : null
                    }
                  />
                  {isOpen ? (
                    <StepDetail
                      step={step}
                      versions={info.versions}
                      versionsLoading={versionsRead.state === "loading" && info.count > 1}
                      current={info.current}
                      count={known}
                      selected={picked}
                      onSelect={(version) => setSelectedVersions((held) => ({ ...held, [step.key]: version }))}
                      selectedVersion={pickedRow}
                      session={isSession(pickedRow ?? step)}
                      viewerId={viewerId}
                      reachable={terminal}
                      continues={index < steps.length - 1}
                      onOpenRun={onOpenRun}
                      verdict={
                        judgeable && finished && judgeableStep
                          ? verdictFor(target, "step", "Your verdict", validatorLineFor(target, false))
                          : null
                      }
                    />
                  ) : null}
                </li>
              );
            })}
          </ol>
          {steps.length === 0 ? (stepsState === "seeding" ? <RecordListSkeleton label="Loading the steps from the cluster" /> : <Caption>{stepsState === "disconnected"
                  ? "Not connected to the cluster"
                  : run.status === "compiling"
                    ? "It is still working out what to do. The steps appear as it decides them."
                    : "This run recorded no steps."}</Caption>) : null}
          {/* ONCE, UNDER THE TIMELINE, not in every step: a read that failed
              is one fact about the page, and repeating it in each disclosure
              would say it forty times. The timeline still draws what the live
              feed holds -- each step's current version. */}
          {versionsRead.state === "error" && steps.length > 0 ? (
            <Notice
              tone="warn"
              sentence="Earlier versions of these steps could not be read."
              next="Each step shows the version the run uses now."
              detail={versionsRead.error}
            >
              <Button onClick={versionsRead.retry}>Try again</Button>
            </Notice>
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
        {head.error === "" ? null : (
          <span className="os-nexus-act-error os-mono" role="alert">
            {head.error}
          </span>
        )}
      </ActionBar>

      <ComposerHost
        request={composer}
        runId={run.id}
        stepOrder={run.stepOrder}
        head={run.head}
        timelineKeys={timelineKeys}
        step={composerStep}
        versions={composerVersions?.versions ?? []}
        current={composerVersions?.current ?? null}
        currentVersion={composerCurrent}
        session={isSession(composerCurrent ?? composerStep)}
        ownLevel={ownLevelOf(composerVersions?.versions ?? [])}
        passedOn={composerDislike !== null && composerDislike.verdict === "dislike" ? composerDislike : null}
        drafts={composerDrafts}
        onDrafts={setComposerDrafts}
        onClose={() => setComposer(null)}
        onRerun={(reply) => {
          setComposer(null);
          setLastRerun(reply);
          // THE NEW VERSION IS WHAT THEY ASKED TO SEE: select it, so the step
          // they re-ran shows the version running rather than the one it
          // replaces.
          if (reply.version !== null) {
            const version = reply.version;
            setSelectedVersions((held) => ({ ...held, [reply.stepKey]: version }));
          }
          setOpenStepKey(reply.stepKey);
        }}
        onBranched={(reply) => {
          setComposer(null);
          onOpenRun(reply.runId);
        }}
      />
    </div>
  );
}

/** The bar's words for a selected step: "draft, version 2 of 3 -- version 3 is current". */
function selectionWords(key: string, selected: number, count: number, current: number | null): string {
  if (count <= 1) return key;
  const base = `${key}, version ${selected} of ${count}`;
  return current !== null && current !== selected ? `${base} -- version ${current} is current` : base;
}

/**
 * The step's OWN level, as recorded serving a version nobody asked a level
 * for. Never an override's: an override applies to one version and never
 * carries to the next, so a re-run that leaves the level alone runs at the
 * step's own -- and naming the last version's override here would promise the
 * wrong one. "" when no version says.
 */
function ownLevelOf(versions: readonly StepVersion[]): string {
  for (let i = versions.length - 1; i >= 0; i -= 1) {
    const version = versions[i] as StepVersion;
    if (version.override.level !== "") continue;
    const level = version.binding?.["level"];
    if (typeof level === "string" && level !== "") return level;
  }
  return "";
}
