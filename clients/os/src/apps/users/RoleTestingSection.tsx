import { useCallback, useEffect, useMemo, useState } from "react";
import { deliverRoleTest, IdentityAdminClient } from "@znasllc-io/memql-sdk-core/identityadmin";
import { useOsConnection } from "../../live/connection";
import { useSession } from "../../chrome/access";
import { isRoleTestTab } from "../../auth/roleTest";
import { Button, Head, Notice, RecordList, RecordRow, Subhead } from "../../kit";

import { PageHelp } from "../../kit/pageHelp";

type RoleTestAnswer = Awaited<ReturnType<IdentityAdminClient["roleTest"]>>;
type Session = RoleTestAnswer["sessions"][number];
type TestableRole = RoleTestAnswer["roles"][number];

/** A role as its row says it: its name, and an organization role's organization. */
function roleLabel(role: TestableRole): string {
  return role.organization === "" ? role.name : `${role.name} (${role.organization})`;
}

export function RoleTestingSection({ role }: { role: string }) {
  const connection = useOsConnection();
  const { config } = useSession();
  const client = useMemo(() => connection?.dispatcher ? new IdentityAdminClient(connection.dispatcher) : null, [connection]);
  const allowed = !isRoleTestTab() && (role === "owner" || role === "developer");
  // THE SERVER'S LIST AND NOTHING ELSE: the roles this operator may test now,
  // each decided as a start of it is decided, so no test it would refuse is
  // drawn (DESIGN.md rule 12). Nothing here ranks or filters a role.
  const [roles, setRoles] = useState<TestableRole[]>([]);
  const [loaded, setLoaded] = useState(false);
  const nameOf = (slug: string) => {
    const offered = roles.find(r => r.role === slug);
    return offered ? roleLabel(offered) : slug;
  };
  const [sessions, setSessions] = useState<Session[]>([]);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const reload = useCallback(async () => {
    if (!client || !allowed) return;
    setLoading(true); setError("");
    try {
      const answer = await client.roleTest("list");
      setSessions(answer.sessions);
      setRoles(answer.roles);
      setLoaded(true);
    }
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
  return <div className="os-settings" data-os-dividers="none">
    <Head title="Role testing" meta="30-minute sessions" />
    <PageHelp title="About role testing"><p>Each role uses a separate identity, created when first needed. Your original session stays signed in. Files and conversations remain for later tests; remove test items from Files or Ask when finished.</p><p>Owners can test every non-owner role. Developers can test Viewer, Member, Developer and the custom roles ranked below them, unless a restriction on their account prevents it or the role gives something their own account does not hold. A test of an organization's role joins that organization's group while it runs, so it sees what that organization's people see, and leaves it when the test is ended, or when you next start or end a test after it expires. Tests cannot issue credentials or deploy the cluster. Use the browser editor; reloading a test tab ends its local session.</p></PageHelp>
    {!allowed ? <Notice tone="info" sentence="Owners and developers can start role tests from their own account." /> : <>
      <Notice tone="info" sentence="Tests use this cluster's real permissions. Changes are saved." />
      {loaded && roles.length > 0 ? <RecordList label="Test as role">
        {roles.map(r => <RecordRow key={r.role} name={busy === r.role ? `Starting ${roleLabel(r)}…` : `Test as ${roleLabel(r)}`} disabled={!client || Boolean(busy)} onOpen={() => { void start(r.role); }} />)}
      </RecordList> : null}
      {loaded && roles.length === 0 ? <Notice tone="info" sentence="No role can be tested from your account, because it is restricted or lacks something each role would allow." /> : null}
      {loading ? <p role="status">Loading role tests…</p> : null}
      {!loading && sessions.length > 0 ? <><Subhead>Active tests</Subhead><RecordList label="Active tests">{sessions.map(s => <RecordRow key={s.id}
        name={nameOf(s.role)}
        secondary={`Ends ${new Date(Number(s.expiresAt) * 1000).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })}`}
        actions={<Button disabled={Boolean(busy)} onClick={() => { void end(s.id); }}>End test</Button>} />
      )}</RecordList></> : null}
    </>}
    {error ? <><Notice tone="error" sentence={error} /><Button disabled={loading || Boolean(busy)} onClick={() => { void reload().catch(e => setError(String(e))); }}>Retry</Button></> : null}
  </div>;
}
