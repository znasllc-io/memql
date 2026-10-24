import { useEffect, useRef, useState } from "react";
import { PackageCheck } from "lucide-react";
import { Button, Caption, ContentSkeleton, Fact, Facts, Notice, Panel, RecordList, RecordRow, Subhead } from "../../../kit";
import { Wizard } from "../../../kit/Wizard";
import type { Stop } from "../../../kit/Rail";
import { formatMoment } from "../../../kit/format";
import type { Act } from "../../../kit/ActionBar";
import { body, readCandidate } from "./model";
import { planKey, readAssemblies, readProfiles, readBuildRuns, runFits, type AssemblyPlan, type BuildRun } from "./assembly";
import { useReleaseRead } from "./useReleaseRead";
import type { ReleaseQueries } from "./ReleasesSection";

export function PrepareRelease({ query, connected, visible, back, prepared }: { query: ReleaseQueries | null; connected: boolean; visible: boolean; back: () => void; prepared: (id: string) => void }) {
  const available = query !== null && connected;
  const config = useReleaseRead("assembly", available ? async (signal) => readAssemblies(await query.releaseCandidateConfiguration({}, { signal })) : null, visible, query);
  const profiles = useReleaseRead("profiles", available ? async (signal) => readProfiles(await query.releaseCandidateProfiles({}, { signal })) : null, visible, query);
  const [plan, setPlan] = useState<AssemblyPlan | null>(null);
  const [selections, select] = useState<Record<string, BuildRun>>({});
  const [open, setOpen] = useState("plan");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const executing = useRef(false), mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const inputs = plan?.components.flatMap((c) => c.runs.map((alias) => ({ alias, component: c.name, repository: c.repository }))) ?? [];
  const selectedConfig = plan?.kind === "profile" ? profiles : config;
  const plans = [...(profiles.value ?? []), ...(config.value ?? [])];
  const current = plan && selectedConfig.fresh && selectedConfig.value?.some((p) => planKey(p) === planKey(plan) && p.configuration === plan.configuration);
  const complete = plan && inputs.every(({ alias }) => selections[alias] && runFits(plan, alias, selections[alias]!, selections));
  const choose = (alias: string, run: BuildRun) => {
    if (!plan || busy || !current || !runFits(plan, alias, run, selections)) return;
    const next = { ...selections, [alias]: run };
    select(next); setError("");
    const remaining = inputs.find((i) => !next[i.alias]);
    setOpen(remaining ? `run:${remaining.alias}` : "review");
  };
  const prepare = async () => {
    if (executing.current || !query || !available || !current || !complete || !plan) return;
    executing.current = true; setBusy(true); setError("");
    try {
      const runs = Object.fromEntries(inputs.map(({ alias }) => [alias, selections[alias]!.workRunId]));
      const result = plan.kind === "profile"
        ? await query.releaseAssembleProfileCandidate({ profileName: plan.name, expectedConfiguration: plan.configuration, runs })
        : await query.releaseAssembleCandidate({ planName: plan.name, runs });
      const id = body(result).candidateId;
      if (typeof id !== "string" || !/^sha256:[a-f0-9]{64}$/.test(id)) throw new Error("The cluster did not return a release identity.");
      readCandidate(result, id);
      if (mounted.current) prepared(id);
    } catch (e) {
      if (mounted.current) setError(e instanceof Error ? e.message : String(e));
    } finally { executing.current = false; if (mounted.current) setBusy(false); }
  };
  const steps: Stop[] = [{ id: "plan", name: "Release plan", state: plan ? "done" : "open", answer: plan?.name, sentence: "Components and destinations", body: <>
    {config.value === null && profiles.value === null && available && (!config.error || !profiles.error) ? <ContentSkeleton label="Loading release plans" /> : null}
    {config.fresh && profiles.fresh && plans.length === 0 ? <Caption>No release plans are configured. A cluster operator must configure a release profile or assembly before preparing a release.</Caption> : null}
    <RecordList as="ul" label="Release plans">{plans.map((p) => <RecordRow key={planKey(p)} name={p.name} secondary={`${p.components.map((c) => c.name).join(", ")}${p.kind === "profile" ? " · From release run" : ""}`} onOpen={!busy && (p.kind === "profile" ? profiles.fresh : config.fresh) ? () => { setPlan(p); select({}); setError(""); setOpen(`run:${p.components[0]!.runs[0]!}`); } : undefined} label={`Choose ${p.kind === "profile" ? "profile" : "plan"} ${p.name}`} />)}</RecordList>
  </> }, ...inputs.map(({ alias, component, repository }, index): Stop => ({ id: `run:${alias}`, name: `${component} / ${alias}`, answer: selections[alias] ? selections[alias]!.title || "Completed run" : undefined, state: selections[alias] ? "done" : inputs.slice(0, index).every((i) => selections[i.alias]) ? "open" : "ahead", sentence: `A successful run from ${repository}`, body: plan ? <><Caption>Every input for this component must come from the same commit.</Caption><BuildPicker key={alias} alias={alias} plan={plan} selections={selections} query={query} available={available && !busy && !!current} visible={visible && open === `run:${alias}`} choose={(run) => choose(alias, run)} /></> : null })), {
    id: "review", name: "Review inputs", state: complete ? "open" : "ahead", sentence: "Verified before it is prepared", body: plan ? <>
      <Panel label="Selected runs"><Subhead>Selected runs</Subhead><RecordList as="ul">{inputs.map(({ alias, component }) => <RecordRow key={alias} name={selections[alias]?.title || "Completed run"} secondary={`${component} / ${alias}`} />)}</RecordList></Panel>
      <details className="nexus-technical"><summary>Source revisions</summary><Facts>{inputs.map(({ alias, component }) => <Fact key={alias} label={`${component} / ${alias}`} value={selections[alias]?.commit} copyValue={selections[alias]?.commit} />)}</Facts></details>
      {plan.kind === "profile" ? <Caption>The installed release recipe resolves versions, artifacts and configured destinations from these runs. You will review the exact result before approving it.</Caption> : <>
        <Panel label="Planned artifacts"><Subhead>Artifacts</Subhead><RecordList as="ul">{plan.components.flatMap((c) => c.artifacts.map((a) => <RecordRow key={`${c.name}/${a.name}`} name={`${c.name} / ${a.name}`} secondary={`${a.platform} · ${a.run} · ${a.stepKey}`}><span className="os-release-identity">{a.path}</span></RecordRow>))}</RecordList></Panel>
        <Panel label="Planned destinations"><Subhead>Destinations</Subhead><RecordList as="ul">{plan.targets.map((t) => <RecordRow key={t.targetId} name={t.targetId} secondary={`${t.origin}/${t.repository}`}><span>{t.tag ?? t.kind}</span></RecordRow>)}</RecordList></Panel>
      </>}
      {plan.compatibility.length ? <Panel label="Required compatibility"><Subhead>Compatibility</Subhead><Facts>{plan.compatibility.map((r) => <Fact key={`${r.component}/${r.requires}`} label={`${r.component} requires ${r.requires}`} value={`${r.minVersion} to below ${r.maxExclusive}`} />)}</Facts></Panel> : null}
      <Caption>Preparation does not approve or publish a release. Review the resolved versions and verified artifacts on the next screen.</Caption>
    </> : null,
  }];
  const acts: Act[] = busy ? [] : [{ label: "Cancel", text: true, onAct: back }];
  if (open === "review" && complete && current && !busy && available) acts.push({ label: "Prepare release", tone: "primary", onAct: () => void prepare() });
  return <Wizard icon={<PackageCheck size={26} />} title="Prepare release" label="Prepare release steps" back={{ label: "Releases", onSelect: back }} steps={steps} open={open} onOpen={(step) => { if (!busy) setOpen(step); }} acts={acts}
    status={{ word: busy ? "Verifying release inputs" : !available ? "Disconnected" : plan && !selectedConfig.fresh ? "Release plan unavailable" : plan && !current ? "Release plan changed" : complete ? "Ready to prepare" : "Choose release inputs", tone: busy ? "busy" : "none", detail: busy ? "The cluster is checking run evidence and artifact bytes." : undefined }}
    notices={<>{!available ? <Notice sentence="Disconnected. Your selections are kept until the cluster reconnects." /> : null}{config.error ? <Notice tone="error" sentence={config.error}><Button onClick={config.retry}>Read assembly plans again</Button></Notice> : null}{profiles.error ? <Notice tone="error" sentence={profiles.error}><Button onClick={profiles.retry}>Read release profiles again</Button></Notice> : null}{plan && selectedConfig.fresh && !current ? <Notice tone="warn" sentence="This release plan changed. Choose the plan again to review its current inputs and destinations." /> : null}{error ? <Notice tone="error" sentence={error} next="The cluster may have prepared a candidate before the reply was lost. Check Releases before preparing again." /> : null}</>}
  />;
}

function BuildPicker({ alias, plan, selections, query, available, visible, choose }: { alias: string; plan: AssemblyPlan; selections: Record<string, BuildRun>; query: ReleaseQueries | null; available: boolean; visible: boolean; choose: (run: BuildRun) => void }) {
  const [cursors, setCursors] = useState<string[]>([""]);
  const cursor = cursors[cursors.length - 1]!;
  const page = useReleaseRead(`builds:${cursor}`, available && query ? async (signal) => readBuildRuns(await query.pipelineRunsForOwner({}, { signal, cursor })) : null, visible, query);
  const runs = page.value?.runs.filter((run) => runFits(plan, alias, run, selections)) ?? [];
  return <>
    {page.value === null && available && !page.error ? <ContentSkeleton label="Loading completed runs" /> : null}
    {page.error ? <Notice tone="error" sentence={page.error}><Button onClick={page.retry}>Try again</Button></Notice> : null}
    {page.fresh && runs.length === 0 ? <Caption>No matching successful runs on this page.</Caption> : null}
    <RecordList as="ul" label={`Successful runs for ${alias}`}>{runs.map((run) => <li key={run.id}>
      <RecordRow name={run.title || "Completed run"} secondary={`${formatMoment(run.finishedAt)}${run.releaseTag ? ` · ${run.releaseTag}` : ""}`} onOpen={page.fresh ? () => choose(run) : undefined} label={`Use run: ${run.title || "Completed run"}, ${formatMoment(run.finishedAt)}${run.releaseTag ? `, ${run.releaseTag}` : ""}`} />
      <details className="nexus-technical"><summary>Source revision</summary><Facts><Fact label="Run" value={run.id} copyValue={run.id} /><Fact label="Repository" value={run.repository} copyValue={run.repository} /><Fact label="Revision" value={run.commit} copyValue={run.commit} /></Facts></details>
    </li>)}</RecordList>
    <div className="os-panel-actions">{cursors.length > 1 ? <Button onClick={() => setCursors((old) => old.slice(0, -1))}>Newer runs</Button> : null}{page.fresh && page.value?.nextCursor ? <Button onClick={() => setCursors((old) => [...old, page.value!.nextCursor])}>Older runs</Button> : null}</div>
  </>;
}
