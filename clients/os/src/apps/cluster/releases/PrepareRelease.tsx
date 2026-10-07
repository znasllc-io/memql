import { useEffect, useRef, useState } from "react";
import { PackageCheck } from "lucide-react";
import { Button, Caption, ContentSkeleton, Fact, Facts, Notice, Panel, RecordList, RecordRow, Subhead } from "../../../kit";
import { Wizard } from "../../../kit/Wizard";
import type { Stop } from "../../../kit/Rail";
import { formatMoment } from "../../../kit/format";
import type { Act } from "../../../kit/ActionBar";
import { body, readCandidate } from "./model";
import { readAssemblies, readBuildRuns, runFits, type AssemblyPlan, type BuildRun } from "./assembly";
import { useReleaseRead } from "./useReleaseRead";
import type { ReleaseQueries } from "./ReleasesSection";

export function PrepareRelease({ query, connected, visible, back, prepared }: { query: ReleaseQueries | null; connected: boolean; visible: boolean; back: () => void; prepared: (id: string) => void }) {
  const available = query !== null && connected;
  const config = useReleaseRead("assembly", available ? async (signal) => readAssemblies(await query.releaseCandidateConfiguration({}, { signal })) : null, visible, query);
  const [plan, setPlan] = useState<AssemblyPlan | null>(null);
  const [selections, select] = useState<Record<string, BuildRun>>({});
  const [open, setOpen] = useState("plan");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const executing = useRef(false), mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const inputs = plan?.components.flatMap((c) => c.runs.map((alias) => ({ alias, component: c.name, repository: c.repository }))) ?? [];
  const current = plan && config.fresh && config.value?.some((p) => p.name === plan.name && p.configuration === plan.configuration);
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
      const result = await query.releaseAssembleCandidate({ planName: plan.name, runs });
      const id = body(result).candidateId;
      if (typeof id !== "string" || !/^sha256:[a-f0-9]{64}$/.test(id)) throw new Error("The cluster did not return a release identity.");
      readCandidate(result, id);
      if (mounted.current) prepared(id);
    } catch (e) {
      if (mounted.current) setError(e instanceof Error ? e.message : String(e));
    } finally { executing.current = false; if (mounted.current) setBusy(false); }
  };
  const steps: Stop[] = [{ id: "plan", name: "Release plan", state: plan ? "done" : "open", answer: plan?.name, sentence: "Choose the components and destinations configured for this release.", body: <>
    {config.value === null && available && !config.error ? <ContentSkeleton label="Loading release plans" /> : null}
    {config.fresh && config.value?.length === 0 ? <Caption>No release plans are configured. A cluster operator must configure an assembly before preparing a release.</Caption> : null}
    <RecordList as="ul" label="Release plans">{config.value?.map((p) => <RecordRow key={p.name} name={p.name} secondary={p.components.map((c) => c.name).join(", ")} onOpen={!busy && config.fresh ? () => { setPlan(p); select({}); setError(""); setOpen(`run:${p.components[0]!.runs[0]!}`); } : undefined} label={`Choose plan ${p.name}`} />)}</RecordList>
  </> }, ...inputs.map(({ alias, component, repository }, index): Stop => ({ id: `run:${alias}`, name: `${component} / ${alias}`, answer: selections[alias]?.title || selections[alias]?.id, state: selections[alias] ? "done" : inputs.slice(0, index).every((i) => selections[i.alias]) ? "open" : "ahead", sentence: `Choose a successful run from ${repository}. Inputs for this component must use the same commit.`, body: plan ? <BuildPicker key={alias} alias={alias} plan={plan} selections={selections} query={query} available={available && !busy && !!current} visible={visible && open === `run:${alias}`} choose={(run) => choose(alias, run)} /> : null })), {
    id: "review", name: "Review inputs", state: complete ? "open" : "ahead", sentence: "MemQL will verify every selected run, artifact and destination before preparing the candidate.", body: plan ? <>
      <Panel label="Selected runs"><Subhead>Selected runs</Subhead><RecordList as="ul">{inputs.map(({ alias, component }) => <RecordRow key={alias} name={`${component} / ${alias}`} secondary={selections[alias]?.title || selections[alias]?.id}><span className="os-release-identity">{selections[alias]?.commit}</span></RecordRow>)}</RecordList></Panel>
      <Panel label="Planned artifacts"><Subhead>Artifacts</Subhead><RecordList as="ul">{plan.components.flatMap((c) => c.artifacts.map((a) => <RecordRow key={`${c.name}/${a.name}`} name={`${c.name} / ${a.name}`} secondary={`${a.platform} · ${a.run} · ${a.stepKey}`}><span className="os-release-identity">{a.path}</span></RecordRow>))}</RecordList></Panel>
      <Panel label="Planned destinations"><Subhead>Destinations</Subhead><RecordList as="ul">{plan.targets.map((t) => <RecordRow key={t.targetId} name={t.targetId} secondary={`${t.origin}/${t.repository}`}><span>{t.tag ?? t.kind}</span></RecordRow>)}</RecordList></Panel>
      {plan.compatibility.length ? <Panel label="Required compatibility"><Subhead>Compatibility</Subhead><Facts>{plan.compatibility.map((r) => <Fact key={`${r.component}/${r.requires}`} label={`${r.component} requires ${r.requires}`} value={`${r.minVersion} to below ${r.maxExclusive}`} />)}</Facts></Panel> : null}
      <Caption>Preparation does not approve or publish a release. Review the resolved versions and verified artifacts on the next screen.</Caption>
    </> : null,
  }];
  const acts: Act[] = busy ? [] : [{ label: "Cancel", text: true, onAct: back }];
  if (open === "review" && complete && current && !busy && available) acts.push({ label: "Prepare release", tone: "primary", onAct: () => void prepare() });
  return <Wizard icon={<PackageCheck size={26} />} title="Prepare release" label="Prepare release steps" back={{ label: "Releases", onSelect: back }} steps={steps} open={open} onOpen={(step) => { if (!busy) setOpen(step); }} acts={acts}
    status={{ word: busy ? "Verifying release inputs" : !available ? "Disconnected" : !config.fresh ? "Release plans unavailable" : plan && !current ? "Release plan changed" : complete ? "Ready to prepare" : "Choose release inputs", tone: busy ? "busy" : "none", detail: busy ? "The cluster is checking run evidence and artifact bytes." : undefined }}
    notices={<>{!available ? <Notice sentence="Disconnected. Your selections are kept until the cluster reconnects." /> : null}{config.error ? <Notice tone="error" sentence={config.error}><Button onClick={config.retry}>Try again</Button></Notice> : null}{plan && config.fresh && !current ? <Notice tone="warn" sentence="This release plan changed. Choose the plan again to review its current inputs and destinations." /> : null}{error ? <Notice tone="error" sentence={error} next="The cluster may have prepared a candidate before the reply was lost. Check Releases before preparing again." /> : null}</>}
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
    <RecordList as="ul" label={`Successful runs for ${alias}`}>{runs.map((run) => <RecordRow key={run.id} name={run.title || run.id} secondary={`${formatMoment(run.finishedAt)} · ${run.commit}`} onOpen={page.fresh ? () => choose(run) : undefined} label={`Use run ${run.id}`} />)}</RecordList>
    <div className="os-panel-actions">{cursors.length > 1 ? <Button onClick={() => setCursors((old) => old.slice(0, -1))}>Newer runs</Button> : null}{page.fresh && page.value?.nextCursor ? <Button onClick={() => setCursors((old) => [...old, page.value!.nextCursor])}>Older runs</Button> : null}</div>
  </>;
}
