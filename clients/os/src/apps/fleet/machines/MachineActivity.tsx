import { useCallback, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { AutoRefresh, EmptyState, Fact, Facts, Head, Notice, RecordList, RecordListSkeleton, RecordRow, useNow } from "../../../kit";
import { formatDuration, formatMoment } from "../../../kit/format";
import { flatten } from "../../../kit/rows";
import { PageHelp } from "../../../kit/pageHelp";
import type { PageTrail } from "../../../kit/pageNavigation";
import { useReading } from "../../../cluster/reading";
import { useOsConnection } from "../../../live/connection";
import { useSessionIfPresent } from "../../../chrome/access";
import { feedIsBehind, useLiveCollection } from "../../../live/useLiveCollection";
import { useAppSessions, useAppSessionDetail } from "../apps/useAppSessions";
import { useInvocations } from "../routing/useInvocations";
import { appLabel, type InvocationRow, type AppSessionRow } from "../rows";
import { MachineDetailsSection } from "./MachineDetailsInfo";

interface ActivityItem { id: string; kind: string; label: string; status: string; startedAt: string; endedAt: string; correlationId: string; facts: [string, string][]; sessionId?: string }
const str = (row: Row, key: string) => typeof row[key] === "string" ? row[key] as string : "";
const LIVE = new Set(["queued", "starting", "running", "pending", "requested"]);
export const activityStatus = (status: string) => ({ queued: "Queued", pending: "Queued", requested: "Queued", starting: "Starting", running: "Running", succeeded: "Completed", success: "Completed", ended: "Completed", failed: "Failed", failure: "Failed", cancelled: "Cancelled", timeout: "Timed out", interrupted: "Interrupted" })[status] ?? status.replaceAll("_", " ");
function lifecycle(raw: Row, now: Date): ActivityItem {
  const r = flatten(raw), recorded = str(r, "status"), expired = LIVE.has(recorded) && Date.parse(str(r, "expiresAt")) < now.getTime();
  return { id: `activity:${str(r, "id")}`, kind: str(r, "kind") === "model" ? "Model call" : "Tool call", label: str(r, "label"), status: expired ? "interrupted" : recorded, startedAt: str(r, "startedAt"), endedAt: str(r, "endedAt"), correlationId: str(r, "correlationId"), facts: [["Activity id", str(r, "id")], ["Details", expired ? "The call stopped reporting before completion was confirmed." : str(r, "detail")]] };
}
function invocation(call: InvocationRow): ActivityItem {
  return { id: `tool:${call.id}`, kind: "Tool call", label: `${call.tool}.${call.action}`, status: call.outcome, startedAt: call.startedAt, endedAt: call.completedAt, correlationId: call.correlationId,
    facts: [["Call id", call.id], ["Run", call.runId], ["Step", call.stepId], ["Duration", formatDuration(call.durationMs)], ["Error", [call.errorCode, call.errorMessage].filter(Boolean).join(": ")], ["Arguments", call.argsRedacted], ["Output preview", call.outputPreview], ["Routing", call.routing.present ? JSON.stringify(call.routing, null, 2) : ""]] };
}
function session(row: AppSessionRow): ActivityItem {
  return { id: `app:${row.id}`, sessionId: row.id, kind: "App session", label: appLabel(row.app), status: row.status, startedAt: row.startedAt, endedAt: row.endedAt, correlationId: "", facts: [["Session id", row.id], ["Run", row.runId], ["Billing", row.billing]] };
}

export function MachineActivity({ workerId, navigation }: { workerId: string; navigation?: Omit<PageTrail, "title"> }) {
  const userId = useSessionIfPresent()?.access?.userId ?? "";
  const connection = useOsConnection();
  const readEarlier = useCallback(async (signal: AbortSignal) => (await connection!.query.myModelCallsOnMachine({ workerId }, { signal })).rows(), [connection, workerId]);
  const earlier = useReading(`fleet:earlier-models:${userId}:${workerId}`, connection && userId ? readEarlier : null);
  const now = useNow(15_000);
  const [selected, setSelected] = useState<ActivityItem | null>(null);
  const calls = useInvocations(workerId, true);
  const apps = useAppSessions();
  const feed = useLiveCollection<Row>(workerId && userId ? `fleet:activity:${userId}:${workerId}` : null, connection => ({
    concept: "v1:worker:activity",
    seed: async (_cursor, signal) => {
      const reads = await Promise.all([connection.query.machineActivity({ workerId }, { signal }), connection.query.activeMachineActivity({ workerId }, { signal }), connection.query.firstMachineActivity({ workerId }, { signal })]);
      return { rows: [...new Map(reads.flatMap(result => result.rows()).map(row => [str(flatten(row), "id"), row])).values()], nextCursor: "" };
    },
    reread: async (activityId, signal) => (await connection.query.machineActivityById({ activityId }, { signal })).rows()[0] ?? null,
    inScope: raw => { const row = flatten(raw); return row["workerId"] === workerId && row["ownerUserId"] === userId; },
    paged: false,
  }));
  const receipts = calls.invocations.map(invocation);
  const completed = new Set(receipts.map(row => row.correlationId).filter(Boolean));
  // Earlier ledger rows remain available. From the first durable lifecycle
  // receipt onward, that feed is the authority, avoiding duplicate model calls.
  const cutover = feed.snapshot.rows.reduce((at, raw) => { const row = flatten(raw); const start = str(row, "kind") === "model" ? str(row, "startedAt") : ""; return start && start < at ? start : at; }, "9999");
  const historical: ActivityItem[] = (earlier.value ?? []).map(flatten).filter(row => str(row, "createdAt") < cutover).map(row => ({
    id: `earlier:${str(row, "id")}`, kind: "Model call", label: str(row, "servedModel") || str(row, "model"), status: str(row, "outcome") === "ok" ? "succeeded" : str(row, "outcome") === "cancelled" ? "cancelled" : "failed",
    startedAt: str(row, "createdAt"), endedAt: "", correlationId: "",
    facts: [["Call id", str(row, "id")], ["Request id", str(row, "requestId")], ["Task", str(row, "promptName")], ["Billing", str(row, "billing")], ["Duration", typeof row["totalDurationMs"] === "number" ? formatDuration(row["totalDurationMs"]) : "Not reported"]],
  }));
  const items = [...historical, ...feed.snapshot.rows.map(row => lifecycle(row, now)).filter(row => !row.correlationId || !completed.has(row.correlationId)), ...receipts, ...apps.sessions.filter(row => row.workerId === workerId).map(session)]
    .sort((a, b) => Number(LIVE.has(b.status)) - Number(LIVE.has(a.status)) || b.startedAt.localeCompare(a.startedAt));
  const item = selected ? items.find(row => row.id === selected.id || row.correlationId && row.correlationId === selected.correlationId) ?? selected : null;
  const loading = feed.snapshot.state === "seeding" || calls.loading || apps.loading || earlier.state === "reading";
  const error = [feed.snapshot.error, calls.error, apps.error, earlier.error].filter(Boolean).join(" · ");
  const behind = feedIsBehind(feed.snapshot.state, error) || feedIsBehind(calls.state) || feedIsBehind(apps.state);
  const back = () => setSelected(null);
  return <div className="fleet-machine-activity">
    <Head title={item ? item.label : "Activity"} meta={!item && !loading && !behind ? items.length : undefined}
      breadcrumbs={[...(navigation?.breadcrumbs ?? []), { label: "Activity", ...(item ? { onSelect: back } : {}) }, ...(item ? [{ label: item.label }] : [])]}
      back={item ? { label: "Activity", onSelect: back } : navigation?.back}>
      <PageHelp title="Activity"><p>Model calls, app sessions and tool calls for this machine. Queued and active work appears first; completed work follows. Updates arrive automatically.</p><p>Older model calls, tool calls and app sessions remain available. Queue and running transitions are recorded from this update onward. Copy a detail using its copy icon.</p></PageHelp>
      <AutoRefresh enabled={behind} busy={loading} onRefresh={() => { feed.reseed(); calls.refresh(); apps.reread(); earlier.reread(); }} />
    </Head>
    {behind ? <Notice tone="warn" sentence="Activity updates are interrupted." next="Showing the last available records." detail={error || undefined} /> : null}
    {item ? <ActivityDetails item={item} /> : <>
      {loading && !items.length ? <RecordListSkeleton label="Loading activity" /> : null}
      {!loading && !behind && !items.length ? <EmptyState title="No activity yet">Work assigned to this machine will appear here.</EmptyState> : null}
      {items.length ? <RecordList as="ul" label="Machine activity">{items.map(row => <RecordRow key={row.id} name={row.label} secondary={`${row.kind} · ${formatMoment(row.startedAt)}`} state={activityStatus(row.status)} tone={LIVE.has(row.status) ? "accent" : ["failed", "failure", "interrupted", "timeout"].includes(row.status) ? "warn" : "muted"} onOpen={() => setSelected(row)} />)}</RecordList> : null}
    </>}
  </div>;
}
function ActivityDetails({ item }: { item: ActivityItem }) {
  return <div className="fleet-machine-details">
    <MachineDetailsSection title="Overview"><Facts>
      <Fact label="Status" value={activityStatus(item.status)} copyValue={activityStatus(item.status)} />
      <Fact label="Type" value={item.kind} copyValue={item.kind} />
      <Fact label={item.id.startsWith("earlier:") ? "Recorded" : "Started"} value={formatMoment(item.startedAt)} copyValue={item.startedAt} />
      {item.endedAt ? <Fact label="Ended" value={formatMoment(item.endedAt)} copyValue={item.endedAt} /> : null}
    </Facts></MachineDetailsSection>
    <MachineDetailsSection title="Details"><Facts>{item.facts.filter(([, value]) => value).map(([label, value]) => <Fact key={label} label={label} value={value} copyValue={value} />)}</Facts></MachineDetailsSection>
    {item.sessionId ? <SessionDetails id={item.sessionId} /> : null}
  </div>;
}
function SessionDetails({ id }: { id: string }) {
  const detail = useAppSessionDetail(id);
  if (detail.error) return <Notice sentence="Session details could not be read." detail={detail.error} />;
  const row = detail.session;
  if (!row) return null;
  return <MachineDetailsSection title="Session"><Facts>{[["Workspace", row.workspace], ["Request", row.prompt], ["Error", row.errorMessage], ["Cancelled", row.cancelReason], ["Recording", row.sessionRunId], ["Transcript", row.transcriptFileId]].filter(([, value]) => value).map(([label, value]) => <Fact key={label} label={label!} value={value} copyValue={value} />)}</Facts></MachineDetailsSection>;
}
