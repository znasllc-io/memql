import { useCallback, useEffect, useRef, useState } from "react";
import type { QueryClient } from "@znasllc-io/memql-sdk-core/client";
import { useSession } from "../../../chrome/access";
import { useConnectionStatus } from "../../../chrome/connection";
import { useOsConnection } from "../../../live/connection";
import { Button, Caption, ContentSkeleton, Fact, Facts, Head, Notice, Panel, RecordList, RecordListSkeleton, RecordRow, Subhead } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import { formatMoment } from "../../../kit/format";
import { candidateTitle, destinationKey, matchingTarget, readCandidate, readPage, readPublications, readTargets, stateLabel, type Candidate, type Destination, type Publication, type Target } from "./model";
import { useReleaseRead } from "./useReleaseRead";
import "./releases.css";

export type ReleaseQueries = Pick<QueryClient, "releaseCandidates" | "releaseGetCandidate" | "releaseCandidatePublications" | "releaseCandidateConfiguration" | "releaseApproveCandidate" | "releaseRetireCandidate" | "releasePublishCandidate" | "releasePrepareCandidate">;

export function ReleasesSection({ visible = true }: { visible?: boolean }) {
  const connection = useOsConnection();
  const status = useConnectionStatus();
  const { access } = useSession();
  // The engine requires an owner, independently of any grant to open a section.
  if (access?.role !== "owner") return <div className="os-cluster"><Head title="Releases" /><Notice sentence="A cluster owner must review and publish releases." /></div>;
  return <ReleaseBrowser key={access.userId} query={connection?.query ?? null} connected={status === "connected"} visible={visible} />;
}

export function ReleaseBrowser({ query, connected, visible = true }: { query: ReleaseQueries | null; connected: boolean; visible?: boolean }) {
  const [selected, select] = useState("");
  const list = useRef<HTMLDivElement>(null);
  const previousSelection = useRef("");
  useEffect(() => {
    if (selected || !previousSelection.current) return;
    const item = Array.from(list.current?.querySelectorAll<HTMLElement>("[data-candidate-key]") ?? []).find((row) => row.dataset.candidateKey === previousSelection.current);
    item?.querySelector<HTMLButtonElement>("button")?.focus();
  }, [selected]);
  const [cursors, setCursors] = useState<string[]>([""]);
  const cursor = cursors[cursors.length - 1]!;
  const available = connected && query !== null;
  const page = useReleaseRead(`page:${cursor}`, available ? async (signal) => readPage(await query.releaseCandidates({ cursor, limit: 20 }, { signal })) : null, visible && !selected, query);
  if (selected) return <CandidatePage key={selected} candidateId={selected} query={query} connected={connected} visible={visible} back={() => select("")} />;
  return <div className="os-cluster os-release-list" ref={list}>
    <Head title="Releases" meta={page.fresh ? `${page.value?.candidates.length ?? 0} on this page` : undefined} />
    {!available ? <Notice sentence="Disconnected. Releases will update when the cluster reconnects." /> : null}
    <ReadFailure error={page.error} retry={page.retry} />
    {page.value === null && available && !page.error ? <RecordListSkeleton label="Loading releases" /> : null}
    {page.value?.candidates.length === 0 ? <Caption>No release candidates have been prepared for your review.</Caption> : null}
    <RecordList as="ul" label="Release candidates">
      {page.value?.candidates.map((c) => <li key={c.candidateId} data-candidate-key={c.candidateId}><RecordRow name={candidateTitle(c)} secondary={formatMoment(c.createdAt)} state={stateLabel[c.state]} tone={c.state === "preparing" ? "warn" : "muted"} onOpen={() => { previousSelection.current = c.candidateId; select(c.candidateId); }} label={`Review ${candidateTitle(c)}`} /></li>)}
    </RecordList>
    {cursors.length > 1 || page.value?.nextCursor ? <div className="os-panel-actions">
      {cursors.length > 1 ? <Button onClick={() => setCursors((old) => old.slice(0, -1))}>Newer</Button> : null}
      {page.fresh && page.value?.nextCursor ? <Button onClick={() => setCursors((old) => [...old, page.value!.nextCursor!])}>Older</Button> : null}
    </div> : null}
  </div>;
}

function CandidatePage({ candidateId, query, connected, visible, back }: { candidateId: string; query: ReleaseQueries | null; connected: boolean; visible: boolean; back: () => void }) {
  const available = connected && query !== null;
  const candidate = useReleaseRead(candidateId, available ? async (signal) => readCandidate(await query.releaseGetCandidate({ candidateId }, { signal }), candidateId) : null, visible, query);
  const history = useReleaseRead(candidateId, available ? async (signal) => readPublications(await query.releaseCandidatePublications({ candidateId }, { signal }), candidateId) : null, visible, query);
  const targets = useReleaseRead("targets", available ? async (signal) => readTargets(await query.releaseCandidateConfiguration({}, { signal })) : null, visible, query);
  const [destination, openDestination] = useState<string | null>(null);
  const content = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const heading = content.current?.querySelector<HTMLHeadingElement>("h3");
    if (heading) { heading.tabIndex = -1; heading.focus({ preventScroll: true }); }
    if (content.current) content.current.scrollTop = 0;
  }, [destination]);
  const [confirmation, confirm] = useState<"approve" | "retire" | "publish" | null>(null);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const executing = useRef(false);
  const mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const record = candidate.value;
  const selected = record?.manifest.destinations.find((d) => destinationKey(d) === destination);
  const publication = selected ? history.value?.find((p) => destinationKey(p.target) === destination) : undefined;
  const target = selected ? matchingTarget(selected, targets.value) : undefined;
  const settled = available && candidate.fresh && history.fresh;
  const configured = targets.fresh && record?.manifest.destinations.every((d) => matchingTarget(d, targets.value)) === true;
  const title = record ? candidateTitle(record.manifest) : "Release";
  const reread = useCallback(() => { candidate.retry(); history.retry(); targets.retry(); }, [candidate.retry, history.retry, targets.retry]);
  const run = async (kind: "approve" | "retire" | "publish" | "prepare") => {
    if (executing.current || !settled || !record || !query) return;
    executing.current = true;
    setBusy(kind === "publish" ? "Verifying publication" : kind === "approve" ? "Verifying approval" : kind === "prepare" ? "Verifying release" : "Retiring release");
    setError("");
    confirm(null);
    try {
      if (kind === "approve") await query.releaseApproveCandidate({ candidateId });
      else if (kind === "retire") await query.releaseRetireCandidate({ candidateId });
      else if (kind === "prepare") {
        // Preparation resumes the exact stored manifest, never a browser-edited recipe.
        const result = await query.releaseGetCandidate({ candidateId });
        const raw = result.single();
        if (raw?.candidateId !== candidateId || !raw.manifest || typeof raw.manifest !== "object" || Array.isArray(raw.manifest)) throw new Error("The cluster returned a different release.");
        await query.releasePrepareCandidate({ candidate: raw.manifest as Record<string, unknown> });
      } else {
        if (!selected || !record.approvalId || !target || !targets.fresh) throw new Error("The approved publication destination is unavailable.");
        await query.releasePublishCandidate({ candidateId, approvalId: record.approvalId, targetId: selected.targetId, component: selected.component, artifact: selected.artifact });
      }
    } catch (e) {
      if (mounted.current) setError(e instanceof Error ? e.message : String(e));
    } finally {
      executing.current = false;
      if (mounted.current) { setBusy(""); reread(); }
    }
  };
  const publishLabel = target?.kind === "file" ? "Upload file" : "Publish artifact";
  const acts: Act[] = [];
  let footer = busy || (record ? stateLabel[record.state] : "Release unavailable");
  let detail: string | undefined;
  if (!busy && settled && record) {
    if (selected) {
      footer = publication?.state === "complete" ? (publication.artifactKind === "file" ? "File uploaded and verified" : "Published and verified") : publication ? "Publication unresolved" : "No publication recorded";
      if (publication?.state === "complete") detail = "Verified when published; remote availability has not been checked again.";
      else if (record.state === "approved" && selected.operation === "publish" && target && targets.fresh) {
        if (confirmation === "publish") {
          detail = target.kind === "file" ? `Upload ${target.assetName} to draft ${target.repository} ${target.tag} (#${target.releaseId}). This does not make the release public.` : `Publish ${selected.component}/${selected.artifact} to ${target.origin}/${target.repository}.`;
          acts.push({ label: "Cancel", text: true, onAct: () => confirm(null) }, { label: publication ? "Reconcile publication" : publishLabel, tone: "primary", onAct: () => void run("publish") });
        } else acts.push({ label: publication ? "Reconcile publication" : publishLabel, tone: "primary", onAct: () => confirm("publish") });
      }
    } else if (confirmation === "retire" && history.value?.length === 0 && record.state !== "retired") {
      detail = "Retirement is permanent and releases this candidate’s artifact retention.";
      acts.push({ label: "Cancel", text: true, onAct: () => confirm(null) }, { label: "Retire release", tone: "danger", onAct: () => void run("retire") });
    } else if (confirmation === "approve" && record.state === "ready" && configured) {
      detail = "Approve these exact versions, checks, artifacts and destinations. Publication remains a separate action.";
      acts.push({ label: "Cancel", text: true, onAct: () => confirm(null) }, { label: "Approve release", tone: "primary", onAct: () => void run("approve") });
    } else {
      if (record.state !== "retired" && history.value?.length === 0) acts.push({ label: "Retire release", text: true, onAct: () => confirm("retire") });
      if (record.state === "preparing" && configured) acts.push({ label: "Resume verification", tone: "primary", onAct: () => void run("prepare") });
      if (record.state === "ready" && configured) acts.push({ label: "Approve release", tone: "primary", onAct: () => confirm("approve") });
    }
  }
  if (!settled && !busy) { footer = "Release state unavailable"; detail = "Actions return after the current state is read."; }
  return <div className="os-release-pane">
    <div className="os-release-scroll" ref={content}>
      <Head title={selected ? `${selected.component} / ${selected.artifact}` : title} back={{ label: selected ? title : "Releases", onSelect: () => { confirm(null); setError(""); if (selected) openDestination(null); else back(); } }} />
      {!available ? <Notice sentence="Disconnected. The last record is shown; actions are unavailable." /> : null}
      <ReadFailure error={candidate.error || history.error} retry={reread} />
      <ReadFailure error={targets.error} retry={targets.retry} />
      {error ? <Notice tone="error" sentence={error} next="An interrupted publication may already have reached its destination. Check the recorded state before trying again." /> : null}
      {!record && available && !candidate.error ? <ContentSkeleton label="Loading release" /> : null}
      {record && selected ? <PublicationDetails record={record} selected={selected} target={target} publication={publication} targetsKnown={targets.fresh} historyKnown={history.fresh} /> : record ? <ReviewDetails record={record} targets={targets.value} history={history.value} open={(d) => { confirm(null); setError(""); openDestination(destinationKey(d)); }} /> : null}
    </div>
    <ActionBar state={footer} detail={detail} tone={busy ? "busy" : "none"} acts={acts} live />
  </div>;
}

function ReviewDetails({ record, targets, history, open }: { record: Candidate; targets: Target[] | null; history: Publication[] | null; open: (d: Destination) => void }) {
  return <>
    <Panel label="Versions"><Subhead>Versions</Subhead><RecordList as="ul">
      {record.manifest.components.map((c) => <RecordRow key={c.name} name={`${c.name} ${c.version}`} secondary={c.repository}><span className="os-release-identity">{c.commit}</span></RecordRow>)}
    </RecordList></Panel>
    <Panel label="Destinations"><Subhead>Destinations</Subhead><RecordList as="ul">
      {record.manifest.destinations.map((d) => { const target = matchingTarget(d, targets); const p = history?.find((p) => destinationKey(p.target) === destinationKey(d)); return <RecordRow key={destinationKey(d)} name={`${d.component} / ${d.artifact}`} secondary={target ? `${target.origin}/${target.repository}` : d.targetId} state={p?.state === "complete" ? (p.artifactKind === "file" ? "Uploaded" : "Published") : p ? "Unresolved" : "Review"} tone={p?.state === "pending" ? "warn" : "muted"} onOpen={() => open(d)} label={`Review destination ${d.targetId}`} />; })}
    </RecordList></Panel>
    <Panel label="Verification"><Subhead meta={record.manifest.evidence.length}>Verification</Subhead><RecordList as="ul" density="compact">
      {record.manifest.evidence.map((e) => <RecordRow key={e.name} name={e.name} secondary={`${e.component}: ${e.stepKey}`}><span>Attempt {e.attempt}</span></RecordRow>)}
    </RecordList></Panel>
    {record.manifest.compatibility.length ? <Panel label="Compatibility"><Subhead>Compatibility</Subhead><RecordList as="ul">
      {record.manifest.compatibility.map((r) => <RecordRow key={`${r.component}/${r.requires}`} name={`${r.component} requires ${r.requires}`} secondary={`${r.minVersion} to below ${r.maxExclusive}`} />)}
    </RecordList></Panel> : null}
    <Panel label="Release identity"><Facts><Fact label="Candidate" value={<Identity value={record.candidateId} />} /></Facts></Panel>
  </>;
}
function PublicationDetails({ record, selected, target, publication, targetsKnown, historyKnown }: { record: Candidate; selected: Destination; target?: Target; publication?: Publication; targetsKnown: boolean; historyKnown: boolean }) {
  const component = record.manifest.components.find((c) => c.name === selected.component);
  const artifact = component?.artifacts.find((a) => a.name === selected.artifact);
  return <>
    <Panel label="Artifact"><Subhead>Artifact</Subhead><Facts>
      <Fact label="Version" value={component?.version ?? "Unavailable"} />
      <Fact label="Platform" value={artifact?.platform ?? "Unavailable"} />
      <Fact label="Source" value={<Identity value={component?.commit ?? "Unavailable"} />} />
      <Fact label="Artifact digest" value={<Identity value={artifact?.digest ?? "Unavailable"} />} />
      {artifact?.imageDigest ? <Fact label="Image digest" value={<Identity value={artifact.imageDigest} />} /> : null}
      <Fact label="Size" value={artifact ? `${artifact.size.toLocaleString()} bytes` : "Unavailable"} />
    </Facts></Panel>
    <Panel label="Destination"><Subhead>Destination</Subhead><Facts>
      <Fact label="Name" value={selected.targetId} />
      <Fact label="Address" value={target ? `${target.origin}/${target.repository}` : "Unavailable"} />
      {target?.releaseId ? <Fact label="Release" value={`Draft #${target.releaseId}`} /> : null}
      {target?.tag ? <Fact label="Release tag" value={target.tag} /> : null}
      {target?.assetName ? <Fact label="File" value={target.assetName} /> : null}
      <Fact label="Approved configuration" value={<Identity value={selected.targetDigest} />} />
    </Facts>
    {target?.kind === "file" ? <Caption>Uploads this file to the existing draft release. Making the release public is a separate action.</Caption> : null}
    {!target && targetsKnown ? <Notice sentence="This destination no longer matches the reviewed configuration. Prepare a new candidate before publishing here." /> : null}
    {selected.operation === "install" ? <Notice sentence="This destination requires the cluster update workflow." /> : null}
    </Panel>
    {historyKnown && publication?.state === "pending" ? <Notice tone="warn" sentence="Publication is unresolved. Reconciliation checks the destination before completing or retrying this exact artifact." /> : null}
    {historyKnown && !publication ? <Caption>No publication intent is recorded for this destination.</Caption> : null}
    {publication ? <Panel label="Publication record"><Facts><Fact label="Record" value={<Identity value={publication.effectId} />} /></Facts></Panel> : null}
  </>;
}
function Identity({ value }: { value: string }) { return <span className="os-release-identity">{value}</span>; }
function ReadFailure({ error, retry }: { error: string; retry: () => void }) { return error ? <Notice tone="error" sentence={error}><Button onClick={retry}>Try again</Button></Notice> : null; }
