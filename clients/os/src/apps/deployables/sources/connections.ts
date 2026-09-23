import { createContext, createElement, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { getRowByConceptAndId, renderMemQLValue, rowString, type QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";
import { flatten } from "../../../kit/rows";
import { useSession } from "../../../chrome/access";
import { useLiveCollection } from "../../../live/useLiveCollection";
import { bare } from "../people";
import { useWrite } from "../packages/actions";
import type { CredentialRow } from "./rows";
import { repositoryPageFrom, type InstallationRow, type PendingInstallation } from "./repositories";

export const SOURCE_CONNECTION_CONCEPT = "v1:platform:sourceConnection";
export interface SourceConnectionRow {
  id: string;
  ownerUserId: string;
  credentialId: string;
  installationId: string;
  providerAccountId: string;
  accountLogin: string;
  accountType: string;
  status: string;
}
export function sourceConnectionFromRow(raw: Row): SourceConnectionRow {
  const row = flatten(raw);
  return Object.fromEntries(["id", "ownerUserId", "credentialId", "installationId", "providerAccountId", "accountLogin", "accountType", "status"].map(key => [key, rowString(row, key)])) as unknown as SourceConnectionRow;
}
export interface SourceConnectionsFeed {
  rows: readonly SourceConnectionRow[];
  state: string;
  error: string;
  retry: () => void;
  revokedCredentialIds: readonly string[];
  noteCredentialRevoked: (id: string) => void;
  observeCredentials: (rows: readonly CredentialRow[]) => void;
}
const ConnectionsContext = createContext<SourceConnectionsFeed>({ rows: [], state: "disconnected", error: "", retry: () => {}, revokedCredentialIds: [], noteCredentialRevoked: () => {}, observeCredentials: () => {} });
/** One retained feed at the app root, shared by Sources, Settings and the wizard. */
export function SourceConnectionsProvider({ children }: { children: ReactNode }) {
  const { access } = useSession();
  const viewer = bare(access?.userId ?? "");
  const [revoked, setRevoked] = useState<{ viewer: string; entries: Record<string, boolean> }>({ viewer, entries: {} });
  const { snapshot, reseed } = useLiveCollection<Row>(viewer ? `deployables:sourceConnections:${viewer}` : null, connection => ({
    concept: SOURCE_CONNECTION_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.executeNamed("sourceConnectionsMine", "query sourceConnectionsMine()", { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (id, signal) => await getRowByConceptAndId(connection.query, SOURCE_CONNECTION_CONCEPT, id, { signal }) as Row ?? null,
    paged: false,
  }));
  const noteCredentialRevoked = useCallback((id: string) => setRevoked(held => ({ viewer, entries: { ...(held.viewer === viewer ? held.entries : {}), [id]: false } })), [viewer]);
  const observeCredentials = useCallback((credentials: readonly CredentialRow[]) => setRevoked(held => {
    if (held.viewer !== viewer) return held;
    const entries = { ...held.entries };
    let changed = false;
    for (const [id, acknowledged] of Object.entries(entries)) {
      const row = credentials.find(grant => grant.id === id && bare(grant.ownerUserId) === viewer);
      if (row?.status === "revoked" && !acknowledged) { entries[id] = true; changed = true; }
      else if (row?.status === "active" && acknowledged) { delete entries[id]; changed = true; }
    }
    return changed ? { viewer, entries } : held;
  }), [viewer]);
  const rows = snapshot.rows.map(sourceConnectionFromRow).filter(row => row.id && row.status === "active" && bare(row.ownerUserId) === viewer);
  return createElement(ConnectionsContext.Provider, { value: { rows, state: snapshot.state, error: snapshot.error, retry: reseed, revokedCredentialIds: revoked.viewer === viewer ? Object.keys(revoked.entries) : [], noteCredentialRevoked, observeCredentials } }, children);
}
export const useSourceConnections = () => useContext(ConnectionsContext);

export interface SourceInstallation extends InstallationRow { providerAccountId: string }
export interface SourceInstallations { installations: SourceInstallation[]; pending: PendingInstallation[] }
export async function readSourceInstallations(query: QueryClient, credentialId: string): Promise<SourceInstallations> {
  const result = await query.executeNamed("sourceInstallations", `builtin sourceInstallations(credentialId: ${renderMemQLValue(credentialId)})`);
  const raw = result.rows()[0];
  if (!raw) throw new Error("Source access could not be read.");
  const parsed = repositoryPageFrom(raw);
  if (parsed.reason && parsed.reason !== "ok") throw new Error(`${parsed.reason}: Source access could not be read. Reconnect this GitHub account or retry.`);
  // The server owns provider attribution; create only sends its installation ID.
  const field = flatten(raw)["installations"];
  let members: Row[] = [];
  try { const value = typeof field === "string" ? JSON.parse(field) : field; if (Array.isArray(value)) members = value; } catch { /* The parsed reading remains authoritative for usable entries. */ }
  return { installations: parsed.installations.map(installation => ({ ...installation, providerAccountId: rowString(members.find(row => rowString(row, "id") === installation.id) ?? {}, "accountId") })), pending: parsed.pending };
}
export async function createSourceConnection(query: QueryClient, credentialId: string, installationId: string): Promise<string> {
  const result = await query.executeNamed("sourceConnectionCreate", `builtin sourceConnectionCreate(credentialId: ${renderMemQLValue(credentialId)}, installationId: ${renderMemQLValue(installationId)})`);
  const row = result.rows()[0];
  const id = row ? rowString(row, "connectionId") : "";
  if (!id || rowString(row!, "status") !== "active") throw new Error("The source was not saved. Try again.");
  return id;
}
export async function removeSourceConnection(query: QueryClient, connectionId: string): Promise<boolean> {
  const result = await query.executeNamed("sourceConnectionRemove", `builtin sourceConnectionRemove(connectionId: ${renderMemQLValue(connectionId)})`);
  const row = result.rows()[0];
  if (!row || rowString(row, "connectionId") !== connectionId || rowString(row, "status") !== "removed") throw new Error("The source was not removed. Try again.");
  return true;
}
/** A read belongs to one identity. A switched identity never receives an older answer. */
export function useSourceInstallations(credentialId: string) {
  const { run, refusal, clear: clearWrite } = useWrite();
  const [answer, setAnswer] = useState<{ credentialId: string; value: SourceInstallations; readAt: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const generation = useRef(0);
  const clearRef = useRef(clearWrite); clearRef.current = clearWrite;
  const read = useCallback(async () => {
    if (!credentialId) return;
    const request = ++generation.current;
    setBusy(true);
    const value = await run(async query => {
      try { return await readSourceInstallations(query, credentialId); }
      catch (error) { if (request !== generation.current) return null; throw error; }
    });
    if (request !== generation.current) return;
    setBusy(false);
    if (value) setAnswer({ credentialId, value, readAt: new Date().toISOString() });
  }, [credentialId, run]);
  useEffect(() => {
    generation.current++; setAnswer(null); setBusy(false); clearRef.current();
    void read();
    return () => { generation.current++; };
  }, [read]);
  const current = answer?.credentialId === credentialId ? answer : null;
  return { installations: current?.value.installations ?? [], pending: current?.value.pending ?? [], readAt: current?.readAt ?? "", busy, refusal, read };
}
