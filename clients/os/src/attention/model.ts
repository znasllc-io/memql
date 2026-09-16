/** A meaningful change, explicitly declared in code or discovered at runtime.
 * Never use build timestamps or data heartbeat counters as revisions. */
export interface AttentionChange {
  id: string;
  revision: string;
  appId: string;
  sectionId: string;
  /** The actual destination. Empty means the section itself. */
  target?: string;
  label: string;
  kind: "feature" | "runtime";
  /** Additional navigational ancestors (e.g. an overview of the same item). */
  ancestors?: readonly string[];
}
export type FeatureChange = Omit<AttentionChange, "appId" | "kind">;
export interface AttentionScope { appId: string; sectionId?: string; target?: string }
export const receiptKey = (id: string, revision: string): string => JSON.stringify([id, revision]);
export function matchesScope(change: AttentionChange, scope: AttentionScope): boolean {
  return change.appId === scope.appId &&
    (scope.sectionId === undefined || change.sectionId === scope.sectionId || (change.ancestors ?? []).includes(scope.sectionId)) &&
    (scope.target === undefined || change.target === scope.target);
}
export function unseenChanges(changes: readonly AttentionChange[], seen: ReadonlySet<string>, scope: AttentionScope): AttentionChange[] {
  return changes.filter(change => matchesScope(change, scope) && !seen.has(receiptKey(change.id, change.revision)));
}
