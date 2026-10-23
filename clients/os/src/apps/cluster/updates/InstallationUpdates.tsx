import { useEffect, useRef, useState } from "react";
import { renderMemQLValue, type QueryClient, type Result } from "@znasllc-io/memql-sdk-core/client";
import { Button, Caption, Details, DetailsSection, Fact, Facts, Notice, RecordList, RecordRow, Subhead } from "../../../kit";
import { InstallationProposals, readInstallationProposal, type InstallationProposal, type ProposedRelease } from "./InstallationProposals";
import { useReleaseRead } from "../releases/useReleaseRead";

export interface InstallationChoice { candidateId: string; catalogDigest: string; overlayRevision: string; label: string }
export interface InstallationPlan extends InstallationChoice {
  installationId: string; preparationId: string; planId: string; state: string;
  expectedCurrentRevision: string; artifactExpiresAt: string; canManage: boolean;
  observation?: { intentObserved?: boolean; operationPending?: boolean; operationPhase?: string; operationSucceeded?: boolean; revisionObserved?: boolean; healthy?: boolean; synced?: boolean };
}
export interface InstallationPreparation extends InstallationChoice { installationId: string; preparationId: string; requestId: string; expectedCurrentRevision: string; state: string; canResume: boolean }
export interface InstallationStatus { preparation: InstallationPreparation | null; installationId: string; currentRevision: string; choices: InstallationChoice[]; active: InstallationPlan | null; latest: InstallationPlan | null; proposalBaseRevision: string; proposals: InstallationProposal[]; choicesWarning: string; proposalWarning: string }
type InstallationQuery = Pick<QueryClient, "executeNamed">;

function record(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("The installation returned an unreadable answer.");
  return value as Record<string, unknown>;
}
function required(value: unknown): string {
  if (typeof value !== "string" || !value) throw new Error("The installation returned incomplete release information.");
  return value;
}
function choice(value: unknown): InstallationChoice {
  const v = record(value);
  return { candidateId: required(v.candidateId), catalogDigest: required(v.catalogDigest), overlayRevision: required(v.overlayRevision), label: typeof v.label === "string" ? v.label : "Release candidate" };
}
export function readInstallationPlan(value: unknown): InstallationPlan {
  const v = record(value);
  return { ...choice(v), installationId: required(v.installationId), preparationId: required(v.preparationId), planId: required(v.planId), state: required(v.state),
    expectedCurrentRevision: required(v.expectedCurrentRevision), artifactExpiresAt: required(v.artifactExpiresAt), canManage: v.canManage === true,
    ...(v.observation && typeof v.observation === "object" ? { observation: v.observation as InstallationPlan["observation"] } : {}) };
}
function readPreparation(value: unknown): InstallationPreparation {
  const v = record(value);
  if (!["preparing", "retiring"].includes(required(v.state))) throw new Error("Preparation progress was not recognized. Read the saved plan again.");
  return { ...choice(v), installationId: required(v.installationId), preparationId: required(v.preparationId), requestId: required(v.requestId), expectedCurrentRevision: required(v.expectedCurrentRevision), state: required(v.state), canResume: v.canResume === true };
}
export function readInstallationStatus(result: Result): InstallationStatus {
  const v = record(result.single());
  if (!Array.isArray(v.choices)) throw new Error("Release choices were not reported by this installation.");
  return { choicesWarning: typeof v.choicesWarning === "string" ? v.choicesWarning : "", proposalWarning: typeof v.proposalWarning === "string" ? v.proposalWarning : "", preparation: v.preparation ? readPreparation(v.preparation) : null, installationId: required(v.installationId), currentRevision: required(v.currentRevision), choices: v.choices.map(choice), active: v.active ? readInstallationPlan(v.active) : null, latest: v.latest ? readInstallationPlan(v.latest) : null, proposalBaseRevision: typeof v.proposalBaseRevision === "string" ? v.proposalBaseRevision : "", proposals: Array.isArray(v.proposals) ? v.proposals.map(readInstallationProposal) : [] };
}
type InstallationArguments = {
  releaseInstallationStatus: Record<string, never>;
  releasePrepareInstallation: Parameters<QueryClient["releasePrepareInstallation"]>[0];
  releaseApplyInstallation: Parameters<QueryClient["releaseApplyInstallation"]>[0];
  releaseObserveInstallation: Parameters<QueryClient["releaseObserveInstallation"]>[0];
  releaseResumeInstallation: Parameters<QueryClient["releaseResumeInstallation"]>[0];
  releaseCancelInstallation: Parameters<QueryClient["releaseCancelInstallation"]>[0];
  releaseProposeInstallation: { installationId: string; requestId: string; candidateId: string; catalogDigest: string; expectedBaseRevision: string };
  releaseObserveInstallationProposal: { proposalId: string };
};
export async function installationCall<Name extends keyof InstallationArguments>(query: InstallationQuery, name: Name, args: InstallationArguments[Name], signal?: AbortSignal) {
  return query.executeNamed(name, `builtin ${name}(${Object.entries(args).map(([key, value]) => `${key}: ${renderMemQLValue(value)}`).join(", ")})`, { signal });
}

/** Preparation is a read-and-validate plan; applying is always a separate human act. */
export function InstallationUpdates({ query, connected, visible = true, proposedRelease, onClearProposedRelease }: { query: InstallationQuery | null; connected: boolean; visible?: boolean; proposedRelease?: ProposedRelease | null; onClearProposedRelease?: () => void }) {
  const available = connected && query !== null;
  const status = useReleaseRead("installation", available ? async signal => readInstallationStatus(await installationCall(query, "releaseInstallationStatus", {}, signal)) : null, visible, query);
  const [selected, select] = useState<InstallationChoice | null>(null);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [confirmCancel, setConfirmCancel] = useState(false);
  const [saved, setSaved] = useState<InstallationPlan | null>(null);
  const request = useRef<{ key: string; id: string } | null>(null);
  const inFlight = useRef(false);
  useEffect(() => setSaved(null), [status.value]);
  const current = status.value;
  const recorded = saved ?? current?.active ?? current?.latest;
  const preparation = current?.preparation;
  const plan = recorded && !["complete", "cancelled"].includes(recorded.state) ? recorded : null;
  const selectedLabel = plan ? current?.choices.find(c => c.candidateId === plan.candidateId && c.overlayRevision === plan.overlayRevision)?.label ?? plan.label : selected?.label;
  async function perform(name: "prepare" | "apply" | "observe" | "resume" | "cancel") {
    if (!query || !current || inFlight.current || !status.fresh) return;
    inFlight.current = true; setBusy(name); setError("");
    try {
      let result: Result;
      if (name === "prepare") {
        if (!selected || plan || preparation || !current.choices.some(choice => choice.candidateId === selected.candidateId && choice.catalogDigest === selected.catalogDigest && choice.overlayRevision === selected.overlayRevision)) return;
        const key = `${current.installationId}:${selected.candidateId}:${selected.catalogDigest}:${selected.overlayRevision}:${current.currentRevision}`;
        if (request.current?.key !== key) request.current = { key, id: crypto.randomUUID() };
        result = await installationCall(query, "releasePrepareInstallation", { installationId: current.installationId, requestId: request.current.id, candidateId: selected.candidateId, catalogDigest: selected.catalogDigest, overlayRevision: selected.overlayRevision, expectedCurrentRevision: current.currentRevision });
      } else if (name === "resume") {
        if (!preparation?.canResume) return;
        result = await installationCall(query, "releaseResumeInstallation", { installationId: current.installationId, preparationId: preparation.preparationId });
      } else {
        if (!plan || ["apply", "cancel"].includes(name) && (plan.state !== "prepared" || !plan.canManage) || name === "cancel" && !confirmCancel) return;
        result = await installationCall(query, name === "apply" ? "releaseApplyInstallation" : name === "cancel" ? "releaseCancelInstallation" : "releaseObserveInstallation", { installationId: current.installationId, planId: plan.planId });
      }
      const outcome = readInstallationPlan(result.single());
      setSaved(outcome);
      if (outcome.state === "complete" || outcome.state === "cancelled") { select(null); request.current = null; setConfirmCancel(false); }
      status.retry();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      // Never repeat a write after an uncertain transport result. Read the saved plan.
      status.retry();
    } finally { inFlight.current = false; setBusy(""); }
  }
  function refresh() { setSaved(null); setError(""); status.retry(); }
  return <Details><DetailsSection title="This installation">
    <Caption>Choose a verified release, review its installation plan, then apply it when you are ready.</Caption>
    {!available ? <Notice sentence="Reconnect to read this installation’s update plan." /> : null}
    {status.error ? <Notice tone="warn" sentence="Installation updates are unavailable." detail={status.error}><Button onClick={refresh}>Check again</Button></Notice> : null}
    {available && !status.value && !status.error ? <Caption>Reading the installed version and available choices…</Caption> : null}
    {current ? <>
      {current.choicesWarning ? <Notice tone="warn" sentence="Release choices are unavailable." detail={current.choicesWarning}><Button onClick={refresh}>Read release choices again</Button></Notice> : null}
      {current.proposalWarning ? <Notice tone="warn" sentence="Update proposals are unavailable." detail={current.proposalWarning}><Button onClick={refresh}>Read update proposals again</Button></Notice> : null}
      <InstallationProposals query={query} status={current} fresh={status.fresh && !current.proposalWarning} release={proposedRelease ?? null} clearRelease={onClearProposedRelease} refresh={status.retry} />
      {!preparation && recorded?.state === "complete" ? <Notice sentence={recorded.overlayRevision === current.currentRevision ? "The selected release is installed and verified." : "A previous selected release was installed and verified."} detail="The installation confirmed its workloads, engine version, migrations and retained access at completion." /> : null}
      {recorded?.state === "cancelled" ? <Notice sentence="The previous installation plan was cancelled." /> : null}
      {preparation ? <section className="os-app-stack" aria-label="Saved installation preparation"><Subhead>Saved preparation</Subhead><Caption>{preparation.state === "retiring" ? "This preparation is being cancelled. Its reserved work must finish cleanup before another release can be selected." : "This release has unfinished preparation. Resume the saved work to verify it and produce an installation plan. Nothing has been applied."}</Caption>
        <details className="nexus-technical"><summary>Preparation details</summary><Facts><Fact label="Selected revision" value={preparation.overlayRevision} /><Fact label="Original request" value={preparation.requestId} /><Fact label="Preparation reference" value={preparation.preparationId} /></Facts></details>
        <div className="os-panel-actions">{preparation.canResume ? <Button tone="primary" disabled={!!busy || !status.fresh} busy={busy === "resume"} onClick={() => void perform("resume")}>Resume preparation</Button> : preparation.state === "preparing" ? <Caption>The original operator must continue this preparation.</Caption> : null}<Button tone="quiet" disabled={!!busy} onClick={refresh}>Read saved preparation</Button></div>
      </section> : plan ? <section className="os-app-stack"><Subhead>{selectedLabel}</Subhead>
        <Caption>{plan.state === "prepared" ? "The installation plan is prepared. Applying it will change this installation." : plan.state === "applying" ? "The selected release is being applied. Check progress to read the latest installation evidence." : `Installation plan: ${plan.state}.`}</Caption>
        {plan.observation ? <Facts><Fact label="Selected version observed" value={plan.observation.revisionObserved === undefined ? "Not reported" : plan.observation.revisionObserved ? "Yes" : "Not yet"} /><Fact label="Controller health" value={plan.observation.healthy === undefined ? "Not reported" : plan.observation.healthy ? "Healthy" : "Not healthy yet"} /><Fact label="Configuration synchronized" value={plan.observation.synced === undefined ? "Not reported" : plan.observation.synced ? "Yes" : "Not yet"} /></Facts> : null}
        <details className="nexus-technical"><summary>Release and installation details</summary><Facts><Fact label="Installation" value={plan.installationId} /><Fact label="Candidate" value={plan.candidateId} copyValue={plan.candidateId} /><Fact label="Selected revision" value={plan.overlayRevision} copyValue={plan.overlayRevision} /><Fact label="Previous revision" value={plan.expectedCurrentRevision} /><Fact label="Plan valid until" value={plan.artifactExpiresAt} /></Facts></details>
        {plan.state === "prepared" && !plan.canManage ? <Caption>The operator who prepared this plan must apply or cancel it.</Caption> : null}
        {confirmCancel && plan.state === "prepared" && plan.canManage ? <Notice sentence="Cancel this installation plan?" detail="Nothing has been applied. Cancelling releases this plan so another release can be selected."><Button tone="quiet" disabled={!!busy} onClick={() => setConfirmCancel(false)}>Keep plan</Button><Button tone="danger" disabled={!!busy || !status.fresh} busy={busy === "cancel"} onClick={() => void perform("cancel")}>Cancel installation plan</Button></Notice> : null}
        <div className="os-panel-actions">{plan.state === "prepared" && plan.canManage && status.fresh && !error && !confirmCancel ? <Button tone="primary" busy={busy === "apply"} disabled={busy !== ""} onClick={() => void perform("apply")}>Apply selected release</Button> : null}{plan.state === "prepared" && plan.canManage && !confirmCancel ? <Button tone="quiet" disabled={!!busy || !status.fresh} onClick={() => setConfirmCancel(true)}>Choose a different release</Button> : null}<Button busy={busy === "observe"} disabled={busy !== "" || !status.fresh} onClick={() => void perform("observe")}>Check progress</Button><Button tone="quiet" disabled={busy !== ""} onClick={refresh}>Read saved plan</Button></div>
      </section> : <>
        <RecordList as="ul" label="Releases available to this installation">{current.choices.map(c => <RecordRow key={`${c.candidateId}:${c.overlayRevision}`} name={c.label} state={selected?.candidateId === c.candidateId && selected?.overlayRevision === c.overlayRevision ? "Selected" : undefined} onOpen={() => { select(c); setError(""); }} />)}</RecordList>
        {current.choices.length === 0 && !current.choicesWarning ? <Caption>No reviewed release is ready to install. Choose a published release to prepare an update proposal.</Caption> : null}
        {selected && current.choices.some(choice => choice.candidateId === selected.candidateId && choice.catalogDigest === selected.catalogDigest && choice.overlayRevision === selected.overlayRevision) ? <div className="os-panel-actions"><Button tone="primary" busy={busy === "prepare"} disabled={busy !== "" || !status.fresh} onClick={() => void perform("prepare")}>Prepare installation plan</Button></div> : null}
      </>}
      <details className="nexus-technical"><summary>Current revision</summary><Facts><Fact label="Installed configuration" value={current.currentRevision} copyValue={current.currentRevision} /></Facts></details>
    </> : null}
    {error ? <Notice tone="error" sentence="The operation could not be confirmed." detail={error}><Button onClick={refresh}>Read saved plan before continuing</Button></Notice> : null}
  </DetailsSection></Details>;
}
