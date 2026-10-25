import { useEffect, useRef, useState } from "react";
import { PackageCheck } from "lucide-react";
import { Caption, ContentSkeleton, Fact, Facts, Notice, RecordList, RecordRow, Subhead } from "../../../kit";
import { Wizard } from "../../../kit/Wizard";
import type { Stop, StopState } from "../../../kit/Rail";
import type { Act } from "../../../kit/ActionBar";
import { formatMoment } from "../../../kit/format";
import { pipelineFromRow, runFromRow, type RunRow } from "../../deployables/pipelines/rows";
import { runOutcome, stageWord } from "../../deployables/pipelines/words";
import { buildCall, buildTitle, buildWord, currentStep, readReleaseBuild, type BuildAction, type JourneyStep, type ReleaseBuild } from "./journey";
import { readCandidate, readTargets, matchingTarget } from "./model";
import { ReviewDetails, type ReleaseQueries } from "./ReleasesSection";
import { useReleaseRead } from "./useReleaseRead";

export interface ReleaseNavigation {
  onOpenRun?: (runId: string) => void;
  onOpenRepository?: (packageId: string) => void;
  onUpdates?: () => void;
}
interface Props extends ReleaseNavigation {
  intentId: string; query: ReleaseQueries | null; connected: boolean; visible: boolean;
  runs?: readonly RunRow[]; runsFresh?: boolean; back: () => void; onCandidate: (id: string) => void;
}
const order: JourneyStep[] = ["release", "source", "version", "build", "review", "publish", "catalog"];

export function ReleaseJourney({ intentId, query, connected, visible, runs = [], runsFresh = false, back, onCandidate, onOpenRun, onOpenRepository, onUpdates }: Props) {
  const available = connected && query !== null;
  const intent = useReleaseRead(`build:${intentId}`, available ? async signal => {
    const saved = readReleaseBuild(await buildCall(query, "releaseGetBuild", { intentId }, signal), intentId);
    // Observation is explicitly read-only with respect to remote effects. Never
    // start, resume, assemble or publish from a lifecycle effect or reconnect.
    return saved.actions.includes("observe") ? readReleaseBuild(await buildCall(query, "releaseObserveBuild", { intentId }, signal), intentId) : saved;
  } : null, visible, query);
  const build = intent.value;
  const candidateId = build?.candidateId ?? "";
  const candidate = useReleaseRead(`journey-candidate:${candidateId}`, available && candidateId ? async signal => readCandidate(await query.releaseGetCandidate({ candidateId }, { signal }), candidateId) : null, visible, query);
  const targets = useReleaseRead(`journey-targets:${candidateId}`, available && candidateId ? async signal => readTargets(await query.releaseCandidateTargets({ candidateId }, { signal }), candidateId) : null, visible, query);
  const pipeline = useReleaseRead(`journey-pipeline:${build?.pipelineId ?? ""}`, available && build ? async signal => {
    const result = await query.pipelineForOwner({ pipelineId: build.pipelineId }, { signal });
    return result.rows().map(pipelineFromRow).find(row => row.id === build.pipelineId && row.repository === build.repository) ?? null;
  } : null, visible, query, 0);
  const [chosenStep, setStep] = useState<JourneyStep | null>(null);
  const [confirmation, confirm] = useState<"start" | "approve" | "publish" | null>(null);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const executing = useRef(false), mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const next = build ? currentStep(build) : "version";
  const open = chosenStep ?? next;
  // Set the first visible step once. Background progress never takes the
  // reader away from the step they are reading; an explicit action may advance.
  useEffect(() => { if (build && chosenStep === null) setStep(next); }, [build, chosenStep, next]);
  const allowed = (action: BuildAction) => available && intent.fresh && !!build?.actions.includes(action);
  const exactCandidate = build && candidate.value && candidate.value.manifest.components.length === 1 && candidate.value.manifest.components.every(c => c.name === build.component && c.repository === build.repository && c.commit === build.sha && c.version === build.version);
  const reviewed = candidate.fresh && targets.fresh && exactCandidate && candidate.value!.manifest.destinations.every(d => matchingTarget(d, targets.value));
  const sealed = intent.fresh && exactCandidate && build?.phase === "published" && build.catalog.state === "sealed" && candidate.fresh && candidate.value?.catalog?.state === "sealed" && candidate.value.approvalId === build.approvalId && candidate.value.catalog.catalogDigest === build.catalog.digest;
  useEffect(() => { confirm(null); }, [build?.candidateId, build?.approvalId, build?.configuration, build?.sha]);
  function reread() { intent.retry(); candidate.retry(); targets.retry(); pipeline.retry(); }
  function checkAgain() { setError(""); confirm(null); reread(); }
  async function act(kind: "start" | "resume" | "approve" | "publish") {
    if (executing.current || !query || !build || !allowed(kind)) return;
    if (kind === "approve" && (!reviewed || candidate.value?.state !== "ready")) return;
    if (kind === "publish" && (!reviewed || candidate.value?.state !== "approved" || !build.approvalId || candidate.value.approvalId !== build.approvalId)) return;
    executing.current = true; setBusy(kind); setError(""); confirm(null);
    try {
      if (kind === "approve") {
        await query.releaseApproveCandidate({ candidateId: build.candidateId });
        if (mounted.current) setStep("publish");
      }
      else {
        const result = kind === "start" ? await buildCall(query, "releaseStartBuild", { intentId }) : kind === "resume" ? await buildCall(query, "releaseResumeBuild", { intentId }) : await buildCall(query, "releasePublishBuild", { intentId, approvalId: build.approvalId });
        const updated = readReleaseBuild(result, intentId);
        if (mounted.current) setStep(currentStep(updated));
      }
    } catch (e) {
      if (mounted.current) setError(e instanceof Error ? e.message : String(e));
    } finally { executing.current = false; if (mounted.current) { setBusy(""); reread(); } }
  }
  function state(step: JourneyStep): StopState {
    if (!build) return step === "release" || step === "source" ? "unknown" : "ahead";
    if (sealed) return "done";
    const at = order.indexOf(next), index = order.indexOf(step);
    if (index < at) return "done";
    if (index > at) return "ahead";
    if (build.phase === "blocked" || build.phase === "uncertain") return "stopped";
    return busy || ["tagging", "building", "publishing"].includes(build.phase) ? "current" : "open";
  }
  const steps: Stop[] = [
    { id: "release", name: "Release", sentence: "Choose what to release", answer: build?.profileName, state: state("release"), body: build ? <Facts><Fact label="Release" value={build.profileName} /><Fact label="Component" value={build.component} /></Facts> : null },
    { id: "source", name: "Source", sentence: "Check the reviewed source", answer: build?.repository, state: state("source"), body: build ? <><Facts><Fact label="Repository" value={build.repository} copyValue={build.repository} /><Fact label="Recorded" value={formatMoment(build.createdAt)} copyValue={build.createdAt} /></Facts><Caption>This release keeps its recorded source. A newer change on the repository does not replace it.</Caption><References build={build} /></> : null },
    { id: "version", name: "Version", sentence: "Confirm the release version", answer: build?.version || undefined, state: state("version"), body: build ? <>
      <Facts><Fact label="Version" value={build.version || "Unavailable"} /><Fact label="Tag" value={build.tag || "Unavailable"} copyValue={build.tag || undefined} /></Facts>
      {build.reasonCode === "version_prepare_required" ? <Caption>Prepare a normal version change in this repository, have it reviewed and merged, then check the new source in a new release request. The existing tag will not be moved.</Caption> : <Caption>Starting the build creates or verifies the tag at this source revision and runs the full release checks. It does not approve, publish or install a release.</Caption>}
      <References build={build} />
    </> : null },
    { id: "build", name: "Build", sentence: "Build and verify artifacts", state: state("build"), body: build ? <>
      <BuildProgress build={build} query={query} available={available} visible={visible && open === "build"} runs={runs} runsFresh={runsFresh} />
      {build.phase === "built" ? <Caption>The build finished. Prepare the candidate to verify its artifacts and resolve the configured publication destinations.</Caption> : null}
      {!build.pipelineRunId ? <Caption>The build has not been confirmed yet. Check this release request before starting any other build.</Caption> : null}
      <References build={build} />
    </> : null },
    { id: "review", name: "Review", sentence: "Approve the exact release", state: state("review"), body: candidate.value ? <><ReviewDetails record={candidate.value} targets={targets.value} history={null} drafts={null} open={() => onCandidate(candidateId)} /><Caption>Approval covers these exact artifacts and destinations. Publication remains a separate action.</Caption></> : <ContentSkeleton label="Reading the release candidate" /> },
    { id: "publish", name: "Publish", sentence: "Make the approved release available", state: state("publish"), body: build ? <>
      <Caption>Publish the approved artifacts to their configured destinations, make file releases public and record the signed release catalog. This does not update an installation.</Caption>
      <RecordList as="ul" label="Publication destinations">{targets.value?.map(target => <RecordRow key={target.targetId} name={`${target.component} / ${target.artifact}`} secondary={`${target.origin}/${target.repository}`}><span>{target.tag ?? target.kind}</span></RecordRow>)}</RecordList>
      {build.phase === "publishing" || build.phase === "uncertain" ? <Caption>Any interrupted publication keeps its original saved attempt. Checking progress does not send it again.</Caption> : null}
      <References build={build} />
    </> : null },
    { id: "catalog", name: "Catalog", sentence: "Verify the signed release record", state: state("catalog"), body: build ? <>
      <Subhead>{sealed ? "Release available" : "Catalog verification"}</Subhead>
      <Caption>{sealed ? `${buildTitle(build)} is published and its signed evidence is in the release catalog. Choose an installation separately in Cluster Updates.` : build.catalog.state === "unavailable" ? "Catalog status is unavailable. This does not establish whether publication or catalog signing completed." : "The release is not complete until its signed catalog record is confirmed."}</Caption>
      <References build={build} />
    </> : null },
  ];
  const acts: Act[] = [{ label: sealed ? "Done" : "Leave", text: !busy, onAct: back }];
  let consequence: string | undefined;
  if (!busy) {
    if (!intent.fresh || intent.error || candidate.error || targets.error || error) acts.push({ label: "Read status again", onAct: checkAgain });
    else if (open === "version" && allowed("start")) {
      consequence = confirmation === "start" ? `Create or verify ${build!.tag} at the reviewed source and start its full build. Publication and installation remain separate.` : undefined;
      acts.push({ label: "Start release build", tone: "primary", onAct: () => confirmation === "start" ? void act("start") : confirm("start") });
    } else if (allowed("resume") && open === next) {
      acts.push({ label: build!.phase === "built" ? "Prepare candidate" : "Check interrupted release", tone: "primary", onAct: () => void act("resume") });
    } else if (open === "review" && allowed("approve") && reviewed && candidate.value?.state === "ready") {
      consequence = confirmation === "approve" ? "Approve these exact versions, verified artifacts and publication destinations. This does not publish or install them." : undefined;
      acts.push({ label: "Approve release", tone: "primary", onAct: () => confirmation === "approve" ? void act("approve") : confirm("approve") });
    } else if (open === "publish" && allowed("publish") && reviewed && candidate.value?.state === "approved" && candidate.value.approvalId === build!.approvalId) {
      consequence = confirmation === "publish" ? "Publish the approved artifacts, make file releases public and sign their catalog evidence. No installation will change." : undefined;
      acts.push({ label: "Publish release", tone: "primary", onAct: () => confirmation === "publish" ? void act("publish") : confirm("publish") });
    } else if (sealed && onUpdates) acts.push({ label: "Review installation choices", tone: "primary", onAct: onUpdates });
    else if (open === "version" && build?.reasonCode === "version_prepare_required" && pipeline.fresh && pipeline.value?.packageId && onOpenRepository) acts.push({ label: "Open repository", onAct: () => onOpenRepository(pipeline.value!.packageId) });
    else if (open === "build" && build?.pipelineRunId && onOpenRun) acts.push({ label: "Open build details", onAct: () => onOpenRun(build.pipelineRunId) });
    if (confirmation) acts[0] = { label: "Cancel", text: true, onAct: () => confirm(null) };
  }
  return <Wizard icon={<PackageCheck size={26} />} title="Cut release" label="Cut release steps" back={{ label: "Releases", onSelect: back }} steps={steps} open={open} onOpen={id => { setStep(id as JourneyStep); confirm(null); }} acts={acts}
    status={{ word: busy ? busy === "approve" ? "Recording approval" : busy === "publish" ? "Publishing release" : busy === "start" ? "Starting release build" : "Checking interrupted release" : !available ? "Disconnected" : !intent.fresh ? "Release status unavailable" : build ? sealed ? "In release catalog" : buildWord(build) : "Release unavailable", detail: consequence, tone: busy || (intent.fresh && build && ["tagging", "building", "publishing"].includes(build.phase)) ? "busy" : "none" }}
    notices={<>{!available ? <Notice sentence="Disconnected. The saved release is kept; no action is repeated on reconnect." /> : null}{!build && available && !intent.error ? <ContentSkeleton label="Reading release progress" /> : null}{intent.error || candidate.error || targets.error ? <Notice tone="error" sentence={intent.error || candidate.error || targets.error} /> : null}{build?.reason ? <Notice tone={build.phase === "blocked" || build.phase === "uncertain" ? "warn" : "info"} sentence={build.reason} /> : null}{candidate.value && !exactCandidate ? <Notice tone="error" sentence="The release candidate does not match this request’s source and version." /> : null}{error ? <Notice tone="error" sentence={error} next="The request may have completed. Read its status before trying again." /> : null}</>}
  />;
}

function References({ build }: { build: ReleaseBuild }) {
  return <details className="nexus-technical"><summary>Release references</summary><Facts>
    <Fact label="Request" value={build.intentId} copyValue={build.intentId} />
    {build.sha ? <Fact label="Revision" value={build.sha} copyValue={build.sha} /> : null}
    {build.pipelineRunId ? <Fact label="Build" value={build.pipelineRunId} copyValue={build.pipelineRunId} /> : null}
    {build.candidateId ? <Fact label="Candidate" value={build.candidateId} copyValue={build.candidateId} /> : null}
    {build.catalog.digest ? <Fact label="Catalog" value={build.catalog.digest} copyValue={build.catalog.digest} /> : null}
  </Facts></details>;
}

function BuildProgress({ build, query, available, visible, runs, runsFresh }: { build: ReleaseBuild; query: ReleaseQueries | null; available: boolean; visible: boolean; runs: readonly RunRow[]; runsFresh: boolean }) {
  const live = runs.find(run => run.id === build.pipelineRunId);
  const direct = useReleaseRead(`release-run:${build.pipelineRunId}`, query && available && build.pipelineRunId && !live ? async signal => {
    const result = await query.pipelineRunForOwner({ runId: build.pipelineRunId }, { signal });
    return result.rows().map(runFromRow).find(run => run.id === build.pipelineRunId) ?? null;
  } : null, visible, query, 0);
  const run = live ?? direct.value;
  const matches = run && run.pipelineId === build.pipelineId && run.repository === build.repository && run.sha === build.sha && run.event === "release" && run.mode === "full" && run.version === build.tag && (!build.workRunId || run.workRunId === build.workRunId);
  if (run && !matches) return <Notice tone="error" sentence="The build details do not match this release request." />;
  if (direct.error) return <Notice tone="error" sentence={direct.error} />;
  if (!run) return build.pipelineRunId ? <Caption>Build details are not available yet. The saved release request remains unchanged.</Caption> : null;
  const outcome = runOutcome(run);
  return <><Subhead>{run.title || "Release build"}</Subhead><Facts><Fact label="Status" value={outcome.word} /><Fact label="Progress" value={outcome.detail || "No stage detail recorded"} /></Facts>{!(live ? runsFresh : direct.fresh) ? <Caption>Last recorded build details. Current progress has not been confirmed.</Caption> : null}<RecordList as="ul" label="Build stages">{run.stages.map(stage => <RecordRow key={stage.name} name={stage.name} state={stageWord(stage.status)} />)}</RecordList></>;
}
