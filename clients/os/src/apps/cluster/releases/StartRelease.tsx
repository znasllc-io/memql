import { useEffect, useRef, useState } from "react";
import { PackageCheck } from "lucide-react";
import { Button, Caption, ContentSkeleton, Notice, RecordList, RecordRow } from "../../../kit";
import { Wizard } from "../../../kit/Wizard";
import type { Stop } from "../../../kit/Rail";
import type { Act } from "../../../kit/ActionBar";
import { pipelineFromRow, type PipelineRow } from "../../deployables/pipelines/rows";
import { readProfiles, type AssemblyPlan } from "./assembly";
import { buildCall, readReleaseBuild } from "./journey";
import type { ReleaseQueries } from "./ReleasesSection";
import { useReleaseRead } from "./useReleaseRead";

export function StartRelease({ query, connected, visible, back, prepared }: { query: ReleaseQueries | null; connected: boolean; visible: boolean; back: () => void; prepared: (id: string) => void }) {
  const available = connected && query !== null;
  const profiles = useReleaseRead("start-profiles", available ? async signal => readProfiles(await query.releaseCandidateProfiles({}, { signal })).filter(p => p.build) : null, visible, query);
  const [profile, selectProfile] = useState<AssemblyPlan | null>(null);
  const [pipeline, selectPipeline] = useState<PipelineRow | null>(null);
  const [open, setOpen] = useState("release");
  const [busy, setBusy] = useState(false), [error, setError] = useState("");
  const [uncertain, setUncertain] = useState(false);
  const inFlight = useRef(false), mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const current = profile && profiles.fresh && profiles.value?.some(p => p.name === profile.name && p.configuration === profile.configuration);
  async function prepare() {
    if (inFlight.current || uncertain || !available || !current || !profile || !pipeline || !query) return;
    inFlight.current = true; setBusy(true); setError("");
    try {
      const intent = readReleaseBuild(await buildCall(query, "releasePrepareBuild", { profileName: profile.name, pipelineId: pipeline.id, expectedConfiguration: profile.configuration }));
      if (intent.profileName !== profile.name || intent.configuration !== profile.configuration || intent.pipelineId !== pipeline.id || intent.repository !== pipeline.repository) throw new Error("The cluster returned a different release source.");
      if (mounted.current) prepared(intent.intentId);
    } catch (e) {
      if (mounted.current) { setError(e instanceof Error ? e.message : String(e)); setUncertain(true); }
    } finally { inFlight.current = false; if (mounted.current) setBusy(false); }
  }
  const steps: Stop[] = [
    { id: "release", name: "Release", sentence: "Choose what to release", answer: profile?.name, state: profile ? "done" : "open", body: <>
      {!profiles.value && available && !profiles.error ? <ContentSkeleton label="Reading release profiles" /> : null}
      {profiles.fresh && profiles.value?.length === 0 ? <Caption>No release build is configured. An operator must connect a release profile to its build and publication recipes.</Caption> : null}
      <RecordList as="ul" label="Release profiles">{profiles.value?.map(p => <RecordRow key={p.name} name={p.name} secondary={p.components[0]!.repository} onOpen={profiles.fresh && !busy && !uncertain ? () => { selectProfile(p); selectPipeline(null); setOpen("source"); } : undefined} label={`Release ${p.name}`} />)}</RecordList>
    </> },
    { id: "source", name: "Source", sentence: "Check the reviewed source", answer: pipeline?.name, state: profile ? "open" : "ahead", body: profile ? <>
      <Caption>The cluster checks {profile.build!.branch} and its committed version before a build can start. Checking the source saves a release request; it does not create a tag, start a build or publish anything.</Caption>
      <SourcePicker key={profile.name} profile={profile} query={query} available={available && !!current && !busy && !uncertain} visible={visible && open === "source"} chosen={pipeline?.id ?? ""} choose={selectPipeline} />
    </> : null },
    { id: "version", name: "Version", sentence: "Confirm the release version", state: "ahead" },
    { id: "build", name: "Build", sentence: "Build and verify artifacts", state: "ahead" },
    { id: "review", name: "Review", sentence: "Approve the exact release", state: "ahead" },
    { id: "publish", name: "Publish", sentence: "Make the approved release available", state: "ahead" },
    { id: "catalog", name: "Catalog", sentence: "Verify the signed release record", state: "ahead" },
  ];
  const acts: Act[] = [{ label: busy ? "Leave" : uncertain ? "Read release requests" : "Cancel", text: !busy, onAct: back }];
  if (open === "source" && pipeline && current && available && !busy && !uncertain) acts.push({ label: "Check source", tone: "primary", onAct: () => void prepare() });
  if (profiles.error && !busy) acts.push({ label: "Read profiles again", onAct: profiles.retry });
  return <Wizard icon={<PackageCheck size={26} />} title="Cut release" label="Cut release steps" back={{ label: "Releases", onSelect: back }} steps={steps} open={open} onOpen={id => { if (!busy && !uncertain) setOpen(id); }} acts={acts}
    status={{ word: busy ? "Checking release source" : uncertain ? "Release request not yet confirmed" : !available ? "Disconnected" : profile && profiles.fresh && !current ? "Release profile changed" : pipeline ? "Ready to check source" : profile ? "Choose a pipeline" : "Choose a release", tone: busy ? "busy" : "none" }}
    notices={<>{!available ? <Notice sentence="Disconnected. Your choices are kept until the connection returns." /> : null}{profiles.error ? <Notice tone="error" sentence={profiles.error} /> : null}{profile && profiles.fresh && !current ? <Notice tone="warn" sentence="This release profile changed. Choose it again before checking the source." /> : null}{error ? <Notice tone="error" sentence={error} next="The request may have been saved. Read release requests before starting another." /> : null}</>}
  />;
}

function SourcePicker({ profile, query, available, visible, chosen, choose }: { profile: AssemblyPlan; query: ReleaseQueries | null; available: boolean; visible: boolean; chosen: string; choose: (pipeline: PipelineRow) => void }) {
  const [cursors, setCursors] = useState([""]);
  const cursor = cursors[cursors.length - 1]!;
  const page = useReleaseRead(`release-pipelines:${cursor}`, query && available ? async signal => {
    const result = await query.pipelinesForOwner({}, { signal, cursor });
    return { rows: result.rows().map(pipelineFromRow).filter(p => p.repository === profile.components[0]!.repository), next: result.meta()?.cursor ?? "" };
  } : null, visible, query);
  // Choosing a source is not proof of write access: the native preparation checks
  // the pipeline's current owner grant and installation before offering Start.
  return <>
    {!page.value && available && !page.error ? <ContentSkeleton label="Reading connected pipelines" /> : null}
    {page.error ? <Notice tone="error" sentence={page.error} /> : null}
    {page.fresh && page.value?.rows.length === 0 ? <Caption>No matching connected pipeline is on this page. Connect this repository in Pipelines before starting a release.</Caption> : null}
    <RecordList as="ul" label="Release pipelines">{page.value?.rows.map(p => <RecordRow key={p.id} name={p.name || p.repository} secondary={p.repository} state={p.id === chosen ? "Selected" : p.status === "disconnected" ? "Automatic runs disconnected" : undefined} onOpen={page.fresh ? () => choose(p) : undefined} label={`Use pipeline ${p.name || p.repository}`} />)}</RecordList>
    <div className="os-panel-actions">{cursors.length > 1 ? <Button onClick={() => setCursors(v => v.slice(0, -1))}>Newer pipelines</Button> : null}{page.fresh && page.value?.next ? <Button onClick={() => setCursors(v => [...v, page.value!.next])}>Older pipelines</Button> : null}{page.error ? <Button onClick={page.retry}>Read pipelines again</Button> : null}</div>
  </>;
}
