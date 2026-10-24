import { useRef, useState } from "react";
import { renderMemQLValue, type QueryClient, type Result } from "@znasllc-io/memql-sdk-core/client";
import { Button, Caption, Chip, ChoiceStack, Fact, Facts, Field, Notice, RecordList, RecordRow, Subhead, TextArea, useAppReach } from "../../kit";
import { useOsConnection } from "../../live/connection";
import { useSession } from "../../chrome/access";
import { holds } from "../../system/roles";
import { useReleaseRead } from "../cluster/releases/useReleaseRead";

export type ChangeOperation = "propose_change" | "deliver_change";
export interface RepositoryAuthority { sourceId: string; repository: string; repositoryId: number; canWrite: boolean; canRequest: boolean; route: string; operations: ChangeOperation[] }
export interface ChangeRequest {
  id: string; repository: string; repositoryId: number; state: string; reason: string;
  goalId: string; runId: string; approvalId: string; expiresAt: string; parentRequestId: string;
  operation: string; statement?: string; paths?: string[];
  proposal?: { state: string; branch: string; commit: string; pullRequestUrl: string };
  review?: { approved: boolean; reason: string };
  delivery?: { state: string; reason: string; headSha: string; queueEntryId: string };
}
const OPERATION_OPTIONS = [
  { value: "propose_change", label: "Prepare a proposal", description: "Stop at a pull request for review." },
  { value: "deliver_change", label: "Deliver a reviewed change", description: "Continue through independent review, required tests and the merge queue." },
] as const;
const isOperation = (value: unknown): value is ChangeOperation => value === "propose_change" || value === "deliver_change";
function operationSummary(operation: string) {
  if (operation === "propose_change") return "Prepare a pull request within the allowed paths. Merging and deployment need separate authorization.";
  if (operation === "deliver_change") return "Request independent review, required tests and merge queue admission for these paths. Required GitHub reviews still apply. Select a production release separately.";
  return "The requested scope was not reported.";
}
function payload(result: Result) { const value = result.single(); if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("Repository authority could not be read."); return value as Record<string, unknown>; }
const text = (v: unknown) => typeof v === "string" ? v : "";
export function readRepositoryAuthority(result: Result): RepositoryAuthority {
  const row = payload(result);
  if (!text(row.sourceId) || !text(row.repository) || typeof row.canWrite !== "boolean" || typeof row.canRequest !== "boolean") throw new Error("Repository authority was not fully reported.");
  return { sourceId: text(row.sourceId), repository: text(row.repository), repositoryId: typeof row.repositoryId === "number" ? row.repositoryId : 0, canWrite: row.canWrite, canRequest: row.canRequest, route: text(row.route), operations: Array.isArray(row.operations) ? [...new Set(row.operations.filter(isOperation))] : [] };
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
    ...(proposal ? { proposal: { state: text(proposal.state), branch: text(proposal.branch), commit: text(proposal.commit), pullRequestUrl: text(proposal.pullRequestUrl) } } : {}),
  };
}
type AuthorityArguments = {
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
  if (!request.delivery) return approval;
  const delivery = DELIVERY_STATES[request.delivery.state] ?? "Delivery status unavailable";
  return request.state === "approved" ? delivery : `${approval} · ${delivery}`;
}


export function RepositoryChange({ sourceId, visible = true }: { sourceId: string; visible?: boolean }) {
  const connection = useOsConnection(); const { access } = useSession();
  const query = connection?.query ?? null;
  const reach = useAppReach("nexus");
  const authority = useReleaseRead(`repository:${sourceId}:${access?.userId ?? ""}`, query ? async signal => readRepositoryAuthority(await call(query, "repositoryChangeAccess", { sourceId }, signal)) : null, visible, query);
  const [operation, setOperation] = useState<ChangeOperation>("propose_change");
  const [composing, setComposing] = useState(false), [statement, setStatement] = useState(""), [paths, setPaths] = useState("");
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
    inFlight.current = true; setBusy(true); setError("");
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
      <Caption>{operationAllowed ? operationSummary(operation) : "Choose a supported request scope before continuing."}</Caption>
      {operationAllowed && authority.value?.route !== "own" ? <Caption>The repository maintainer must approve this scope before work can start.</Caption> : null}
      {paths.trim() && !scope ? <Notice tone="warn" sentence="Use relative repository paths without wildcards or parent-directory references." /> : null}
      <div className="os-panel-actions"><Button tone="quiet" disabled={busy} onClick={() => setComposing(false)}>Keep draft</Button><Button tone="primary" busy={busy} disabled={busy || !scope || !statement.trim() || !authority.fresh || !operationAllowed} onClick={() => void submit()}>{actionLabel}</Button></div>
    </div> : null}
    {error ? <Notice tone="error" sentence="The request could not be confirmed." detail={error} next="Your draft is kept. Trying again with the same draft reuses its request reference." /> : null}
    {!composing && !current && history.value?.length ? <RecordList as="ul" label="Saved change requests">{history.value.map(item => <RecordRow key={item.id} name={item.statement || "Repository change request"} state={requestState(item)} onOpen={() => setSaved(item)} />)}</RecordList> : null}
    {!composing && !current && history.error ? <Notice tone="warn" sentence="Saved change requests could not be read." detail={history.error}><Button tone="quiet" onClick={history.retry}>Read requests again</Button></Notice> : null}
    {current && !composing ? <section className="os-app-stack"><Subhead>{current.statement || "Change request"}</Subhead><Caption>{STATES[current.state] ?? "Request status unavailable"}</Caption>
      <Caption>{operationSummary(current.operation)}</Caption>
      {current.reason ? <p>{current.reason}</p> : null}
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
