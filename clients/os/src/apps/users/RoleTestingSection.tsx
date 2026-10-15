import { useCallback, useEffect, useMemo, useState } from "react";
import { deliverRoleTest, IdentityAdminClient } from "@znasllc-io/memql-sdk-core/identityadmin";
import { useOsConnection } from "../../live/connection";
import { useSession } from "../../chrome/access";
import { isRoleTestTab } from "../../auth/roleTest";
import { Button, Head, Notice, RecordList, RecordRow, Subhead } from "../../kit";

import { PageHelp } from "../../kit/pageHelp";

const roles = [{ id: "reader", name: "Viewer" }, { id: "writer", name: "Member" }, { id: "developer", name: "Developer" }, { id: "admin", name: "Admin" }];
type Session = Awaited<ReturnType<IdentityAdminClient["roleTest"]>>["sessions"][number];

export function RoleTestingSection({ role }: { role: string }) {
  const connection = useOsConnection();
  const { config } = useSession();
  const client = useMemo(() => connection?.dispatcher ? new IdentityAdminClient(connection.dispatcher) : null, [connection]);
  const allowed = !isRoleTestTab() && (role === "owner" || role === "developer");
  const [sessions, setSessions] = useState<Session[]>([]);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const reload = useCallback(async () => {
    if (!client || !allowed) return;
    setLoading(true); setError("");
    try { setSessions((await client.roleTest("list")).sessions); }
    finally { setLoading(false); }
  }, [client, allowed]);
  useEffect(() => { void reload().catch(e => setError(String(e))); }, [reload]);
  const start = async (target: string) => {
    if (!client || busy) return;
    const tab = window.open("about:blank", "_blank");
    if (!tab) { setError("Allow a new tab to start a role test."); return; }
    setBusy(target); setError("");
    let sessionId = "";
    try {
      const result = await client.roleTest("start", { role: target });
      const session = result.sessions[0];
      if (!session || !result.token) throw new Error("The test session could not be created.");
      sessionId = session.id;
      deliverRoleTest(tab, `${location.origin}/`, { token: result.token, sessionId, role: session.role, expiresAt: Number(session.expiresAt), domain: config.domain });
      await reload();
    } catch (e) { tab.close(); setError(e instanceof Error ? e.message : String(e)); if (sessionId) await client.roleTest("end", { sessionId }).catch(() => {}); }
    finally { setBusy(""); }
  };
  const end = async (sessionId: string) => {
    if (!client || busy) return;
    setBusy(sessionId); setError("");
    try { await client.roleTest("end", { sessionId }); await reload(); }
    catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    finally { setBusy(""); }
  };
  return <div className="os-settings">
    <Head title="Role testing" meta="30-minute sessions" />
    <PageHelp title="About role testing"><p>Each role uses a separate identity, created when first needed. Your original session stays signed in. Files and conversations remain for later tests; remove test items from Files or Ask when finished.</p><p>Owners can test every non-owner role. Developers can test Viewer, Member and Developer unless a restriction on their account prevents it. Tests cannot issue credentials or deploy the cluster. Use the browser editor; reloading a test tab ends its local session.</p></PageHelp>
    {!allowed ? <Notice tone="info" sentence="Owners and developers can start role tests from their own account." /> : <>
      <Notice tone="info" sentence="Tests use this cluster's real permissions. Changes are saved." />
      <RecordList label="Test as role">
        {roles.filter(r => r.id !== "admin" || role === "owner").map(r => <RecordRow key={r.id} name={busy === r.id ? `Starting ${r.name}…` : `Test as ${r.name}`} disabled={!client || Boolean(busy)} onOpen={() => { void start(r.id); }} />)}
      </RecordList>
      {loading ? <p role="status">Loading active tests…</p> : null}
      {!loading && sessions.length > 0 ? <><Subhead>Active tests</Subhead><RecordList label="Active tests">{sessions.map(s => <RecordRow key={s.id}
        name={roles.find(r => r.id === s.role)?.name ?? s.role}
        secondary={`Ends ${new Date(Number(s.expiresAt) * 1000).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })}`}
        actions={<Button disabled={Boolean(busy)} onClick={() => { void end(s.id); }}>End test</Button>} />
      )}</RecordList></> : null}
    </>}
    {error ? <><Notice tone="error" sentence={error} /><Button disabled={loading || Boolean(busy)} onClick={() => { void reload().catch(e => setError(String(e))); }}>Retry</Button></> : null}
  </div>;
}
