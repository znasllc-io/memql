import { useCallback, useEffect, useRef, useState } from "react";
import { renderMemQLValue, type Row } from "@znasllc-io/memql-sdk-core/client";
import { useOsConnection } from "../../live/connection";
import { notifyGrantWritten } from "../../system/roles";
import { flatten } from "../../kit/rows";

export interface AccountPerson { id: string; name: string; email: string; role: string; apps: string[] }
export interface AccountAccess { invitations?: { id: string; email: string; expiresAt: string; groupIds: string[] }[]; accountId: string; ownerUserId: string; primary: boolean; actorRole: string; clusterOwner: boolean; assignsApps: boolean; revision: string; apps: string[]; people: AccountPerson[]; candidates: AccountPerson[]; groups: { id: string; name: string; kind: string; apps: string[]; userIds: string[] }[] }

export function accountAccessCall(name: "accountAccess" | "accountAccessUpdate", args: Record<string, unknown>): string {
  return `builtin ${name}(${Object.entries(args).filter(([, value]) => value !== undefined).map(([key, value]) => `${key}: ${renderMemQLValue(value)}`).join(", ")})`;
}
export function accountAccessFromRows(rows: Row[]): AccountAccess {
  if (!rows.length) throw new Error("Account access could not be read.");
  const row = flatten(rows[0]!);
  if (typeof row.accountId !== "string" || !Array.isArray(row.people) || typeof row.revision !== "string") throw new Error("Account access returned an incomplete answer.");
  return row as unknown as AccountAccess;
}

/** Re-read on mount/reconnect and after edits. The revision prevents a retained
 * screen from overwriting another manager's changes. Failed writes keep drafts. */
export function useAccountAccess(accountId: string) {
  const connection = useOsConnection();
  const [value, setValue] = useState<AccountAccess | null>(null), [error, setError] = useState(""), [busy, setBusy] = useState(false), [loading, setLoading] = useState(true), [nonce, setNonce] = useState(0);
  const generation = useRef(0), writing = useRef(false);
  const [readAt, setReadAt] = useState("");
  const reload = useCallback(() => setNonce(current => current + 1), []);
  useEffect(() => {
    const current = ++generation.current, controller = new AbortController();
    setValue(null); setError(""); setLoading(true);
    if (connection && accountId) void connection.query.executeNamed("accountAccess", accountAccessCall("accountAccess", { accountId }), { signal: controller.signal })
      .then(result => { if (current === generation.current && !controller.signal.aborted) { setValue(accountAccessFromRows(result.rows())); setReadAt(new Date().toISOString()); } })
      .catch((cause: unknown) => { if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : String(cause)); })
      .finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => { generation.current++; controller.abort(); };
  }, [connection, accountId, nonce]);
  const change = async (args: Record<string, unknown>): Promise<boolean> => {
    if (!connection || !value || writing.current) return false;
    const current = generation.current;
    writing.current = true; setBusy(true); setError("");
    try {
      const result = await connection.query.executeNamed("accountAccessUpdate", accountAccessCall("accountAccessUpdate", { ...args, accountId, revision: value.revision }));
      if (current !== generation.current) return false;
      setValue(accountAccessFromRows(result.rows())); setReadAt(new Date().toISOString()); notifyGrantWritten(); return true;
    } catch (cause) {
      if (current === generation.current) setError(cause instanceof Error ? cause.message : String(cause));
      return false;
    } finally { writing.current = false; setBusy(false); }
  };
  return { value, error, loading, busy, reload, change, readAt };
}
