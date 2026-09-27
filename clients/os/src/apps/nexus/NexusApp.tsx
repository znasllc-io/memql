import { useEffect, useMemo, useRef, useState } from "react";
import type { ComponentProps } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { Caption, Check, ContentSkeleton, Head, Panel, SetupGroup } from "../../kit";
import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { ApprovalsSection } from "./ApprovalsSection";
import { AutomationsSection } from "./AutomationsSection";
import { GoalsSection } from "./GoalsSection";
import { GoalView } from "./GoalView";
import { OverviewSection } from "./OverviewSection";
import { EMPTY_RUN_MEMORY, RunPage, type RunPageMemory } from "./RunPage";
import { RunsSection } from "./RunsSection";
import { defaultRunId } from "./world";
import { NEXUS_APP_ID, NEXUS_LOG_CONCEPTS } from "./concepts";
import { PROCEDURE_PROMOTION } from "./ladder";
import { useAutomationFeeds } from "./useAutomations";
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
  runTitle,
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
  type NexusSettingsStore, NEXUS_REQUIRES, NEXUS_WANTS } from "./settings";
import {
  useApprovals,
  useGoals,
  useJournal,
  useRunArtifacts,
  useRunSteps,
  useRuns,
} from "./useNexus";
import { useSession } from "../../chrome/access";

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
// a selection held inside a section would be lost on the way. So the ids live
// here and each section is told what is open -- which also means going to
// Approvals to answer something and coming back lands somebody exactly where
// they were. A learned procedure's page is the fourth (epic memql#5408): its
// promotion is decided in Approvals, and its approval card links back to it.
//
// THE CATALOG'S FEEDS ARE HELD HERE TOO, for the same reason: the Automations
// list and a promotion's approval card both follow them, and two collections
// over one catalog would be free to disagree about where a procedure stands.

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
  const [openGoalId, setOpenGoalId] = useState("");
  const [openRunId, setOpenRunId] = useState("");
  // THE RUNS THE PERSON CAME THROUGH to reach the open one, from a run page:
  // a branch or a replay opened from a run, a child run, the run a branch
  // came from. Back pops it, so it returns to the run it was opened from
  // rather than to the list; a run opened from anywhere else starts it empty.
  const [runTrail, setRunTrail] = useState<string[]>([]);
  // What each run page was showing -- the open step, the version picked, the
  // drafts -- kept across a visit elsewhere, so Back lands where the person
  // left off. A ref: remembering must not re-render the app.
  const runMemory = useRef(new Map<string, RunPageMemory>());
  const [selectedApprovalId, setSelectedApprovalId] = useState("");
  const [selectedAutomationId, setSelectedAutomationId] = useState("");
  // Which run of the open goal the map draws, and the moment it is rewound to.
  // Both are the GOAL VIEW's, held here so leaving the view for an approval and
  // coming back lands somebody exactly where they were.
  const [mapRunId, setMapRunId] = useState("");
  const [openAt, setOpenAt] = useState("");

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

  // THE CATALOG, FOLLOWED WHILE A VISIBLE SURFACE NEEDS IT (see
  // useAutomations). Automations always does, and so does the Overview, whose
  // reuse figures count the same two lists; Approvals does only while a
  // promotion waits in it, because only that card names a procedure it has to
  // find.
  const feeds = useAutomationFeeds(
    sectionId === "automations" ||
      sectionId === "overview" ||
      (sectionId === "approvals" && approvalRows.some((a) => a.kind === PROCEDURE_PROMOTION)),
  );
  // Which procedure's page is open. A link naming one the feed does not hold
  // yet is kept until the feed has answered; the section then opens it or says
  // it is not there.
  const [openProcedureId, setOpenProcedureId] = useState("");

  function openRun(runId: string) {
    if (runId.trim() === "") return;
    setRunTrail([]);
    setOpenRunId(runId);
    askContext(`nexus run:${idTail(runId)}`);
    navigate("runs");
  }

  /** Open a run FROM the run page showing `from`, so Back returns there. */
  function openRunFrom(from: string, runId: string) {
    if (runId.trim() === "") return;
    setRunTrail((held) => [...held, from]);
    setOpenRunId(runId);
    askContext(`nexus run:${idTail(runId)}`);
  }

  function backFromRun() {
    const previous = runTrail[runTrail.length - 1];
    if (previous === undefined) {
      setOpenRunId("");
      return;
    }
    setRunTrail((held) => held.slice(0, -1));
    setOpenRunId(previous);
    askContext(`nexus run:${idTail(previous)}`);
  }

  function openGoal(goalId: string) {
    if (goalId.trim() === "") return;
    setSelectedGoalId(goalId);
    setOpenGoalId(goalId);
    setMapRunId(defaultRunId(runs.snapshot.rows, goalId));
    askContext(`nexus goal:${idTail(goalId)}`);
    navigate("goals");
  }

  function openApproval(approvalId: string) {
    if (approvalId.trim() === "") return;
    setSelectedApprovalId(approvalId);
    navigate("approvals");
  }

  function openProcedureById(constructId: string) {
    if (constructId.trim() === "") return;
    setOpenProcedureId(constructId);
    askContext(`nexus procedure:${idTail(constructId)}`);
    navigate("automations");
  }

  // A STANDING OPEN INSTRUCTION, id-matched on consumption so acting on a
  // stale render can never eat a newer one. The payload names ONE of the four
  // things this app can open; anything else is left alone rather than guessed
  // at, which is what keeps an unrelated opener from moving somebody's window.
  const handled = useRef("");
  useEffect(() => {
    if (intent === undefined || intent.id === handled.current) return;
    const payload = intent.payload;
    const runId = typeof payload["runId"] === "string" ? payload["runId"] : "";
    const goalId = typeof payload["goalId"] === "string" ? payload["goalId"] : "";
    const approvalId = typeof payload["approvalId"] === "string" ? payload["approvalId"] : "";
    const procedureId = typeof payload["procedureId"] === "string" ? payload["procedureId"] : "";
    // A MOMENT, so a rewound goal is shareable. The OS has no per-window URL --
    // this is the shell's deep-link primitive, and an opener that hands one in
    // gets the goal drawn as it stood. Ignored on a run or approval payload,
    // because neither of those surfaces is rewindable.
    const at = typeof payload["at"] === "string" ? payload["at"] : "";
    if (runId === "" && goalId === "" && approvalId === "" && procedureId === "") return;
    handled.current = intent.id;
    if (runId !== "") openRun(runId);
    else if (approvalId !== "") openApproval(approvalId);
    else if (procedureId !== "") openProcedureById(procedureId);
    else {
      setOpenAt(at);
      openGoal(goalId);
    }
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
  if (sectionId === "overview") {
    return (
      <OverviewSection
        goals={goals.snapshot}
        runs={runs.snapshot}
        approvals={approvals.snapshot}
        catalog={feeds.catalog.snapshot}
        procedures={feeds.procedures.snapshot}
        navigate={navigate}
      />
    );
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
  if (sectionId === "automations") {
    return (
      <AutomationsSection
        feeds={feeds}
        selectedId={selectedAutomationId}
        onSelect={setSelectedAutomationId}
        openProcedureId={openProcedureId}
        onOpenProcedure={openProcedureById}
        onCloseProcedure={() => setOpenProcedureId("")}
        approvals={approvalRows}
        approvalsKnown={approvals.snapshot.state === "live"}
        runs={runRows}
        onOpenApproval={openApproval}
        onOpenRun={openRun}
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
        procedures={feeds.procedureRows}
        onOpenProcedure={openProcedureById}
      />
    );
  }
  if (sectionId === "runs") {
    const open = runRows.find((run) => idTail(run.id) === idTail(openRunId)) ?? null;
    if (openRunId !== "" && open !== null) {
      const previousId = runTrail[runTrail.length - 1];
      const previous =
        previousId === undefined ? null : (runRows.find((run) => idTail(run.id) === idTail(previousId)) ?? null);
      return (
        // KEYED ON THE RUN: another run is another page, with its own open
        // step and its own drafts -- which the memory below hands back when
        // the person returns.
        <RunView
          key={idTail(open.id)}
          run={open}
          runs={runRows}
          goal={goalsById.get(idTail(open.goalId)) ?? null}
          approvals={pendingApprovalsOfRun(approvalRows, open.id)}
          derive={derive}
          backLabel={previousId === undefined ? "Runs" : previous === null ? "the run it came from" : runTitle(previous)}
          onBack={backFromRun}
          onBackToList={() => {
            setRunTrail([]);
            setOpenRunId("");
          }}
          onOpenGoal={openGoal}
          onOpenApprovals={openApproval}
          onOpenRun={(runId) => openRunFrom(open.id, runId)}
          memory={runMemory.current.get(idTail(open.id)) ?? EMPTY_RUN_MEMORY}
          onRemember={(memory) => runMemory.current.set(idTail(open.id), memory)}
        />
      );
    }
    // A RUN OPENED FROM A RUN PAGE, NOT ARRIVED YET. A branch or a replay
    // answers with the new run's id a moment before its row reaches the feed,
    // and falling back to the list in that moment would be the page moving
    // somewhere nobody asked to go. So the page it will be stands in its
    // shape, with Back to where the person was.
    if (openRunId !== "" && runTrail.length > 0) {
      return (
        <ArrivingRun
          onBack={backFromRun}
          onBackToList={() => {
            setRunTrail([]);
            setOpenRunId("");
          }}
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

  // THE GOAL VIEW REPLACES THE LIST (rule 11's `<- Goals` form). It is a map, a
  // rail and a detail -- taller than the run page, which already took this
  // form -- and two Heads in one scroller is the tell that neither happened.
  const openGoalRow =
    openGoalId === ""
      ? null
      : (goals.snapshot.rows.find((row) => idTail(rowId(row)) === idTail(openGoalId)) ?? null);
  const openGoalProjected = goalRows.find((goal) => idTail(goal.id) === idTail(openGoalId)) ?? null;
  if (openGoalRow !== null && openGoalProjected !== null) {
    return (
      <GoalViewHost
        goal={openGoalProjected}
        goalRow={openGoalRow}
        runRows={runs.snapshot.rows}
        approvalRows={approvals.snapshot.rows}
        openRunId={mapRunId === "" ? defaultRunId(runs.snapshot.rows, openGoalId) : mapRunId}
        onPickRun={setMapRunId}
        cancel={cancel}
        derive={derive}
        onBack={() => {
          setOpenGoalId("");
          setOpenAt("");
        }}
        onOpenRun={openRun}
        onOpenApproval={openApproval}
        openAt={openAt}
      />
    );
  }

  return (
    <GoalsSection
      goals={goals.source}
      runs={runRows}
      create={create}
      cancel={cancel}
      onOpenGoal={openGoal}
      selectedGoalId={selectedGoalId}
    />
  );
}

/** The row id, read the way `idTail` expects it. */
function rowId(row: Row): string {
  const value = row["id"];
  return typeof value === "string" ? value : "";
}

/**
 * The goal view, with the one feed that belongs to it and to nothing else.
 *
 * IT EXISTS ONLY WHILE A GOAL IS OPEN, which is the whole point: the steps
 * subscription is the page's, so closing the page closes it and opening a
 * different goal is a different collection with its own baseline -- the
 * previous run's steps are not rows this one is missing. Same rule the run page
 * takes, and the same reason: subscribing a window to every step of every run
 * this person owns in order to draw one of them is what it forbids.
 */
function GoalViewHost(
  props: Omit<ComponentProps<typeof GoalView>, "stepRows" | "stepsState" | "artifactRows">,
) {
  const steps = useRunSteps(props.openRunId);
  // Keyed on the same run for the same reason. It is a SECOND collection
  // rather than a field on the first because the two are different concepts
  // with different reads -- `useLiveCollection` is per concept, and folding
  // them would mean one baseline covering two seeds.
  const artifacts = useRunArtifacts(props.openRunId);
  return (
    <GoalView
      {...props}
      stepRows={steps.snapshot.rows}
      stepsState={steps.snapshot.state}
      artifactRows={artifacts.snapshot.rows}
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
  runs,
  goal,
  approvals,
  derive,
  backLabel,
  onBack,
  onBackToList,
  onOpenGoal,
  onOpenApprovals,
  onOpenRun,
  memory,
  onRemember,
}: {
  run: RunRow;
  runs: readonly RunRow[];
  goal: GoalRow | null;
  approvals: ApprovalRow[];
  derive: ReturnType<typeof useDeriveRun>;
  backLabel: string;
  onBack: () => void;
  onBackToList: () => void;
  onOpenGoal: (goalId: string) => void;
  onOpenApprovals: (approvalId: string) => void;
  onOpenRun: (runId: string) => void;
  memory: RunPageMemory;
  onRemember: (memory: RunPageMemory) => void;
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
      runs={runs}
      goal={goal}
      steps={ordered}
      stepsState={steps.snapshot.state}
      approvals={approvals}
      journal={journal}
      derive={derive}
      backLabel={backLabel}
      onBack={onBack}
      onBackToList={onBackToList}
      onOpenGoal={onOpenGoal}
      onOpenApprovals={onOpenApprovals}
      onOpenRun={onOpenRun}
      memory={memory}
      onRemember={onRemember}
    />
  );
}

/** The run page's shape, while the run it will show has not reached the feed. */
function ArrivingRun({ onBack, onBackToList }: { onBack: () => void; onBackToList: () => void }) {
  return (
    <div className="os-nexus-run">
      <Head
        title="New run"
        breadcrumbs={[{ label: "Runs", onSelect: onBackToList }, { label: "New run" }]}
        back={{ label: "the run it came from", onSelect: onBack }}
      />
      <div className="os-nexus-run-body">
        <ContentSkeleton kind="detail" label="Opening the new run" />
      </div>
    </div>
  );
}

function NexusSettingsSection({
  settings,
  update,
}: {
  settings: NexusSettings;
  update: (patch: Partial<NexusSettings>) => void;
}) {
  const { readiness } = useSession();
  return (
    <div className="os-settings">
      <Head title="Nexus settings" />
      {/* THE SET UP GROUP sits above the preferences on purpose: it is the
          reason a person was sent here from an unconfigured surface, and the
          first thing they need is what to configure and where. Rule 4 puts
          micro-preferences in Settings; it never said they come first. */}
      <SetupGroup
        app="Nexus"
        requires={NEXUS_REQUIRES}
        wants={NEXUS_WANTS}
        readiness={readiness}
      />
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
