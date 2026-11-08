import { useRef, useState } from "react";
import { InPlaceAsk } from "../../kit/pressHold";
import { renderMemQLValue, type QueryClient, type Result } from "@znasllc-io/memql-sdk-core/client";
import { Button, Caption, Chip, ChoiceStack, Fact, Facts, Field, Notice, RecordList, RecordRow, Subhead, TextArea, useAppReach } from "../../kit";
import { useOsConnection } from "../../live/connection";
import { useSession } from "../../chrome/access";
import { holds } from "../../system/roles";
import { useReleaseRead } from "../cluster/releases/useReleaseRead";
import { runFromRow } from "../nexus/rows";

export type ChangeOperation = "propose_change" | "deliver_change";
export interface RepositoryAuthority { landingMode: "owner_push" | "pull_request" | ""; sourceId: string; repository: string; repositoryId: number; canWrite: boolean; canRequest: boolean; route: string; operations: ChangeOperation[] }
export interface ChangeRequest {
  id: string; repository: string; repositoryId: number; state: string; reason: string;
  goalId: string; runId: string; approvalId: string; expiresAt: string; parentRequestId: string;
  operation: string; statement?: string; paths?: string[];
  proposal?: { state: string; branch: string; commit: string; pullRequestUrl: string; landingMode: string; baseBranch: string; expectedHead: string; landedCommit: string };
  review?: { approved: boolean; reason: string };
  delivery?: { state: string; reason: string; headSha: string; queueEntryId: string };
}
const OPERATION_OPTIONS = [
  { value: "propose_change", label: "Prepare a proposal", description: "Prepare an exact change for review before it is landed." },
  { value: "deliver_change", label: "Deliver a reviewed change", description: "Continue through independent review, required tests and the merge queue." },
] as const;
const isOperation = (value: unknown): value is ChangeOperation => value === "propose_change" || value === "deliver_change";
function operationSummary(operation: string, ownerPush = false) {
  if (operation === "propose_change" && ownerPush) return "Prepare a reviewable branch within the allowed paths. Landing it requires a separate confirmation using your connected GitHub identity.";
  if (operation === "propose_change") return "Prepare a pull request within the allowed paths. Merging and deployment need separate authorization.";
  if (operation === "deliver_change") return "Request independent review, required tests and merge queue admission for these paths. Required GitHub reviews still apply. Select a production release separately.";
  return "The requested scope was not reported.";
}
function payload(result: Result) { const value = result.single(); if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("Repository authority could not be read."); return value as Record<string, unknown>; }
const text = (v: unknown) => typeof v === "string" ? v : "";
export function readRepositoryAuthority(result: Result): RepositoryAuthority {
  const row = payload(result);
  if (!text(row.sourceId) || !text(row.repository) || typeof row.canWrite !== "boolean" || typeof row.canRequest !== "boolean") throw new Error("Repository authority was not fully reported.");
  return { landingMode: row.landingMode === "owner_push" || row.landingMode === "pull_request" ? row.landingMode : "", sourceId: text(row.sourceId), repository: text(row.repository), repositoryId: typeof row.repositoryId === "number" ? row.repositoryId : 0, canWrite: row.canWrite, canRequest: row.canRequest, route: text(row.route), operations: Array.isArray(row.operations) ? [...new Set(row.operations.filter(isOperation))] : [] };
}
export function readChangeRequest(result: Result): ChangeRequest { return changeRequest(payload(result)); }
function changeRequest(row: Record<string, unknown>): ChangeRequest {
  if (!text(row.id) || !text(row.state)) throw new Error("The saved request could not be read.");
  const review = row.review && typeof row.review === "object" && !Array.isArray(row.review) ? row.review as Record<string, unknown> : null;
  const delivery = row.delivery && typeof row.delivery === "object" && !Array.isArray(row.delivery) ? row.delivery as Record<string, unknown> : null;
  const proposal = row.proposal && typeof row.proposal === "object" && !Array.isArray(row.proposal) ? row.proposal as Record<string, unknown> : null;
  if (review && typeof review.approved !== "boolean") throw new Error("The independent review result was not fully reported.");
  return {
    id: text(row.id), state: text(row.state), operation: text(row.operation), repository: text(row.repository),
    repositoryId: typeof row.repositoryId === "number" ? row.repositoryId : 0, reason: text(row.reason),
    goalId: text(row.goalId), runId: text(row.runId), approvalId: text(row.approvalId),
    expiresAt: text(row.expiresAt), parentRequestId: text(row.parentRequestId), statement: text(row.statement),
    paths: Array.isArray(row.paths) ? row.paths.filter((p): p is string => typeof p === "string") : [],
    ...(review ? { review: { approved: review.approved as boolean, reason: text(review.reason) } } : {}),
    ...(delivery ? { delivery: { state: text(delivery.state), reason: text(delivery.reason), headSha: text(delivery.headSha), queueEntryId: text(delivery.queueEntryId) } } : {}),
    ...(proposal ? { proposal: { state: text(proposal.state), branch: text(proposal.branch), commit: text(proposal.commit), pullRequestUrl: text(proposal.pullRequestUrl), landingMode: text(proposal.landingMode), baseBranch: text(proposal.baseBranch), expectedHead: text(proposal.expectedHead), landedCommit: text(proposal.landedCommit) } } : {}),
  };
}
type AuthorityArguments = {
  repositoryChangeLand: Parameters<QueryClient["repositoryChangeLand"]>[0];
  repositoryChangeAccess: Parameters<QueryClient["repositoryChangeAccess"]>[0];
  repositoryChangeRequest: Parameters<QueryClient["repositoryChangeRequest"]>[0];
  repositoryChangeRequests: Parameters<QueryClient["repositoryChangeRequests"]>[0];
  repositoryChangeStatus: Parameters<QueryClient["repositoryChangeStatus"]>[0];
};
async function call<Name extends keyof AuthorityArguments>(query: QueryClient, name: Name, args: AuthorityArguments[Name], signal?: AbortSignal) {
  return query.executeNamed(name, `builtin ${name}(${Object.entries(args).map(([k,v]) => `${k}: ${renderMemQLValue(v)}`).join(", ")})`, { signal });
}
export function validRequestPaths(value: string): string[] | null {
  const paths = value.split(/\r?\n/).map(path => path.trim()).filter(Boolean);
  return paths.length && paths.every(path => !/^(\/|[A-Za-z]:)/.test(path) && !/[\\*?\[\]{}]/.test(path) && path.split("/").every((part, index, all) => part !== "." && part !== ".." && (part !== "" || index === all.length - 1))) ? [...new Set(paths)] : null;
}
const STATES: Record<string, string> = { opening: "Preparing the request", pending: "Waiting for the repository maintainer", approved: "Approved for the requested work", declined: "Declined by the maintainer", revoked: "Permission withdrawn", expired: "Approval expired" };
const DELIVERY_STATES: Record<string, string> = { waiting: "Waiting to join the merge queue", attention: "Delivery needs attention", queued: "In the merge queue", merged: "Merged into the default branch" };
function proposalLink(request: ChangeRequest | null): string | null {
  const link = request?.proposal?.pullRequestUrl;
  if (!link) return null;
  const match = /^https:\/\/github\.com\/([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)\/pull\/[1-9][0-9]*$/.exec(link);
  return match && match[1]?.toLowerCase() === request.repository.toLowerCase() ? link : null;
}
function requestState(request: ChangeRequest) {
  const approval = STATES[request.state] ?? "Request status unavailable";
  if (!request.delivery) {
    if (request.proposal?.landedCommit) return "Change landed";
    if (request.state === "approved" && request.proposal?.state === "complete") return "Proposal ready for review";
    return approval;
  }
  const delivery = DELIVERY_STATES[request.delivery.state] ?? "Delivery status unavailable";
  return request.state === "approved" ? delivery : `${approval} · ${delivery}`;
}


export function RepositoryChange({ sourceId, visible = true }: { sourceId: string; visible?: boolean }) {
  const connection = useOsConnection(); const { access } = useSession();
  const query = connection?.query ?? null;
  const reach = useAppReach("nexus");
  const authority = useReleaseRead(`repository:${sourceId}:${access?.userId ?? ""}`, query ? async signal => readRepositoryAuthority(await call(query, "repositoryChangeAccess", { sourceId }, signal)) : null, visible, query);
  const [confirmLanding, setConfirmLanding] = useState<string | null>(null);
  const [operation, setOperation] = useState<ChangeOperation>("propose_change");
  const [composing, setComposing] = useState(false), [statement, setStatement] = useState(""), [paths, setPaths] = useState("");
  const [errorAction, setErrorAction] = useState<"request" | "landing">("request");
  const [busy, setBusy] = useState(false), [error, setError] = useState(""), [saved, setSaved] = useState<ChangeRequest | null>(null);
  const inFlight = useRef(false), request = useRef<{ key: string; id: string } | null>(null);
  const history = useReleaseRead(`requests:${sourceId}:${access?.userId ?? ""}`, query ? async signal => {
    const value = payload(await call(query, "repositoryChangeRequests", { sourceId }, signal));
    if (!Array.isArray(value.requests)) throw new Error("Saved requests were not reported.");
    return value.requests.map(row => changeRequest(row as Record<string, unknown>));
  } : null, visible, query);
  const status = useReleaseRead(`request:${saved?.id ?? ""}`, query && saved ? async signal => readChangeRequest(await call(query, "repositoryChangeStatus", { requestId: saved.id }, signal)) : null, visible, query);
  const current = status.value ?? saved;
  const pullRequestUrl = proposalLink(current);
  const proposal = current?.proposal;
  const preparationRunId = current?.state === "approved" && proposal?.state !== "complete" ? current.runId : "";
  // Scope approval is not execution success. Read this exact owned attempt so a
  // draft/review refusal remains visible even before a proposal was recorded.
  const preparation = useReleaseRead(`request-run:${preparationRunId}`, query && preparationRunId ? async signal => {
    const result = await query.workRunSummaryForOwner({ runId: preparationRunId }, { signal });
    const run = result.rows().map(runFromRow).find(row => row.id === preparationRunId && row.goalId === current?.goalId);
    if (!run) throw new Error("The saved preparation attempt could not be read.");
    return run;
  } : null, visible, query);
  // A failed automation step may wait for owner input rather than terminate.
  // Keep that recorded refusal actionable; an ordinary wait is still progress.
  const preparationNeedsInput = preparation.value?.status === "waiting" && !!preparation.value.errorMessage.trim();
  const preparationStopped = preparation.value && (["failed", "abandoned", "cancelled"].includes(preparation.value.status) || preparationNeedsInput);
  const ownerPush = authority.value?.landingMode === "owner_push";
  const reviewURL = current && /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(current.repository) && /^[a-f0-9]{40}$/.test(proposal?.expectedHead ?? "") && /^[a-f0-9]{40}$/.test(proposal?.commit ?? "") ? `https://github.com/${current.repository}/compare/${proposal!.expectedHead}...${proposal!.commit}` : null;
  const canLand = ownerPush && authority.fresh && authority.value?.canWrite && authority.value.route === "own" && status.fresh && current?.state === "approved" && proposal?.state === "complete" && proposal.landingMode === "owner_push" && !proposal.landedCommit && !!reviewURL && holds("execute", "app:pipelines/requests");
  async function land() {
    if (!query || !canLand || !current || !proposal || confirmLanding !== proposal.commit || inFlight.current) return;
    inFlight.current = true; setBusy(true); setError(""); setErrorAction("landing");
    try {
      setSaved(readChangeRequest(await call(query, "repositoryChangeLand", { requestId: current.id, expectedCommit: proposal.commit })));
      setConfirmLanding(null); status.retry(); history.retry();
    } catch (e) { setError(e instanceof Error ? e.message : String(e)); status.retry(); }
    finally { inFlight.current = false; setBusy(false); }
  }
  const scope = validRequestPaths(paths);
  const operations = authority.fresh && authority.value?.canRequest && holds("execute", "app:pipelines/requests") ? authority.value.operations : [];
  const operationAllowed = operations.includes(operation);
  const actionLabel = authority.value?.canWrite && authority.value.route === "own"
    ? operation === "deliver_change" ? "Start reviewed delivery" : "Start requested work"
    : operation === "deliver_change" ? "Send delivery for approval" : "Send for approval";
  async function submit() {
    if (!query || !authority.fresh || !authority.value?.canRequest || !operationAllowed || !scope || !statement.trim() || inFlight.current) return;
    const key = JSON.stringify([sourceId, operation, statement.trim(), scope]);
    if (request.current?.key !== key) request.current = { key, id: crypto.randomUUID() };
    inFlight.current = true; setBusy(true); setError(""); setErrorAction("request");
    try {
      const result = await call(query, "repositoryChangeRequest", { sourceId, requestId: request.current.id, statement: statement.trim(), operation, paths: scope });
      setSaved(readChangeRequest(result)); request.current = null; setComposing(false); history.retry();
    } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    finally { inFlight.current = false; setBusy(false); }
  }
  return <section className="os-app-stack repository-change" aria-label="Repository changes"><Subhead>Repository changes</Subhead>
    {authority.error ? <Notice tone="warn" sentence="Repository permissions are unavailable." detail={authority.error}><Button tone="quiet" onClick={authority.retry}>Check permissions</Button></Notice> : !authority.value ? <Caption>Checking repository permissions…</Caption> : <>
      <div><Chip tone="muted">{authority.value.canWrite ? "GitHub write access" : "Read access"}</Chip></div>
      <Caption>{authority.value.canWrite ? "Your connected GitHub identity has write access to this repository." : authority.value.canRequest ? "Changes go to this repository’s maintainer for approval." : "This repository has no change-request route available to this account."}</Caption>
      {authority.fresh && authority.value.canRequest && holds("execute", "app:pipelines/requests") && operations.length === 0 ? <Caption>Supported change requests were not reported for this repository.</Caption> : null}
      {operations.length > 0 && !composing ? <Button tone="quiet" onClick={() => { if (saved) setOperation("propose_change"); setComposing(true); setSaved(null); }}>Request changes</Button> : null}
    </>}
    {composing ? <div className="os-app-stack">
      {operations.length ? <ChoiceStack name={`operation-${sourceId}`} label="How far should this request go?" voice="prose" value={operationAllowed ? operation : ""} onChange={value => { if (isOperation(value) && !busy) setOperation(value); }} options={OPERATION_OPTIONS.filter(option => operations.includes(option.value)).map(option => ({ ...option, unavailable: busy }))} /> : <Caption>Change requests are unavailable until repository permissions and supported operations can be confirmed.</Caption>}
      <Field label="What should change?"><TextArea id={`request-${sourceId}`} label="What should change?" value={statement} onChange={setStatement} /></Field>
      <Field label="Files or folders this request may change"><TextArea id={`scope-${sourceId}`} label="Files or folders this request may change" value={paths} onChange={setPaths} placeholder="clients/os/" /></Field><Caption>One repository path per line. Use a trailing / for a folder.</Caption>
      <Caption>{operationAllowed ? operationSummary(operation, ownerPush) : "Choose a supported request scope before continuing."}</Caption>
      {operationAllowed && authority.value?.route !== "own" ? <Caption>The repository maintainer must approve this scope before work can start.</Caption> : null}
      {paths.trim() && !scope ? <Notice tone="warn" sentence="Use relative repository paths without wildcards or parent-directory references." /> : null}
      <div className="os-panel-actions"><Button tone="quiet" disabled={busy} onClick={() => setComposing(false)}>Keep draft</Button><Button tone="primary" busy={busy} disabled={busy || !scope || !statement.trim() || !authority.fresh || !operationAllowed} onClick={() => void submit()}>{actionLabel}</Button></div>
    </div> : null}
    {error ? <Notice tone="error" sentence={errorAction === "landing" ? "Landing could not be confirmed." : "The request could not be confirmed."} detail={error} next={errorAction === "landing" ? "The proposal is kept. Retrying checks whether this exact commit already landed before making another change." : "Your draft is kept. Trying again with the same draft reuses its request reference."} /> : null}
    {!composing && !current && history.value?.length ? <RecordList as="ul" label="Saved change requests">{history.value.map(item => <RecordRow key={item.id} name={item.statement || "Repository change request"} state={requestState(item)} onOpen={() => { setSaved(item); setConfirmLanding(null); }} />)}</RecordList> : null}
    {!composing && !current && history.error ? <Notice tone="warn" sentence="Saved change requests could not be read." detail={history.error}><Button tone="quiet" onClick={history.retry}>Read requests again</Button></Notice> : null}
    {current && !composing ? <section className="os-app-stack"><Subhead>{current.statement || "Change request"}</Subhead><Caption>{STATES[current.state] ?? "Request status unavailable"}</Caption>
      <Caption>{operationSummary(current.operation, proposal?.landingMode === "owner_push" || !proposal && ownerPush)}</Caption>
      {current.reason ? <p>{current.reason}</p> : null}
      {preparationStopped ? <Notice tone="warn" sentence={preparationNeedsInput ? "Proposal preparation needs attention." : "Proposal preparation stopped."} detail={preparation.value?.errorMessage || "This attempt ended before a complete proposal was recorded."} next="Review the reason before starting a revised request. The saved attempt is kept.">
        {operations.includes(current.operation as ChangeOperation) ? <Button tone="quiet" onClick={() => { setStatement(current.statement ?? ""); setPaths((current.paths ?? []).join("\n")); setOperation(current.operation as ChangeOperation); setSaved(null); setConfirmLanding(null); setError(""); request.current = null; setComposing(true); }}>Revise request</Button> : null}
      </Notice> : preparation.value && ["queued", "running", "waiting"].includes(preparation.value.status) ? <Caption>Preparing the requested change…</Caption> : null}
      {preparation.error ? <Notice tone="warn" sentence="Preparation progress is unavailable." detail={preparation.error}><Button tone="quiet" onClick={preparation.retry}>Read preparation again</Button></Notice> : null}
      {proposal?.landedCommit ? <Notice tone="info" sentence={`Change landed on ${proposal.baseBranch}.`} next="Wait for the default-branch checks to pass, then cut the release from Pipelines > Releases." /> : proposal?.landingMode === "owner_push" ? <>
        <Caption>The exact proposal is ready for review. Pull requests are disabled for this repository.</Caption>
        {reviewURL ? <a className="os-link" href={reviewURL} target="_blank" rel="noopener noreferrer">Review proposed changes</a> : null}
        {canLand ? confirmLanding === proposal.commit ? <InPlaceAsk key={proposal.commit}><Notice sentence={`Land this change on ${proposal.baseBranch}?`} detail={`Commit ${proposal.commit}`} next="This moves the default branch using your connected GitHub identity and starts its checks. Release publication is a separate action."><div className="os-panel-actions"><Button tone="quiet" disabled={busy} onClick={() => setConfirmLanding(null)}>Keep proposal</Button><Button tone="primary" busy={busy} disabled={busy} onClick={() => void land()}>Confirm landing</Button></div></Notice></InPlaceAsk> : <Button disabled={busy} onClick={() => setConfirmLanding(proposal.commit)}>Land change</Button> : null}
      </> : null}
      {current.review ? <div><Subhead>{current.review.approved ? "Independent review passed" : "Independent review needs attention"}</Subhead>{current.review.reason ? <p>{current.review.reason}</p> : null}</div> : null}
      {current.delivery ? <div><Subhead>{DELIVERY_STATES[current.delivery.state] ?? "Delivery status unavailable"}</Subhead>
        {current.delivery.reason ? <p>{current.delivery.reason}</p> : null}
        {current.delivery.state === "queued" ? <Caption>GitHub will verify this change before merging it. Queue acceptance is not a production release.</Caption> : current.delivery.state === "merged" ? <Caption>Select a release separately to update production.</Caption> : null}
      </div> : null}
      {!status.fresh && status.value && !status.error ? <Caption>Showing the saved status while reconnecting.</Caption> : null}
      {status.error ? <Notice tone="warn" sentence="The saved request could not be refreshed." detail={status.error}><Button onClick={status.retry}>Check again</Button></Notice> : null}
      <div className="os-panel-actions"><Button tone="quiet" onClick={() => setSaved(null)}>All requests</Button>{pullRequestUrl ? <a className="os-link" href={pullRequestUrl} target="_blank" rel="noopener noreferrer">Open pull request</a> : null}{current.goalId && reach.sections.includes("goals") ? <Button onClick={() => reach.open("goals", { goalId: current.goalId })}>Open goal</Button> : null}{current.approvalId && reach.sections.includes("approvals") ? <Button tone="quiet" onClick={() => reach.open("approvals", { approvalId: current.approvalId })}>Open approval</Button> : null}</div>
      <details className="nexus-technical"><summary>Request details</summary><Facts><Fact label="Request reference" value={current.id} copyValue={current.id} /><Fact label="Repository" value={current.repository} /><Fact label="Requested scope" value={OPERATION_OPTIONS.find(option => option.value === current.operation)?.label ?? "Not reported"} />{current.delivery?.headSha ? <Fact label="Reviewed commit" value={current.delivery.headSha} copyValue={current.delivery.headSha} /> : null}{current.delivery?.queueEntryId ? <Fact label="Queue reference" value={current.delivery.queueEntryId} copyValue={current.delivery.queueEntryId} /> : null}{current.paths?.length ? <Fact label="Allowed paths" value={current.paths.join(", ")} /> : null}<Fact label="Approval expires" value={current.expiresAt || "Not reported"} /></Facts></details>
    </section> : null}
  </section>;
}
