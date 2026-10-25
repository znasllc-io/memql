import { useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { FileText } from "lucide-react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsIfPresent } from "../../../chrome/state";
import { useAuthSource } from "../../../auth/context";
import { AutoRefresh, Button, Caption, ContentSkeleton, Head, Notice, Panel, RecordListSkeleton, Subhead, useAppReach } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { useMachines } from "../../../live/machines";
import { useOsConnection } from "../../../live/connection";
import { machineFromRow, machineName } from "../../fleet/rows";
import { ProblemNotice } from "../packages/ReportView";
import { copyFor, toneFor } from "../packages/refusals";
import type { PartsHeld } from "../parts";
import { runBarFor, type RunActName } from "./acts";
import { cancelRun, problemFrom, rerunRun, type Problem } from "./calls";
import { useRunFiles, useRunSteps } from "./feeds";
import { useExpiredRunFiles } from "./expiry";
import { runGithubUrl } from "./github";
import { fetchLogTail, type LogTail } from "./logTail";
import { artifactDisplayName, isFinished, type PipelineRow, type RunFileRow, type RunRow, type StepRow } from "./rows";
import { newerAttemptOf, runById } from "./runs";
import { Mark } from "./Mark";
import { StopsAcross } from "./StopsAcross";
import { openStopFor, shardsOf, stopsForRun, type RunStop } from "./stops";
import { attemptWords, branchWords, durationWords, modeWord, runTitle, shortSha, stepLabel, stepMark, stepWord, triggerWords, whenWords, whereWords } from "./words";
import "./pipelines.css";

// THE RUN PAGE, layout A (design record D13; issue memql#5500): the
// add-a-machine device applied to a run.
//
//   the trail       Runs > source > branch, or Sources > source > branch --
//                   back follows where the person came from
//   the Head        the commit's message as the title
//   the meta line   what opened it, the commit, the mode, how long, the attempt
//   the stops       the stages across the top, read from the step rows
//   the open stop   its steps: where each ran and for how long; a failed one
//                   with its last lines and the way to the whole log; a
//                   skipped one with its reason in words
//   artifacts       what the steps saved, each a file in the Library
//   the bar         the state, then the legal acts: Open on GitHub, Re-run,
//                   Re-run failed -- or Cancel while it runs
//
// IT REPLACES THE LIST (DESIGN.md rule 11), so it is a page with one Head and
// its own scroller, and the bar is a grid row on the window's bottom edge.
//
// NOTHING HERE IS A COPY OF A RUN'S STATE. The run is the root's retained
// feed's row; the steps and files are this page's own feeds, retained while it
// is open; a re-run's new attempt arrives on the runs feed like any other run.

export interface RunPageProps {
  runId: string;
  /** Every run the root's feed holds: the run itself, and a newer attempt of its key. */
  runs: readonly RunRow[];
  runsState: string;
  pipeline: PipelineRow | null;
  /** What the source is called, for the page context. */
  sourceName: string;
  breadcrumbs: readonly Breadcrumb[];
  back: { label: string; onSelect: () => void };
  can: PartsHeld;
  /** Open another run of this key -- the attempt a re-run just opened. */
  onOpenRun: (runId: string) => void;
}

export function RunPage(props: RunPageProps) {
  const run = runById(props.runs, props.runId);
  if (run === null) {
    const reading = props.runsState === "seeding" || props.runsState === "disconnected";
    return (
      <div className="os-deploy-pane pipeline-run-page">
        <div className="os-deploy-scroll">
          <Panel label="Run">
            <Head title="Run" breadcrumbs={props.breadcrumbs} back={props.back} />
            {reading ? <ContentSkeleton kind="detail" label="Loading the run" /> : (
              <Notice tone="warn" sentence="This run is not one you can read here." next="A run belongs to the person whose source it checked. Open Runs to see yours." />
            )}
          </Panel>
        </div>
      </div>
    );
  }
  return <RunPageFor {...props} run={run} />;
}

function useMachineNames(): (workerId: string) => string {
  const { collection } = useMachines();
  const snapshot = useSyncExternalStore(
    useMemo(() => (collection ? collection.subscribe.bind(collection) : () => () => {}), [collection]),
    () => collection?.snapshot ?? null,
  );
  return useMemo(() => {
    const names = new Map<string, string>();
    for (const raw of (snapshot?.rows ?? []) as Row[]) {
      const m = machineFromRow(raw);
      const id = m.id.slice(m.id.lastIndexOf(":") + 1);
      names.set(id, machineName(m));
    }
    return (workerId: string) => names.get(workerId.slice(workerId.lastIndexOf(":") + 1)) ?? "";
  }, [snapshot]);
}

function RunPageFor({ run, runs, pipeline, sourceName, breadcrumbs, back, can, onOpenRun }: RunPageProps & { run: RunRow }) {
  const connection = useOsConnection();
  const nexus = useAppReach("nexus");
  const os = useOsIfPresent();
  const machineName = useMachineNames();
  const { steps, state: stepsState } = useRunSteps(run.workRunId);
  const files = useRunFiles(run.workRunId);
  const expiry = useExpiredRunFiles(run.id, steps, files.byFileId, files.state === "live" && stepsState === "live");
  const stops = useMemo(() => stopsForRun(run, steps), [run, steps]);
  const uncertainSteps = stops.flatMap((stop) => stop.steps.filter(executionUncertain).map((step) => ({ step, label: `${stop.name} / ${stepLabel(step, shardsOf(step, stop))}` })));

  // THE OPEN STOP FOLLOWS THE RUN until somebody picks one, and a pick holds
  // only until the run moves: a stage finishing re-asks where the question is
  // now, which is the add-a-machine flow's rule for its own override.
  const computed = openStopFor(stops, run);
  const movement = `${run.id}:${run.status}:${stops.map((s) => s.state).join(",")}`;
  const [chosen, setChosen] = useState<{ at: string; stop: string } | null>(null);
  const open = chosen !== null && chosen.at === movement && stops.some((s) => s.id === chosen.stop) ? chosen.stop : computed;
  const openStop = stops.find((s) => s.id === open) ?? null;

  const [busy, setBusy] = useState<RunActName | null>(null);
  const [problem, setProblem] = useState<Problem | null>(null);
  const [confirmCancel, setConfirmCancel] = useState(false);
  useEffect(() => { setProblem(null); setConfirmCancel(false); }, [run.id]);

  const githubUrl = runGithubUrl(run);
  const newer = newerAttemptOf(run, runs);
  const bar = runBarFor({ run, steps, newer, pipelineActive: pipeline?.status !== "disconnected", can, onGithub: githubUrl !== "" });

  async function act(name: RunActName) {
    if (name === "Open on GitHub") {
      window.open(githubUrl, "_blank", "noopener,noreferrer");
      return;
    }
    if (name === "Cancel" && !confirmCancel) {
      setConfirmCancel(true);
      return;
    }
    if (!connection) return;
    setBusy(name);
    setProblem(null);
    try {
      if (name === "Cancel") {
        await cancelRun(connection.query, run.id);
        setConfirmCancel(false);
      } else {
        const opened = await rerunRun(connection.query, run.id, name === "Re-run failed");
        if (opened.runId !== "") onOpenRun(opened.runId);
      }
    } catch (err) {
      setProblem(problemFrom(err));
    } finally {
      setBusy(null);
    }
  }

  const acts: Act[] = confirmCancel
    ? [
        { label: "Keep this run", text: true, onAct: () => setConfirmCancel(false) },
        { label: "Cancel the run", tone: "danger", busy: busy === "Cancel", onAct: () => void act("Cancel") },
      ]
    : bar.acts.map((spec) => ({
        label: spec.name,
        ...(spec.primary ? { tone: spec.danger ? ("danger" as const) : ("primary" as const) } : { text: true }),
        busy: busy === spec.name,
        onAct: () => void act(spec.name),
      }));

  const refused = stops.length === 0 && run.refusalCode !== "";
  const artifacts = steps.flatMap((step) => step.artifactFileIds.map((id) => ({ step, id, file: files.byFileId.get(id) ?? null })));
  const panelId = `pipeline-run-${run.id}-stage`;
  const title = runTitle(run);

  return (
    <div className="os-deploy-pane pipeline-run-page" data-os-page-context={JSON.stringify({ page: "Pipeline run", runId: run.id, source: sourceName, sha: run.sha, attempt: run.attempt })}>
      <AutoRefresh enabled={expiry.needsRefresh} busy={expiry.loading} onRefresh={expiry.refresh} />
      <div className="os-deploy-scroll">
        <section className="os-app-stack" aria-label={`Run ${title}`}>
          <Head navigationKey={`run:${run.id}`} title={title} breadcrumbs={breadcrumbs} back={back} />
          <p className="pipeline-run-meta">
            {run.event === "pull_request" && run.pullRequest > 0 ? (
              <a href={`https://github.com/${run.repository}/pull/${run.pullRequest}`} target="_blank" rel="noopener noreferrer">{triggerWords(run)}</a>
            ) : <span>{triggerWords(run)}</span>}
            {branchWords(run) !== "" ? <span>{branchWords(run)}</span> : null}
            {run.sha !== "" ? (
              <a className="pipeline-mono" href={`https://github.com/${run.repository}/commit/${run.sha}`} target="_blank" rel="noopener noreferrer" title={run.sha}>{shortSha(run.sha)}</a>
            ) : null}
            <span>{modeWord(run.mode)}</span>
            {attemptWords(run) !== "" ? <span>{attemptWords(run)}</span> : null}
            {whenWords(run.queuedAt, new Date()) !== "" ? <time dateTime={run.queuedAt}>{whenWords(run.queuedAt, new Date())}</time> : null}
          </p>

          {run.maintenanceState && run.maintenanceState !== "none" ? <Notice tone={run.maintenanceState === "human" ? "warn" : "info"} sentence={run.maintenanceState === "analysis" ? "A follow-up is investigating this failure." : run.maintenanceState === "human" ? "This failure needs a person’s attention." : "A follow-up is being prepared."} detail={run.maintenanceMessage || undefined}>{run.maintenanceGoalId && nexus.sections.includes("goals") ? <Button tone="quiet" onClick={() => nexus.open("goals", { goalId: run.maintenanceGoalId })}>Open follow-up goal</Button> : null}</Notice> : null}
          {run.notes.map((note) => (
            <ProblemNotice key={note.code} problem={{ code: note.code, message: note.message }} tone="warn" />
          ))}
          {uncertainSteps.map(({ step, label }) => (
            <UncertainStepNotice key={step.key} step={step} label={label} />
          ))}

          {refused ? (
            <ProblemNotice problem={{ code: run.refusalCode, message: run.refusalMessage, scope: run.refusalScope }} tone={toneFor(run.refusalCode)} />
          ) : stops.length === 0 ? (
            isFinished(run) ? <Caption>This run had nothing to run.</Caption> : <ContentSkeleton kind="detail" label="Loading the run's stages" />
          ) : (
            <>
              {run.refusalCode !== "" ? (
                <ProblemNotice problem={{ code: run.refusalCode, message: run.refusalMessage, scope: run.refusalScope }} tone={toneFor(run.refusalCode)} />
              ) : null}
              <div className="pipeline-run-workspace"><StopsAcross stops={stops} open={open} onOpen={(id) => setChosen({ at: movement, stop: id })} label="Stages" panelId={panelId} />
              <section className="pipeline-stage-panel" role="tabpanel" id={panelId} aria-labelledby={openStop ? `${panelId}-tab-${openStop.id}` : undefined}>
                {openStop === null ? null : openStop.steps.length === 0 ? (
                  stepsState === "seeding" || run.workRunId === "" && !isFinished(run) ? <RecordListSkeleton label="Loading the steps" rows={2} /> : <Caption>{stageSummary(openStop)}</Caption>
                ) : (
                  <StageSteps stop={openStop} files={files.byFileId} expired={expiry.files} machineName={machineName} onOpenFile={os ? (artifactId) => os.actions.openApp("files", "browse", { artifactId }) : null} />
                )}
              </section></div>
            </>
          )}

          {artifacts.length > 0 ? (
            <section className="os-report-part pipeline-artifacts" aria-label="Artifacts">
              <Subhead meta={artifacts.length}>Artifacts</Subhead>
              <RecordList as="ul" density="compact">
                {artifacts.map(({ step, id, file }) => (
                  <RecordRow
                    key={`${step.key}:${id}`}
                    icon={<FileText size={16} aria-hidden />}
                    name={artifactDisplayName(file?.name || expiry.files.get(id) || "Artifact")}
                    secondary={step.key}
                    state={file ? undefined : expiry.files.has(id) ? "Expired" : files.state === "seeding" || expiry.loading ? "Checking availability" : "Unavailable"}
                    {...(file && os ? { label: `Open ${artifactDisplayName(file.name)} in Files`, onOpen: () => os.actions.openApp("files", "browse", { artifactId: file.artifactId }) } : {})}
                  />
                ))}
              </RecordList>
            </section>
          ) : null}
          {expiry.error ? <Caption>{expiry.error}</Caption> : null}

          {problem !== null ? (
            problem.code !== "" ? <ProblemNotice problem={problem} tone={toneFor(problem.code)} /> : <Notice tone="error" sentence="The cluster refused." detail={problem.message} />
          ) : null}
        </section>
      </div>
      <ActionBar state={confirmCancel ? "Cancel this run?" : bar.state} detail={confirmCancel ? "Request cancellation. Finished steps keep their results." : bar.detail} tone={bar.tone} live acts={acts} />
    </div>
  );
}

function executionUncertain(step: StepRow): boolean {
  return step.status === "failed" && step.errorCode === "pipeline_execution_uncertain";
}

/** This typed receipt matters even when another stage is selected. It is not a cleanup receipt. */
function UncertainStepNotice({ step, label }: { step: StepRow; label: string }) {
  const copy = copyFor(step.errorCode)!;
  return (
    <Notice tone="warn" sentence={copy.title} next={copy.next}>
      <Caption>{label}</Caption>
      {step.errorMessage !== "" ? <details className="os-report-log">
        <summary>Details for {label}</summary>
        <pre>{step.errorMessage}</pre>
      </details> : null}
    </Notice>
  );
}

/** A stage with no step rows to show: what the run row's table says of it. */
function stageSummary(stop: RunStop): string {
  if (stop.status === "blocked") return "Not run: an earlier stage failed.";
  if (stop.status === "skipped") return "Skipped.";
  if (stop.status === "waiting") return "Waiting for the stages before it.";
  return stop.word + (stop.took ? `, ${stop.took}` : "") + ".";
}

function StageSteps({ stop, files, expired, machineName, onOpenFile }: {
  stop: RunStop;
  files: ReadonlyMap<string, RunFileRow>;
  expired: ReadonlyMap<string, string>;
  machineName: (workerId: string) => string;
  onOpenFile: ((artifactId: string) => void) | null;
}) {
  return (
    <ul className="os-record-list pipeline-steps" data-density="compact" aria-label={`Steps of ${stop.name}`}>
      {stop.steps.map((step) => {
        const label = stepLabel(step, shardsOf(step, stop));
        const where = whereWords(step, machineName);
        const word = stepWord(step);
        const log = step.logFileId !== "" ? files.get(step.logFileId) ?? null : null;
        const skipped = step.status === "skipped";
        return (
          <li key={step.key} className="pipeline-step" data-status={step.status}>
            <RecordRow
              icon={<Mark state={stepMark(step)} />}
              name={label}
              secondary={skipped ? (step.reason || word) : where}
              state={word}
              tone={step.status === "failed" ? "warn" : step.status === "done" || step.status === "running" || step.errorCode === "pipeline_passed_earlier" ? "accent" : "muted"}
              stateExtra={step.durationMs > 0 && !skipped ? <span className="pipeline-step-took">{durationWords(step.durationMs)}</span> : null}
              dim={step.status === "pending" || step.status === "ready" || step.status === "waiting"}
            />
            {step.status === "failed" || (step.status === "cancelled" && step.errorMessage !== "") ? (
              <FailedStep step={step} log={log} onOpenFile={onOpenFile} />
            ) : null}
            {step.notes.map((note) => <ProblemNotice key={note.code} problem={{ code: note.code, message: note.message }} tone="warn" />)}
            {expired.has(step.logFileId) ? <Caption>Log expired.</Caption> : null}
          </li>
        );
      })}
    </ul>
  );
}

/**
 * A failed step's account: the server's sentence (a typed failure under its
 * copy), its last lines, and the way to the whole log.
 *
 * "SAVED TO YOUR LIBRARY" IS A FACT, NOT AN ACT. The runner archives every step
 * that ran as a file in its owner's Library before it reports (epic
 * memql#5478), so the full log is already there by the time this renders; a
 * "Save" button would promise an act that has already happened. When the
 * Library refused the file -- a quota, no storage -- there is no log here and
 * the step's own note says so.
 */
function FailedStep({ step, log, onOpenFile }: { step: StepRow; log: RunFileRow | null; onOpenFile: ((sourceRef: string) => void) | null }) {
  const auth = useAuthSource();
  const [tail, setTail] = useState<LogTail | null>(null);
  const [tailError, setTailError] = useState("");
  const artifactId = log?.artifactId ?? "";
  // A LOG TAIL OPENS AT ITS END. The failure's own summary -- the FAIL line,
  // the exit status -- is the last thing a step prints, and a box that opens
  // at its top shows the setup above it with the answer below the fold.
  const box = useRef<HTMLPreElement>(null);
  useLayoutEffect(() => {
    const el = box.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [tail]);
  useEffect(() => {
    setTail(null);
    setTailError("");
    if (artifactId === "") return;
    const abort = new AbortController();
    fetchLogTail({ artifactId, bearer: () => auth.bearer(), signal: abort.signal })
      .then((t) => { if (!abort.signal.aborted) setTail(t); })
      .catch((err) => { if (!abort.signal.aborted) setTailError(err instanceof Error ? err.message : String(err)); });
    return () => abort.abort();
  }, [artifactId, auth]);

  const coded = step.errorCode !== "" && step.errorCode !== "pipeline_executor_error";
  return (
    <div className="pipeline-step-failure">
      {executionUncertain(step) ? null : coded ? (
        <ProblemNotice problem={{ code: step.errorCode, message: step.errorMessage }} tone={toneFor(step.errorCode)} />
      ) : step.errorMessage !== "" ? <p className="pipeline-step-message">{step.errorMessage}</p> : null}
      {log === null ? null : tail === null && tailError === "" ? (
        <ContentSkeleton kind="detail" label="Loading the log's last lines" />
      ) : tailError !== "" ? (
        <Caption>{tailError}</Caption>
      ) : tail !== null && tail.lines.length > 0 ? (
        <pre ref={box} className="pipeline-log" data-os-scroll-ignore tabIndex={0} aria-label={`The last lines of ${step.key}'s log`}>{tail.lines.join("\n")}</pre>
      ) : (
        <Caption>The log is empty.</Caption>
      )}
      {log !== null ? (
        <p className="pipeline-log-acts">
          {onOpenFile ? <button type="button" className="os-link" onClick={() => onOpenFile(log.artifactId)}>Open the full log</button> : null}
          <span className="pipeline-log-saved">Saved to your Library</span>
        </p>
      ) : null}
    </div>
  );
}
