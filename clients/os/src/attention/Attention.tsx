import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { getRowByConceptAndId, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import { useSession } from "../chrome/access";
import { useOsConnection } from "../live/connection";
import { useLiveCollection } from "../live/useLiveCollection";
import { flatten } from "../kit/rows";
import { accessAdmits, type OsAppManifest } from "../system/registry";
import { receiptKey, unseenChanges, type AttentionChange, type AttentionScope } from "./model";

interface AttentionState {
  changes: readonly AttentionChange[];
  seen: ReadonlySet<string>;
  publish: (source: string, changes: readonly AttentionChange[]) => void;
  acknowledge: (changes: readonly AttentionChange[]) => Promise<void>;
}
const EMPTY: AttentionState = { changes: [], seen: new Set(), publish: () => {}, acknowledge: async () => {} };
const Context = createContext<AttentionState>(EMPTY);
const VisibleContext = createContext(true);
const CONCEPT = "v1:os:attentionReceipt";

export function AttentionProvider({ apps, children }: { apps: readonly OsAppManifest[]; children: ReactNode }) {
  const { access, accessEpoch } = useSession();
  // Scope data to the identity without remounting open app/window state when
  // an initially unresolved identity lands.
  return <UserAttention userId={access?.userId ?? ""} apps={apps} accessEpoch={accessEpoch}>{children}</UserAttention>;
}
function UserAttention({ userId, apps, children }: { userId: string; apps: readonly OsAppManifest[]; children: ReactNode; accessEpoch?: number }) {
  const connection = useOsConnection();
  const { snapshot } = useLiveCollection<Row>(userId ? `attention:${userId}` : null, connection => ({
    concept: CONCEPT,
    seed: async (_cursor, signal) => ({ rows: (await connection.query.myAttentionReceipts({}, { signal })).rows(), nextCursor: "" }),
    reread: async (id, signal) => (await getRowByConceptAndId(connection.query, CONCEPT, id, { signal })) as Row | null,
    paged: false,
  }));
  const [sources, setSources] = useState<Record<string, { userId: string; changes: readonly AttentionChange[] }>>({});
  const [confirmed, setConfirmed] = useState<Record<string, ReadonlySet<string>>>({});
  const publish = useCallback((source: string, changes: readonly AttentionChange[]) => setSources(held => ({ ...held, [source]: { userId, changes } })), [userId]);
  const seen = new Set(confirmed[userId] ?? []);
  for (const raw of snapshot.rows) { const row = flatten(raw); seen.add(receiptKey(rowString(row, "changeId"), rowString(row, "revision"))); }
  const features = apps.filter(app => accessAdmits(app.requires)).flatMap(app => (app.attentionChanges ?? [])
    .filter(change => accessAdmits(app.sections?.find(section => section.id === change.sectionId)?.requires))
    .map(change => ({ ...change, appId: app.id, kind: "feature" as const })));
  const changes = [...features, ...Object.values(sources).filter(source => source.userId === userId).flatMap(source => source.changes)].map(change => {
    const app = apps.find(app => app.id === change.appId);
    const ancestors = new Set(change.ancestors ?? []);
    let parent = app?.sections?.find(section => section.id === change.sectionId)?.parent;
    while (parent && !ancestors.has(parent)) {
      ancestors.add(parent);
      parent = app?.sections?.find(section => section.id === parent)?.parent;
    }
    return { ...change, ancestors: [...ancestors] };
  });
  const acknowledge = useCallback(async (items: readonly AttentionChange[]) => {
    if (!connection || !userId) throw new Error("Connect to save viewed changes.");
    // Capture the revisions actually shown, never whatever is newest when the write finishes.
    await Promise.all(items.map(item => connection.query.acknowledgeAttention({ changeId: item.id, revision: item.revision })));
    setConfirmed(held => ({ ...held, [userId]: new Set([...(held[userId] ?? []), ...items.map(item => receiptKey(item.id, item.revision))]) }));
  }, [connection, userId]);
  return <Context.Provider value={{ changes, seen, publish, acknowledge }}>{children}</Context.Provider>;
}
export function usePublishAttention(source: string, changes: readonly AttentionChange[]) {
  const { publish } = useContext(Context);
  const key = JSON.stringify(changes);
  const stable = useMemo(() => JSON.parse(key) as AttentionChange[], [key]);
  useEffect(() => { publish(source, stable); return () => publish(source, []); }, [source, stable, publish]);
}
export function useAttention(scope: AttentionScope) {
  const state = useContext(Context);
  const unseen = unseenChanges(state.changes, state.seen, scope);
  return { unseen, acknowledge: () => state.acknowledge(unseen) };
}
/** An ancestor only renders a mark. Opening it NEVER acknowledges descendants. */
export function AttentionMarker({ appId, sectionId, target }: AttentionScope) {
  const { unseen } = useAttention({ appId, sectionId, target });
  return unseen.length ? <span className="os-attention-dot" role="img" aria-label="Unseen change" title={unseen.map(item => item.label).join("; ")} /> : null;
}
/** Mount at the actual declared destination, with visible=false for hidden windows.
 * A new revision arriving while visible is also viewed. Failure preserves the mark. */
export function AttentionDestination({ visible = true, children, ...scope }: AttentionScope & { visible?: boolean; children: ReactNode }) {
  const { unseen } = useAttention(scope);
  const ancestorVisible = useContext(VisibleContext);
  visible = visible && ancestorVisible;
  const exact = unseen.filter(item => (item.target ?? "") === (scope.target ?? "") && item.sectionId === scope.sectionId);
  const state = useContext(Context);
  const key = JSON.stringify(exact);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  useEffect(() => {
    if (!visible || exact.length === 0) return;
    let active = true;
    void state.acknowledge(exact).then(() => { if (active) setError(""); }, err => { if (active) setError(String(err)); });
    return () => { active = false; };
  }, [key, visible, retry, state.acknowledge]);
  return <VisibleContext.Provider value={visible}>{children}{error ? <p className="os-caption" role="status">Could not save that this change was viewed. <button type="button" className="os-link" title={error} onClick={() => setRetry(value => value + 1)}>Try again</button></p> : null}</VisibleContext.Provider>;
}
