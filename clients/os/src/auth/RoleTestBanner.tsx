import { useEffect, useMemo, useState } from "react";
import { IdentityAdminClient } from "@znasllc-io/memql-sdk-core/identityadmin";
import { useOsConnection } from "../live/connection";
import { Button } from "../kit";
import { useAuth } from "./AuthProvider";
import { currentRoleTest, registerRoleTestEnd } from "./roleTest";

export function RoleTestBanner() { return currentRoleTest() ? <ActiveRoleTestBanner /> : null; }
function ActiveRoleTestBanner() {
  const test = currentRoleTest();
  const connection = useOsConnection();
  const { signOut } = useAuth();
  const client = useMemo(() => connection?.dispatcher ? new IdentityAdminClient(connection.dispatcher) : null, [connection]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (!test || !client) return;
    return registerRoleTestEnd(async () => { await client.roleTest("end", { sessionId: test.sessionId }); });
  }, [client, test]);
  if (!test) return null;
  const role = ({ reader: "Viewer", writer: "Member", developer: "Developer", admin: "Admin" } as Record<string, string>)[test.role] ?? test.role;
  return <RoleTestNotice role={role} expiresAt={test.expiresAt} disabled={busy || !client} error={error} onEnd={() => { setBusy(true); void signOut().catch(e => setError(String(e))).finally(() => setBusy(false)); }} />;
}

export function RoleTestNotice({ role, expiresAt, disabled, error, onEnd }: { role: string; expiresAt: number; disabled: boolean; error: string; onEnd: () => void }) {
  return <aside className="os-role-test-banner" aria-label="Role test session">
    <span>Testing as {role} · ends {new Date(expiresAt * 1000).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })}</span>
    <Button disabled={disabled} onClick={onEnd}>End test</Button>
    {error ? <span role="alert">{error}</span> : null}
  </aside>;
}
