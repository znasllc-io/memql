import { useCallback, useEffect, useId, useMemo, useSyncExternalStore, type ReactNode } from "react";
import { ListChecks } from "lucide-react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { Caption, ChoiceStack, ContentSkeleton, Fact, Facts, Field, Notice, Select, type Stop } from "../../../../kit";
import type { Act } from "../../../../kit/ActionBar";
import type { Breadcrumb } from "../../../../kit/Breadcrumbs";
import { useNow } from "../../../../kit/useNow";
import { Wizard } from "../../../../kit/Wizard";
import { useOsConnection } from "../../../../live/connection";
import { useMachines } from "../../../../live/machines";
import { machineFromRow, type MachineRow } from "../../../fleet/rows";
import { ProblemNotice } from "../../packages/ReportView";
import { toneFor } from "../../packages/refusals";
import { shortRepo, type PackageRow } from "../../packages/rows";
import type { PartsHeld } from "../../parts";
import type { PipelinePreview, Problem } from "../calls";
import { sameId, type Compute, type Delivery, type PipelineRow } from "../rows";
import { computeWords, joinWords } from "../words";
import {
  COMPUTE_CHOICES,
  DELIVERY_LABELS,
  barFor,
  deliveryCaption,
  fleetAvailable,
  fleetReason,
  movementOf,
  needsRemedy,
  onWords,
  openStepFor,
  readFromWords,
  readingOf,
  stepPhrase,
  stepsFor,
  type ActId,
  type ConnectFacts,
  type ConnectMode,
  type Reading,
  type StepId,
} from "./flow";
import type { ConnectFlow } from "./useConnectFlow";
import "./connect.css";

// CONNECT PIPELINE: the rail-as-form page over the source (design record D14;
// issue memql#5502).
//
// ===========================================================================
// THE ADD-A-MACHINE DEVICE, OVER A SOURCE
// ===========================================================================
// The kit's one wizard (DESIGN.md, "One wizard for adding"): the orb, the
// act's own name, one sentence, then three stops -- Repository, Compute,
// Confirm -- and the floor. The page replaces the source page (rule 11) and
// goes back to it; there is no source picker, because the rail is OVER a
// source the person already chose.
//
//   Repository   pipelinesPreview, read on the way in. It writes nothing, so
//                the stages show before anything is confirmed, and a refusal
//                -- no pipeline block, a repository another source runs, a
//                grant that no longer reaches it -- stops here, in place.
//   Compute      where the steps run, offered from what is true: the fleet
//                only when a machine of yours allows pipelines.
//   Confirm      what connecting turns on, how changes arrive, and the
//                floor's one write.
//
// EVERYTHING IT KNOWS IS THE FLOW'S (held by DeployablesApp) or a reading of
// it (`flow.ts`), so the page can be unmounted by a tab change and remounted
// with every answer where it was. The page holds no state of its own.

export interface ConnectPageProps {
  pkg: PackageRow;
  /** The source's pipeline when it has one: a reconnect is prefilled from it. */
  pipeline: PipelineRow | null;
  flow: ConnectFlow;
  can: PartsHeld;
  /** Where Back and the trail's parent go: the source page. */
  backLabel: string;
  onBack: () => void;
  /** The pipeline is connected: back to the source page, which now shows its checks. */
  onDone: () => void;
  /**
   * The ANCESTORS of this page -- the list, then the source -- when the
   * section knows them; the wizard names itself, as DomainWizard's `trail`
   * does, because its name follows the flow ("Connect pipeline", "Change
   * pipeline") and the section should not have to know which. Without them
   * the trail is the source alone, which still goes back to the right place.
   */
  trail?: readonly Breadcrumb[];
}

/** The source's repository as the source names it: what the page can say before anything is read. */
function repositoryName(pkg: PackageRow): string {
  if (pkg.sourceKind === "repo" && pkg.repoUrl.trim() !== "") return shortRepo(pkg.repoUrl);
  return pkg.name.trim() || "this source";
}

/**
 * The viewer's machines, read off the shell's one machines feed.
 *
 * KNOWN once the feed has answered. With no feed at all -- a section mounted
 * outside the shell -- there is no fleet to offer, which is an answer, and a
 * skeleton waiting on a read nobody will make would not be one.
 */
function useMachineRows(): { rows: MachineRow[]; known: boolean } {
  const machines = useMachines();
  const { collection } = machines;
  const subscribe = useCallback((listener: () => void) => (collection ? collection.subscribe(listener) : () => {}), [collection]);
  const snapshot = useSyncExternalStore(subscribe, () => collection?.snapshot ?? null, () => null);
  const rows = useMemo(() => (snapshot ? snapshot.rows.map((row) => machineFromRow(row as Row)) : []), [snapshot]);
  return { rows, known: machines.settled || machines.feedState === "absent" };
}

export function ConnectPage({ pkg, pipeline, flow, can, backLabel, onBack, onDone, trail }: ConnectPageProps) {
  const connection = useOsConnection();
  const machines = useMachineRows();
  const ids = useId();
  // A WAIT IS MEASURED BY THE SECOND, and nothing else here needs a clock.
  const waiting = flow.read.state === "reading" || flow.outcome.state === "connecting";
  const now = useNow(waiting ? 1000 : 60_000);

  const held = sameId(flow.packageId, pkg.id);
  // The flow fixes its mode when it starts. A page over another source's flow
  // reads its own from the pipeline it was handed.
  const mode: ConnectMode = held ? flow.mode : pipeline?.status === "active" ? "change" : "connect";
  const repository = repositoryName(pkg);
  const facts: ConnectFacts = {
    mode,
    can: can.connect,
    held,
    online: connection !== null,
    repository,
    read: flow.read,
    existing: flow.existing,
    // The pipeline's steps go to its owner's machines, and only the source's
    // owner connects one: the source's owner is whose fleet is asked about.
    fleet: { known: machines.known, available: fleetAvailable(machines.rows, pkg.ownerUserId) },
    answers: flow.answers,
    outcome: flow.outcome,
    reviewed: flow.reviewed,
    now: now.getTime(),
  };
  const reading = readingOf(facts);
  const steps = stepsFor(facts, reading);
  const bar = barFor(facts, reading);

  // A STOPPED COMPUTE IS PAST THE STAGES. Steps that need a machine none of
  // yours allows open the page on Compute, the stop that stopped. When one of
  // your machines starts allowing pipelines while the page is open, Compute
  // turns into a question in place -- the page stays where the person was
  // looking rather than going back to stages it never showed them.
  const shownCompute = held && reading.phase === "needsFleet" && !flow.reviewed;
  const { review } = flow;
  useEffect(() => {
    if (shownCompute) review();
  }, [shownCompute, review]);

  // THE PAGE SHOWS THE STOP THE FLOW IS AT, and a person may open any other
  // that opens, to read it again. Their choice holds until the flow moves.
  const movement = movementOf(reading, facts);
  const pinned = held ? flow.chosen : null;
  const chosen = pinned !== null && pinned.at === movement && steps.some((s) => s.id === pinned.step && s.openable) ? (pinned.step as StepId) : null;
  const open: StepId = chosen ?? openStepFor(steps, facts);

  const title = mode === "change" ? "Change pipeline" : "Connect pipeline";
  const notDone = mode === "change" ? "The changes were not saved." : "The pipeline was not connected.";

  const drawn: Stop[] = steps.map((step) => ({
    id: step.id,
    name: step.name,
    state: step.state,
    sentence: step.sentence,
    answer: step.answer,
    openable: step.openable,
    body: step.openable ? bodyFor(step.id) : undefined,
  }));

  const acts: Act[] = bar.acts.map((act) => ({
    label: act.label,
    tone: act.tone,
    text: act.text,
    busy: act.busy,
    onAct: () => run(act.id),
  }));

  return (
    <Wizard
      className="pipeline-connect"
      icon={<ListChecks size={20} aria-hidden />}
      title={title}
      lead={`Run ${repository}'s checks on this cluster and report them on GitHub.`}
      breadcrumbs={[...(trail ?? [{ label: backLabel, onSelect: onBack }]), { label: title }]}
      /* GOING BACK IS LEAVING. The answers are held above the section, so the
         source page's Connect pipeline opens this again where it was left;
         only Cancel on the floor drops them. */
      back={{ label: backLabel, onSelect: onBack }}
      label={mode === "change" ? "Changing a pipeline" : "Connecting a pipeline"}
      steps={drawn}
      open={open}
      onOpen={(id) => {
        if (held) flow.openStep(id, movement);
      }}
      status={{ word: bar.word, detail: bar.detail === "" ? undefined : bar.detail, tone: bar.tone, meta: bar.meta === "" ? undefined : bar.meta }}
      acts={acts}
      context={{ page: title, packageId: pkg.id, repository, phase: reading.phase, step: open }}
    />
  );

  function run(id: ActId): void {
    switch (id) {
      case "cancel":
        // Nothing has been written, so cancelling is going. Only this
        // source's answers are this page's to drop: a flow held over another
        // source belongs to the page that started it.
        if (held) flow.reset();
        onBack();
        return;
      case "leave":
        onBack();
        return;
      case "read":
        flow.start(pkg.id, pipeline);
        return;
      case "readAgain":
        flow.readAgain();
        return;
      case "continue":
        flow.review();
        return;
      case "connect":
        if (reading.preview !== null && reading.compute !== null) {
          // THE MANIFEST'S SECRET NAMES ARE THE ALLOWLIST: Confirm lists them,
          // and connecting is what allows them. Values never pass through here.
          flow.connect({ compute: reading.compute.value, delivery: reading.delivery, secretNames: reading.preview.secrets });
        }
        return;
      case "done":
        flow.reset();
        onDone();
        return;
    }
  }

  function bodyFor(id: StepId): ReactNode {
    const problem = reading.problem !== null && reading.problem.at === id ? reading.problem.problem : null;
    switch (id) {
      case "repository":
        return <RepositoryBody reading={reading} problem={problem} />;
      case "compute":
        return (
          <ComputeBody
            reading={reading}
            problem={problem === null ? null : <ProblemAt problem={problem} fallback={notDone} />}
            name={`${ids}-compute`}
            onChoose={flow.chooseCompute}
          />
        );
      case "confirm":
        return (
          <ConfirmBody
            reading={reading}
            mode={mode}
            problem={problem === null ? null : <ProblemAt problem={problem} fallback={notDone} />}
            selectId={`${ids}-delivery`}
            onDelivery={flow.chooseDelivery}
          />
        );
    }
  }
}

/**
 * A refusal, AT THE STOP IT BELONGS TO (README, "A STOP OWNS ITS OWN
 * REFUSAL"): the copy keyed by its code above, the server's sentence verbatim
 * beneath. A failure with no code is not dressed as a refusal; it says what
 * did not happen, and the server's own words.
 */
function ProblemAt({ problem, fallback }: { problem: Problem; fallback: string }) {
  if (problem.code !== "") return <ProblemNotice problem={problem} tone={toneFor(problem.code)} />;
  return <Notice tone="error" sentence={fallback} detail={problem.message} />;
}

// ---------------------------------------------------------------------------
// Repository
// ---------------------------------------------------------------------------

function RepositoryBody({ reading, problem }: { reading: Reading; problem: Problem | null }) {
  switch (reading.phase) {
    case "unheld":
      return <Caption>Connecting starts by reading this source's memql-package.yaml.</Caption>;
    case "offline":
      return null;
    case "reading":
      // The shape of the stages, not a sentence about reading them: the floor
      // carries the running read, its words and its measure.
      return <ContentSkeleton kind="detail" label="Loading the stages memql-package.yaml declares" />;
    case "failed":
      return problem === null ? null : (
        <Notice
          tone="error"
          sentence="memql-package.yaml could not be read."
          next="Nothing was written. Read again to try once more."
          detail={problem.message}
        />
      );
  }
  if (problem !== null) return <ProblemNotice problem={problem} tone={toneFor(problem.code)} />;
  if (reading.preview === null) return null;
  const from = readFromWords(reading.preview);
  return (
    <>
      <StageList preview={reading.preview} />
      {from === "" ? null : <Caption>{from}</Caption>}
    </>
  );
}

/**
 * The stages, in the order the pipeline runs them: each one's name, its steps
 * as the manifest names them, and when it runs if not on every run.
 *
 * ITS SHAPE, NEVER ITS COMMANDS. The preview carries no command for a step,
 * and a page that did not have one could not be the place a command leaks.
 */
function StageList({ preview }: { preview: PipelinePreview }) {
  return (
    <ul className="pipeline-connect-stages" aria-label={`Stages of ${preview.repository}`}>
      {preview.stages.map((stage, index) => {
        const when = onWords(stage.on);
        return (
          <li key={`${index}:${stage.name}`} className="pipeline-connect-stage">
            <span className="pipeline-connect-stage-name">{stage.name}</span>
            <div className="pipeline-connect-stage-body">
              {stage.steps.length > 0 ? (
                <ul className="pipeline-connect-steps" aria-label={`Steps of ${stage.name}`}>
                  {stage.steps.map((step, at) => (
                    <li key={`${at}:${step.name}`}>{stepPhrase(step)}</li>
                  ))}
                </ul>
              ) : null}
              {stage.channel !== "" ? <p className="pipeline-connect-notify">{`Notifies ${stage.channel}`}</p> : null}
              {when !== "" ? <p className="pipeline-connect-when">{when}</p> : null}
            </div>
          </li>
        );
      })}
    </ul>
  );
}

// ---------------------------------------------------------------------------
// Compute
// ---------------------------------------------------------------------------

/**
 * Where the steps run: one question, answered by choosing.
 *
 * NO CONTINUE. Choosing is the answer and the flow moves on to Confirm; the
 * checked option is the preselection until somebody chooses, and choosing it
 * is as much an answer as choosing the other one.
 */
function ComputeBody({ reading, problem, name, onChoose }: {
  reading: Reading;
  problem: ReactNode;
  name: string;
  onChoose: (compute: Compute) => void;
}) {
  const compute = reading.compute;
  if (compute === null) return null;
  if (compute.options === null) {
    return (
      <>
        {problem}
        <ContentSkeleton kind="form" label="Loading your machines" />
      </>
    );
  }
  if (compute.options.length === 0) {
    return (
      <>
        {problem}
        <Notice tone="warn" sentence={`${compute.needers.join("; ")}.`} next={needsRemedy(compute.needers)} />
      </>
    );
  }
  const offersFleet = compute.options.includes("cluster_and_fleet");
  const why = compute.needers.length > 0
    ? fleetReason(compute.needers)
    : offersFleet
      ? "No step needs a machine yet, so the cluster is suggested."
      : "Your fleet is offered once one of your machines allows pipelines in its policy.yaml.";
  return (
    <>
      {problem}
      <ChoiceStack
        name={name}
        label="Where steps run"
        /* PROSE: "Cluster and your fleet" is a sentence about a choice, not a
           value anybody types. The value the engine stores is
           cluster_and_fleet, and nobody needs to read that here. */
        voice="prose"
        value={compute.value}
        onChange={(value) => onChoose(value === "cluster_and_fleet" ? "cluster_and_fleet" : "cluster")}
        options={compute.options.map((value) => ({ value, label: COMPUTE_CHOICES[value].label, description: COMPUTE_CHOICES[value].description }))}
      />
      <Caption>{why}</Caption>
    </>
  );
}

// ---------------------------------------------------------------------------
// Confirm
// ---------------------------------------------------------------------------

/**
 * What connecting turns on, and the one thing still asked: how changes arrive.
 *
 * THE SECRETS ARE A CONSEQUENCE, SAID BEFORE THE ACT. Connecting allows every
 * secret the manifest's steps name, and anybody who can push a branch can add a
 * step that reads one (docs/public/operate/pipelines.md, "Allow only what
 * anybody who can push a branch may see") -- a fact a person has to have read
 * before pressing the button, not after.
 *
 * ONCE PRESSED, THE ANSWERS ARE FACTS. The field becomes a line of the facts:
 * a select left open beside a write in flight would accept a change the write
 * has already been sent without.
 */
function ConfirmBody({ reading, mode, problem, selectId, onDelivery }: {
  reading: Reading;
  mode: ConnectMode;
  problem: ReactNode;
  selectId: string;
  onDelivery: (delivery: Delivery) => void;
}) {
  const preview = reading.preview;
  if (preview === null || reading.compute === null) return null;
  const locked = reading.phase === "connecting" || reading.phase === "connected";
  const secrets = preview.secrets;
  return (
    <>
      {problem}
      <Facts>
        <Fact label="Check on GitHub" value={preview.checkName} />
        <Fact label="Stages" value={joinWords(...preview.stages.map((stage) => stage.name))} />
        <Fact label="Where steps run" value={computeWords(reading.compute.value)} />
        <Fact label="Secrets the steps read" value={secrets.length > 0 ? secrets.join(", ") : "None"} mono={secrets.length > 0} />
        {locked ? <Fact label="How changes arrive" value={DELIVERY_LABELS[reading.delivery]} /> : null}
      </Facts>
      {secrets.length > 0 ? (
        <Caption>
          {mode === "change" ? "Saving" : "Connecting"} lets their values reach the steps that name them. Anyone who can push a
          branch to the repository can add a step that reads one.
        </Caption>
      ) : null}
      {locked ? null : (
        <>
          <Field label="How changes arrive">
            <Select
              id={selectId}
              label="How changes arrive"
              value={reading.delivery}
              onChange={(value) => onDelivery(value === "poll" ? "poll" : "webhook")}
            >
              <option value="webhook">{DELIVERY_LABELS.webhook}</option>
              <option value="poll">{DELIVERY_LABELS.poll}</option>
            </Select>
          </Field>
          <Caption>{deliveryCaption(reading.delivery, preview.suggestedDelivery)}</Caption>
        </>
      )}
    </>
  );
}
