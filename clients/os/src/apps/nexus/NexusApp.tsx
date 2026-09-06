import { useEffect, useMemo, useRef, useState } from "react";
import { Caption, Check, Head, Panel } from "../../kit";
import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { ApprovalsSection } from "./ApprovalsSection";
import { GoalsSection } from "./GoalsSection";
import { RunPage } from "./RunPage";
import { RunsSection } from "./RunsSection";
import { NEXUS_APP_ID, NEXUS_LOG_CONCEPTS } from "./concepts";
import {
  useCancelGoal,
  useCreateGoal,
  useDecideApproval,
  useDeriveRun,
} from "./actions";
import {
  approvalFromRow,
  goalFromRow,
  idTail,
  pendingApprovalsOfRun,
  runFromRow,
  stepFromRow,
  stepsInOrder,
  type ApprovalRow,
  type GoalRow,
  type RunRow,
} from "./rows";
import {
  DEFAULT_NEXUS_SETTINGS,
  LocalNexusSettingsStore,
  NEXUS_SECTIONS,
  type NexusSettings,
  type NexusSettingsStore,
} from "./settings";
import { useApprovals, useGoals, useJournal, useRunSteps, useRuns } from "./useNexus";

// WORK: what you asked the system to do, what it did about it, and the places
// it had to stop and ask you.
//
// ===========================================================================
// THREE FEEDS AT THE ROOT, ONE PER CONCEPT; STEPS BELONG TO THE OPEN RUN
// ===========================================================================
// Goals, runs and approvals are retained here and passed down, so the Goals
// surface and the Runs surface cannot disagree about what the cluster holds.
// The rule is per CONCEPT rather than per app, so three feeds is not three
// copies of one thing.
//
// The steps feed is deliberately NOT here. A per-run timeline retained by the
// app root would subscribe a window to every step of every run this person
// owns in order to draw one of them -- which is exactly the rule the
// Deployables app wrote down about deployment timelines. It is held by
// `RunView`, which exists only while a run is open, so the subscription's life
// is the page's life and nothing else's.
//
// ===========================================================================
// THE SELECTION IS THE APP'S, NOT A SECTION'S
// ===========================================================================
// Following a goal to its run and a run to its approval crosses sections, and
// a selection held inside a section would be lost on the way. So the three ids
// live here and each section is told what is open -- which also means going to
// Approvals to answer something and coming back lands somebody exactly where
// they were.

/** The concepts this app owns, for its Logs section's subject scope. */
const LOG_CONCEPTS = NEXUS_LOG_CONCEPTS;

export function NexusApp({
  sectionId,
  navigate,
  askContext,
  intent,
  consumeIntent,
  store,
}: OsAppProps & { store?: NexusSettingsStore }) {
  // Injectable for tests, which is the whole reason the parameter exists --
  // nothing in the shell passes one.
  const settingsStore = useMemo(() => store ?? new LocalNexusSettingsStore(), [store]);
  const [settings, setSettings] = useState<NexusSettings>(() => settingsStore.load());

  const goals = useGoals();
  const runs = useRuns();
  const approvals = useApprovals();

  const create = useCreateGoal();
  const cancel = useCancelGoal();
  const derive = useDeriveRun();
  const decide = useDecideApproval();

  const [selectedGoalId, setSelectedGoalId] = useState("");
  const [openRunId, setOpenRunId] = useState("");
  const [selectedApprovalId, setSelectedApprovalId] = useState("");

  // The two feeds every surface reads as PLAIN ROWS rather than as a live
  // source: the goal a run is for, and the approvals a run is parked on. They
  // are joins over feeds already retained, not second reads.
  const goalRows: GoalRow[] = useMemo(
    () => goals.snapshot.rows.map(goalFromRow).filter((goal) => goal.id !== ""),
    [goals.snapshot],
  );
  const runRows: RunRow[] = useMemo(
    () => runs.snapshot.rows.map(runFromRow).filter((run) => run.id !== ""),
    [runs.snapshot],
  );
  const approvalRows: ApprovalRow[] = useMemo(
    () => approvals.snapshot.rows.map(approvalFromRow).filter((a) => a.id !== ""),
    [approvals.snapshot],
  );
  const goalsById = useMemo(() => {
    const byId = new Map<string, GoalRow>();
    for (const goal of goalRows) byId.set(idTail(goal.id), goal);
    return byId;
  }, [goalRows]);

  function openRun(runId: string) {
    if (runId.trim() === "") return;
    setOpenRunId(runId);
    askContext(`nexus run:${idTail(runId)}`);
    navigate("runs");
  }

  function openGoal(goalId: string) {
    if (goalId.trim() === "") return;
    setSelectedGoalId(goalId);
    navigate("goals");
  }

  function openApproval(approvalId: string) {
    if (approvalId.trim() === "") return;
    setSelectedApprovalId(approvalId);
    navigate("approvals");
  }

  // A STANDING OPEN INSTRUCTION, id-matched on consumption so acting on a
  // stale render can never eat a newer one. The payload names ONE of the three
  // things this app can open; anything else is left alone rather than guessed
  // at, which is what keeps an unrelated opener from moving somebody's window.
  const handled = useRef("");
  useEffect(() => {
    if (intent === undefined || intent.id === handled.current) return;
    const payload = intent.payload;
    const runId = typeof payload["runId"] === "string" ? payload["runId"] : "";
    const goalId = typeof payload["goalId"] === "string" ? payload["goalId"] : "";
    const approvalId = typeof payload["approvalId"] === "string" ? payload["approvalId"] : "";
    if (runId === "" && goalId === "" && approvalId === "") return;
    handled.current = intent.id;
    if (runId !== "") openRun(runId);
    else if (approvalId !== "") openApproval(approvalId);
    else openGoal(goalId);
    consumeIntent?.(intent.id);
  }, [intent]);

  function update(patch: Partial<NexusSettings>) {
    const next = { ...settings, ...patch, version: 1 as const };
    setSettings(next);
    settingsStore.save(next);
  }

  // THE DEFAULT-SECTION PREFERENCE, APPLIED ONCE PER WINDOW -- the pattern
  // every app since Fleet uses. The shell opens an app on its manifest's FIRST
  // section, so an app-level "open me here" can only be the app navigating
  // itself on first render. It applies ONLY when the window opened on the
  // shell's default: a window opened on a named section was opened by somebody
  // who said where they wanted to be.
  const applied = useRef(false);
  useEffect(() => {
    if (applied.current) return;
    applied.current = true;
    const shellDefault = NEXUS_SECTIONS[0]?.id ?? "";
    if (sectionId !== shellDefault) return;
    if (settings.defaultSection && settings.defaultSection !== sectionId) {
      navigate(settings.defaultSection);
    }
    // ONCE PER MOUNT, WHICH IS ONCE PER WINDOW.
  }, []);

  if (sectionId === "settings") {
    return <NexusSettingsSection settings={settings} update={update} />;
  }
  if (sectionId === "logs") {
    return (
      <AppLogsSection
        app={NEXUS_APP_ID}
        subjectConcepts={LOG_CONCEPTS}
        intent={intent}
        consumeIntent={consumeIntent}
      />
    );
  }
  if (sectionId === "approvals") {
    return (
      <ApprovalsSection
        approvals={approvals.source}
        runs={runRows}
        decide={decide}
        selectedApprovalId={selectedApprovalId}
        onSelectApproval={setSelectedApprovalId}
        onOpenRun={openRun}
      />
    );
  }
  if (sectionId === "runs") {
    const open = runRows.find((run) => idTail(run.id) === idTail(openRunId)) ?? null;
    if (openRunId !== "" && open !== null) {
      return (
        <RunView
          run={open}
          goal={goalsById.get(idTail(open.goalId)) ?? null}
          approvals={pendingApprovalsOfRun(approvalRows, open.id)}
          derive={derive}
          onBack={() => setOpenRunId("")}
          onOpenGoal={openGoal}
          onOpenApprovals={openApproval}
          onOpenRun={openRun}
        />
      );
    }
    return (
      <RunsSection
        runs={runs.source}
        goalsById={goalsById}
        showFinished={settings.showFinishedRuns}
        onOpenRun={openRun}
      />
    );
  }

  return (
    <GoalsSection
      goals={goals.source}
      runs={runRows}
      create={create}
      cancel={cancel}
      onOpenRun={openRun}
      selectedGoalId={selectedGoalId}
      onSelectGoal={setSelectedGoalId}
    />
  );
}

/**
 * One run, with the two reads that belong to it and to nothing else.
 *
 * IT EXISTS ONLY WHILE A RUN IS OPEN, which is the whole point: the steps
 * subscription and the journal's read are the run page's, so closing the page
 * closes the subscription and opening a different run is a different
 * collection with its own baseline -- the previous run's steps are not rows
 * this one is missing.
 */
function RunView({
  run,
  goal,
  approvals,
  derive,
  onBack,
  onOpenGoal,
  onOpenApprovals,
  onOpenRun,
}: {
  run: RunRow;
  goal: GoalRow | null;
  approvals: ApprovalRow[];
  derive: ReturnType<typeof useDeriveRun>;
  onBack: () => void;
  onOpenGoal: (goalId: string) => void;
  onOpenApprovals: (approvalId: string) => void;
  onOpenRun: (runId: string) => void;
}) {
  const steps = useRunSteps(run.id);
  const journal = useJournal(run.id);

  // ORDERED BY `seq` HERE AND NOT BY THE READ. `workStepsForOwnerRun` carries
  // `@unbounded`, which excludes `sort`, so the rows arrive in whatever order
  // the collection folded them -- and a timeline drawn in fold order
  // reshuffles itself the moment any step updates, which is exactly when
  // somebody is watching it.
  const ordered = useMemo(
    () => stepsInOrder(steps.snapshot.rows.map(stepFromRow).filter((step) => step.key !== "")),
    [steps.snapshot],
  );

  return (
    <RunPage
      run={run}
      goal={goal}
      steps={ordered}
      stepsState={steps.snapshot.state}
      approvals={approvals}
      journal={journal}
      derive={derive}
      onBack={onBack}
      onOpenGoal={onOpenGoal}
      onOpenApprovals={onOpenApprovals}
      onOpenRun={onOpenRun}
    />
  );
}

function NexusSettingsSection({
  settings,
  update,
}: {
  settings: NexusSettings;
  update: (patch: Partial<NexusSettings>) => void;
}) {
  return (
    <div className="os-settings">
      <Head title="Nexus settings" />
      <Panel label="Nexus settings">
        <fieldset className="os-field-group">
          <legend>Open Nexus on</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Default section">
            {NEXUS_SECTIONS.map((section) => (
              <button
                key={section.id}
                type="button"
                role="radio"
                aria-checked={settings.defaultSection === section.id}
                className="os-choice"
                onClick={() => update({ defaultSection: section.id })}
              >
                {section.name}
              </button>
            ))}
          </div>
          <p className="os-caption">
            Applies the next time a Nexus window opens; it does not move the window you are looking
            at. Approvals is the one worth choosing if you spend the day here -- a run parked on a
            question does not move until somebody answers it.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Finished runs</legend>
          <Check
            checked={settings.showFinishedRuns}
            onChange={(showFinishedRuns) => update({ showFinishedRuns })}
          >
            List runs that have finished
          </Check>
          <p className="os-caption">
            On by default, unlike every "show archived" preference in this shell -- and the
            difference is the subject rather than an inconsistency. An archived thing is one you
            filed away; a finished run is the ordinary end of every run there is, and "what did it
            do" is a question about finished runs. Turning it off narrows the list to what is in
            flight.
          </p>
        </fieldset>

        {/* THE ABSENT CONTROL, WITH AN ACCOUNT OF ITSELF. Somebody who opens
            settings in an app about spend looks for a budget field. There is
            none, and an absent control with nothing said about it reads as
            something nobody got round to building. */}
        <fieldset className="os-field-group">
          <legend>Ceilings and budgets</legend>
          <p className="os-caption">
            Not set here. A run's ceilings -- tokens, cost, wall clock, retries, model calls --
            are the goal's, inherited by every run of it, and they are set when the goal is
            accepted. A run that reaches one parks and asks you rather than stopping, and that
            question arrives in Approvals like any other.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>The journal</legend>
          <p className="os-caption">
            A run's model calls and observations are not part of the live feed -- deliberately, on
            volume grounds. The run page reads them when you ask and says when it looked, rather
            than showing a list that would silently never move.
          </p>
        </fieldset>

        <p className="os-caption">
          These are kept in this browser, separately from your desktop, so an app learning a
          checkbox can never cost you your desks. The defaults are{" "}
          {DEFAULT_NEXUS_SETTINGS.defaultSection} with finished runs listed.
        </p>
      </Panel>
      <Caption>
        Nothing here changes what the cluster does. Every read and write this app makes is decided
        by the engine against the rows you own.
      </Caption>
    </div>
  );
}
