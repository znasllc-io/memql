import { useRef, useState } from "react";
import { renderMemQLValue, type QueryClient, type Result } from "@znasllc-io/memql-sdk-core/client";
import { Button, Caption, Chip, Fact, Facts, Field, Notice, RecordList, RecordRow, Subhead, TextArea, useAppReach } from "../../kit";
import { useOsConnection } from "../../live/connection";
import { useSession } from "../../chrome/access";
import { holds } from "../../system/roles";
import { useReleaseRead } from "../cluster/releases/useReleaseRead";

export interface RepositoryAuthority { sourceId: string; repository: string; repositoryId: number; canWrite: boolean; canRequest: boolean; route: string }
export interface ChangeRequest { id: string; repository: string; repositoryId: number; state: string; reason: string; goalId: string; runId: string; approvalId: string; expiresAt: string; parentRequestId: string; statement?: string; paths?: string[] }
function payload(result: Result) { const value = result.single(); if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("Repository authority could not be read."); return value as Record<string, unknown>; }
const text = (v: unknown) => typeof v === "string" ? v : "";
export function readRepositoryAuthority(result: Result): RepositoryAuthority {
  const row = payload(result);
  if (!text(row.sourceId) || !text(row.repository) || typeof row.canWrite !== "boolean" || typeof row.canRequest !== "boolean") throw new Error("Repository authority was not fully reported.");
  return { sourceId: text(row.sourceId), repository: text(row.repository), repositoryId: typeof row.repositoryId === "number" ? row.repositoryId : 0, canWrite: row.canWrite, canRequest: row.canRequest, route: text(row.route) };
}
export function readChangeRequest(result: Result): ChangeRequest { return changeRequest(payload(result)); }
function changeRequest(row: Record<string, unknown>): ChangeRequest {
  if (!text(row.id) || !text(row.state)) throw new Error("The saved request could not be read.");
  return { id: text(row.id), state: text(row.state), repository: text(row.repository), repositoryId: typeof row.repositoryId === "number" ? row.repositoryId : 0, reason: text(row.reason), goalId: text(row.goalId), runId: text(row.runId), approvalId: text(row.approvalId), expiresAt: text(row.expiresAt), parentRequestId: text(row.parentRequestId), statement: text(row.statement), paths: Array.isArray(row.paths) ? row.paths.filter((p): p is string => typeof p === "string") : [] };
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

export function RepositoryChange({ sourceId, visible = true }: { sourceId: string; visible?: boolean }) {
  const connection = useOsConnection(); const { access } = useSession();
  const query = connection?.query ?? null;
  const reach = useAppReach("nexus");
  const authority = useReleaseRead(`repository:${sourceId}:${access?.userId ?? ""}`, query ? async signal => readRepositoryAuthority(await call(query, "repositoryChangeAccess", { sourceId }, signal)) : null, visible, query);
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
  const scope = validRequestPaths(paths);
  async function submit() {
    if (!query || !authority.fresh || !authority.value?.canRequest || !scope || !statement.trim() || inFlight.current) return;
    const key = JSON.stringify([sourceId, statement.trim(), scope]);
    if (request.current?.key !== key) request.current = { key, id: crypto.randomUUID() };
    inFlight.current = true; setBusy(true); setError("");
    try {
      const result = await call(query, "repositoryChangeRequest", { sourceId, requestId: request.current.id, statement: statement.trim(), operation: "propose_change", paths: scope });
      setSaved(readChangeRequest(result)); setComposing(false); history.retry();
    } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    finally { inFlight.current = false; setBusy(false); }
  }
  return <section className="os-app-stack repository-change" aria-label="Repository changes"><Subhead>Repository changes</Subhead>
    {authority.error ? <Notice tone="warn" sentence="Repository permissions are unavailable." detail={authority.error}><Button tone="quiet" onClick={authority.retry}>Check permissions</Button></Notice> : !authority.value ? <Caption>Checking repository permissions…</Caption> : <>
      <div><Chip tone="muted">{authority.value.canWrite ? "Can authorize proposed changes" : "Read access"}</Chip></div>
      <Caption>{authority.value.canWrite ? "Your connected GitHub identity can authorize work within this repository." : authority.value.canRequest ? "Changes go to this repository’s maintainer for approval." : "This repository has no change-request route available to this account."}</Caption>
      {holds("execute", "app:pipelines/requests") && authority.value.canRequest && !composing ? <Button tone="quiet" onClick={() => { setComposing(true); setSaved(null); }}>Request changes</Button> : null}
    </>}
    {composing ? <div className="os-app-stack"><Field label="What should change?"><TextArea id={`request-${sourceId}`} label="What should change?" value={statement} onChange={setStatement} /></Field>
      <Field label="Files or folders this request may change"><TextArea id={`scope-${sourceId}`} label="Files or folders this request may change" value={paths} onChange={setPaths} placeholder="clients/os/" /></Field><Caption>One repository path per line. Use a trailing / for a folder. This request does not authorize a merge or deployment.</Caption>
      {paths.trim() && !scope ? <Notice tone="warn" sentence="Use relative repository paths without wildcards or parent-directory references." /> : null}
      <div className="os-panel-actions"><Button tone="quiet" disabled={busy} onClick={() => setComposing(false)}>Keep draft</Button><Button tone="primary" busy={busy} disabled={busy || !scope || !statement.trim() || !authority.fresh} onClick={() => void submit()}>{authority.value?.canWrite ? "Start requested work" : "Send for approval"}</Button></div>
    </div> : null}
    {error ? <Notice tone="error" sentence="The request could not be confirmed." detail={error} next="Your draft is kept. Trying again with the same draft reuses its request reference." /> : null}
    {!composing && !current && history.value?.length ? <RecordList as="ul" label="Saved change requests">{history.value.map(item => <RecordRow key={item.id} name={item.statement || "Repository change request"} state={STATES[item.state] ?? "Status not recognized"} onOpen={() => setSaved(item)} />)}</RecordList> : null}
    {!composing && !current && history.error ? <Notice tone="warn" sentence="Saved change requests could not be read." detail={history.error}><Button tone="quiet" onClick={history.retry}>Read requests again</Button></Notice> : null}
    {current && !composing ? <section className="os-app-stack"><Subhead>{current.statement || "Change request"}</Subhead><Caption>{STATES[current.state] ?? "Request status unavailable"}</Caption>{current.reason ? <p>{current.reason}</p> : null}
      {status.error ? <Notice tone="warn" sentence="The saved request could not be refreshed." detail={status.error}><Button onClick={status.retry}>Check again</Button></Notice> : null}
      <div className="os-panel-actions"><Button tone="quiet" onClick={() => setSaved(null)}>All requests</Button>{current.goalId && reach.sections.includes("goals") ? <Button onClick={() => reach.open("goals", { goalId: current.goalId })}>Open goal</Button> : null}{current.approvalId && reach.sections.includes("approvals") ? <Button tone="quiet" onClick={() => reach.open("approvals", { approvalId: current.approvalId })}>Open approval</Button> : null}</div>
      <details className="nexus-technical"><summary>Request details</summary><Facts><Fact label="Request reference" value={current.id} copyValue={current.id} /><Fact label="Repository" value={current.repository} />{current.paths?.length ? <Fact label="Allowed paths" value={current.paths.join(", ")} /> : null}<Fact label="Approval expires" value={current.expiresAt || "Not reported"} /></Facts></details>
    </section> : null}
  </section>;
}
