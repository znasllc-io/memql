import { useCallback, useEffect, useMemo, useState } from "react";
import { deliverRoleTest, IdentityAdminClient } from "@znasllc-io/memql-sdk-core/identityadmin";
import { useOsConnection } from "../../live/connection";
import { useSession } from "../../chrome/access";
import { isRoleTestTab } from "../../auth/roleTest";
import { roleRungOf } from "../../system/roles";
import { Button, Head, Notice, RecordList, RecordRow, Subhead } from "../../kit";
import { accountNameFrom, type AccountRow } from "../accounts/rows";
import type { RoleRow } from "./rows";
import { ladderDescending, type RoleCatalog } from "./useRoles";

import { PageHelp } from "../../kit/pageHelp";

const predefined = [{ id: "reader", name: "Viewer" }, { id: "writer", name: "Member" }, { id: "developer", name: "Developer" }, { id: "admin", name: "Admin" }];
type Session = Awaited<ReturnType<IdentityAdminClient["roleTest"]>>["sessions"][number];

/** A custom role's name, and its organization's for an organization role. */
function customName(role: RoleRow, accounts: readonly AccountRow[]): string {
  return role.accountId === "" ? role.name : `${role.name} (${accountNameFrom(accounts, role.accountId)})`;
}

export function RoleTestingSection({ role, catalog, accounts }: { role: string; catalog: RoleCatalog; accounts: readonly AccountRow[] }) {
  const connection = useOsConnection();
  const { config } = useSession();
  const client = useMemo(() => connection?.dispatcher ? new IdentityAdminClient(connection.dispatcher) : null, [connection]);
  const allowed = !isRoleTestTab() && (role === "owner" || role === "developer");
  // OFFERED BY THE EXISTING RULE AND NOTHING MORE: the predefined roles as
  // before, Admin for an owner only, then every active custom role ranked
  // strictly below the operator. Whether the operator holds everything the
  // role would is the server's question alone; its refusal is the error Notice.
  const operatorRank = roleRungOf(role)?.rank;
  const roles = useMemo(() => [
    ...predefined.filter(r => r.id !== "admin" || role === "owner"),
    ...ladderDescending(catalog.roles)
      .filter(r => !r.predefined && r.active && operatorRank !== undefined && r.rank < operatorRank)
      .map(r => ({ id: r.slug, name: customName(r, accounts) })),
  ], [role, operatorRank, catalog.roles, accounts]);
  const nameOf = (slug: string) => {
    const custom = catalog.roles.find(r => r.slug === slug && !r.predefined);
    return predefined.find(r => r.id === slug)?.name ?? (custom ? customName(custom, accounts) : slug);
  };
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
  return <div className="os-settings" data-os-dividers="none">
    <Head title="Role testing" meta="30-minute sessions" />
    <PageHelp title="About role testing"><p>Each role uses a separate identity, created when first needed. Your original session stays signed in. Files and conversations remain for later tests; remove test items from Files or Ask when finished.</p><p>Owners can test every non-owner role. Developers can test Viewer, Member, Developer and the custom roles ranked below them, unless a restriction on their account prevents it or the role gives something their own account does not hold. A test of an organization's role joins that organization's group while it runs, so it sees what that organization's people see, and leaves it when the test ends. Tests cannot issue credentials or deploy the cluster. Use the browser editor; reloading a test tab ends its local session.</p></PageHelp>
    {!allowed ? <Notice tone="info" sentence="Owners and developers can start role tests from their own account." /> : <>
      <Notice tone="info" sentence="Tests use this cluster's real permissions. Changes are saved." />
      <RecordList label="Test as role">
        {roles.map(r => <RecordRow key={r.id} name={busy === r.id ? `Starting ${r.name}…` : `Test as ${r.name}`} disabled={!client || Boolean(busy)} onOpen={() => { void start(r.id); }} />)}
      </RecordList>
      {loading ? <p role="status">Loading active tests…</p> : null}
      {!loading && sessions.length > 0 ? <><Subhead>Active tests</Subhead><RecordList label="Active tests">{sessions.map(s => <RecordRow key={s.id}
        name={nameOf(s.role)}
        secondary={`Ends ${new Date(Number(s.expiresAt) * 1000).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })}`}
        actions={<Button disabled={Boolean(busy)} onClick={() => { void end(s.id); }}>End test</Button>} />
      )}</RecordList></> : null}
    </>}
    {error ? <><Notice tone="error" sentence={error} /><Button disabled={loading || Boolean(busy)} onClick={() => { void reload().catch(e => setError(String(e))); }}>Retry</Button></> : null}
  </div>;
}
