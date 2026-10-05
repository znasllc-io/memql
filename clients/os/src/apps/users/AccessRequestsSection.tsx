import { useMemo, useState } from "react";
import { IdentityAdminClient } from "@znasllc-io/memql-sdk-core/identityadmin";
import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";
import { useOsConnection } from "../../live/connection";
import { useLiveCollection } from "../../live/useLiveCollection";
import { LiveList } from "../../live/LiveList";
import { ActionBar } from "../../kit/ActionBar";
import { Button, Caption, CopyValue, Field, Head, Input, Notice, RecordRow, Select, roleRungOf } from "../../kit";

import type { RoleCatalog } from "./useRoles";
import { rungRefusal, rungSentence, type AssignContext } from "./assign";

const CONCEPT = "v1:identity:accessRequest";
const text = (row: Row, key: string) => typeof row[key] === "string" ? row[key] as string : "";

export function AccessRequestsSection({ catalog, viewerRole, canReview }: { catalog: RoleCatalog; viewerRole: string; canReview: boolean }) {
  const connection = useOsConnection();
  const client = useMemo(() => connection?.dispatcher ? new IdentityAdminClient(connection.dispatcher) : null, [connection]);
  const requests = useLiveCollection<Row>("users:access-requests", connection => ({
    concept: CONCEPT, actions: ["created", "updated"],
    seed: async (cursor, signal) => {
      const result = await connection.query.pendingAccessRequests({}, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: (id, signal) => getRowByConceptAndId(connection.query, CONCEPT, id, { signal }),
    inScope: row => text(row, "status") === "pending",
  }));
  const caller = roleRungOf(viewerRole);
  const assignContext: AssignContext = { kind: "invitation", callerRole: viewerRole, callerRank: caller?.rank ?? 0,
    callerIsOwner: caller?.slug === "owner", grants: catalog.grants, targetRole: "", targetRank: 0,
    targetIsOwner: false, targetAccountIds: [] };
  const [selected, select] = useState<Row | null>(null);
  const [role, setRole] = useState("");
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<Awaited<ReturnType<IdentityAdminClient["reviewAccessRequest"]>> | null>(null);
  const review = async (decision: "approve" | "reject") => {
    if (!selected || !client || !canReview || busy) return;
    setBusy(true); setError("");
    try {
      setResult(await client.reviewAccessRequest(text(selected, "id"), decision, role, note));
      requests.reseed();
    } catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setBusy(false); }
  };
  if (selected) return <div className="os-action-pane">
    <div className="os-action-body os-app-stack">
      <Head title={text(selected, "name") || text(selected, "email")} back={{ label: "Access requests", onSelect: () => select(null) }} />
      <Caption>{text(selected, "email")}</Caption>
      {text(selected, "additionalContext") ? <p>{text(selected, "additionalContext")}</p> : null}
      {result ? <>
        <Notice tone="info" sentence={result.message} />
        {result.url ? <CopyValue value={result.url} label="Invitation link" /> : null}
        {result.emailError ? <Notice tone="warn" sentence="The invitation was created, but email delivery failed." detail={result.emailError} /> : null}
      </> : !canReview ? <Notice tone="info" sentence="An owner or admin can approve or reject this request." /> : <>
        <Field label="Initial role"><Select id="access-request-role" label="Initial role" value={role} onChange={setRole}>
          <option value="">Use internal team defaults</option>
          {catalog.roles.map(rung => <option key={rung.slug} value={rung.slug} disabled={Boolean(rungRefusal(rung, assignContext))} title={rungSentence(rungRefusal(rung, assignContext))}>{rung.name}</option>)}
        </Select></Field>
        <Field label="Review note"><Input id="access-request-note" label="Review note" value={note} onChange={setNote} placeholder="Required when rejecting" /></Field>
      </>}
      {!client ? <Notice tone="warn" sentence="Reconnect to review this request." /> : null}
      {error ? <Notice tone="error" sentence={error} /> : null}
    </div>
    <ActionBar state={busy ? "Saving review" : result ? "Reviewed" : "Awaiting review"} tone={busy ? "busy" : "none"}
      acts={busy ? [] : [{ label: "Back", text: true, onAct: () => select(null) }, ...(result || !client || !canReview ? [] : [
        ...(note.trim() ? [{ label: "Reject", text: true, onAct: () => { void review("reject"); } }] : []),
        { label: "Approve and invite", tone: "primary" as const, onAct: () => { void review("approve"); } },
      ])]} />
  </div>;
  return <div className="os-settings">
    <Head title="Access requests" />
    <Caption>Review who may join. Approval creates an invitation; an account is created when they accept it.</Caption>
    {requests.snapshot.error ? <Notice tone="error" sentence="Access requests could not be read."><Button onClick={requests.reseed}>Try again</Button></Notice> : null}
    <LiveList<Row> source={requests.source} rowId={row => text(row, "id")} fingerprint={row => `${text(row, "status")}|${text(row, "email")}`}
      label="Pending access requests" emptyText="No access requests waiting for review."
      renderRow={(row, tick) => <RecordRow name={text(row, "name") || text(row, "email")} secondary={text(row, "name") ? text(row, "email") : undefined}
        state="Pending" stateExtra={tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        onOpen={() => { select(row); setRole(""); setNote(""); setError(""); setResult(null); }} />} />
  </div>;
}
