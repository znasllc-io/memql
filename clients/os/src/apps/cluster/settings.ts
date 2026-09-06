// The Cluster app's own settings, and the store that keeps them.
//
// ===========================================================================
// THE SAME DISCIPLINE apps/fleet/settings.ts STATES, FOR THE SAME REASON
// ===========================================================================
// system/store.ts's DesktopDocument is the SHELL's state, and its
// `version: 1` gate rejects a document whose version it does not know.
// Folding per-app preferences into it would mean every app that ships a
// settings section bumps that version, and a bump discards the whole desktop
// of anyone who has not upgraded yet.
//
// So this is a separate key with the SAME discipline -- versioned, sanitized
// on read, every storage touch in a try/catch. `sanitizeClusterSettings` is
// the corrupt-value tolerance: an unreadable, wrong-version or
// partially-garbage document comes back as the DEFAULTS, never as a crash and
// never as half a document.

import type { OsAppSection } from "../../system/registry";

/**
 * The sections this app declares, in manifest order. Exported because the
 * manifest and the settings picker must offer the same set: a preference
 * naming a section that no longer exists is what `sanitize` repairs.
 *
 * ===========================================================================
 * THE ROLE FLOORS ARE ENGINE FACTS, NOT HOUSE STYLE
 * ===========================================================================
 * Each one below was read off the construct or the Go gate it mirrors, and
 * two of them are the kind that look wrong until the reason is written down.
 *
 * MODULES IS `{ any: ["owner", "admin"] }` AND NOT `{ min: "admin" }`. The
 * engine gate is `AuthorizeModuleRead` -> `auth.AtLeastAdmin` ->
 * `roleHasCapability(role, "create", "principal")`, and
 * `component/auth/rbac_model.go` gives DEVELOPER only `read` on `principal`
 * -- so a developer is REFUSED. Under the cluster's one role ladder
 * developer ranks 300, ABOVE admin's 200, so `{ min: "admin" }` admits
 * exactly the role the engine refuses, and every developer would open this
 * section onto a refusal. That is the same non-monotonic shape as
 * Settings -> Integrations (`{ any: ["owner", "developer"] }`), and the set
 * form is the only one that can say it: `min` is a floor on a ladder, and
 * the rung being excluded here is in the MIDDLE of the one it would name.
 *
 * DATA ORIGINS AND AUDIT TRAIL ARE `{ min: "owner" }`. `syncStatesAll`
 * filters `actor.isClusterOwner==true`; `recentAuditEvents` filters the same
 * AND `v1:identity:auditEvent` declares `@rowAuthz(clusterOwner)`. The audit
 * floor is load-bearing rather than cosmetic -- see the header of
 * `audit/AuditSection.tsx`, where the reason is the finding.
 *
 * LOGS IS `{ min: "admin" }` because every read on the log store is (spec
 * L3). It is the one section whose floor is not this app's to choose.
 */
export const CLUSTER_SECTIONS: OsAppSection[] = [
  { id: "readiness", name: "Readiness" },
  { id: "modules", name: "Modules", roles: { any: ["owner", "admin"] } },
  { id: "origins", name: "Data origins", roles: { min: "owner" } },
  { id: "agents", name: "Agents" },
  { id: "audit", name: "Audit trail", roles: { min: "owner" } },
  { id: "logs", name: "Logs", roles: { min: "admin" } },
  { id: "settings", name: "Settings" },
];

export const CLUSTER_SECTION_IDS = CLUSTER_SECTIONS.map((s) => s.id);

export interface ClusterSettings {
  version: 1;
  /** The section the app navigates to when its window opens. */
  defaultSection: string;
  /**
   * Whether agents that are not active are listed (DESIGN.md rule 4).
   *
   * OFF by default, which picks `activeAgents` over `allAgents`: an
   * inactive agent is a template nothing will run, and the standing question
   * the Agents section answers is what this cluster DOES run. Fleet's
   * show-revoked preference is the same shape for the same reason.
   */
  showInactiveAgents: boolean;
}

export const CLUSTER_SETTINGS_KEY = "memql-os-cluster-v1";

export const DEFAULT_CLUSTER_SETTINGS: ClusterSettings = {
  version: 1,
  // Readiness first, per the manifest. Named here as well because this value
  // is what a corrupt or absent document falls back to, and falling back to
  // "whatever is first in an array" would move with an unrelated edit.
  defaultSection: "readiness",
  showInactiveAgents: false,
};

/**
 * Bring a parsed document back to invariants.
 *
 * Every field is checked INDEPENDENTLY and repaired to its default, rather
 * than the document being rejected wholesale on the first bad value: a
 * garbage `defaultSection` must not cost the operator their show-inactive
 * choice. A wrong `version` IS wholesale, because the field names cannot be
 * trusted at all then.
 *
 * A `defaultSection` naming a section this app no longer declares is
 * repaired, not kept: navigating to it would leave the window on the
 * manifest's first section with the nav highlighting nothing, which reads as
 * a broken app rather than as a stale preference.
 */
export function sanitizeClusterSettings(raw: unknown): ClusterSettings {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ...DEFAULT_CLUSTER_SETTINGS };
  }
  const doc = raw as Partial<ClusterSettings>;
  if (doc.version !== 1) return { ...DEFAULT_CLUSTER_SETTINGS };

  const defaultSection =
    typeof doc.defaultSection === "string" && CLUSTER_SECTION_IDS.includes(doc.defaultSection)
      ? doc.defaultSection
      : DEFAULT_CLUSTER_SETTINGS.defaultSection;

  return {
    version: 1,
    defaultSection,
    showInactiveAgents:
      typeof doc.showInactiveAgents === "boolean"
        ? doc.showInactiveAgents
        : DEFAULT_CLUSTER_SETTINGS.showInactiveAgents,
  };
}

export interface ClusterSettingsStore {
  load(): ClusterSettings;
  save(settings: ClusterSettings): void;
}

export class LocalClusterSettingsStore implements ClusterSettingsStore {
  /** Pass `null` for "no storage" (the default only replaces undefined),
   *  which is the shape the desktop store takes for the same reason: a
   *  private window and a full quota are normal cases, not failures. */
  constructor(
    private readonly storage: Pick<Storage, "getItem" | "setItem"> | null | undefined = globalThis.localStorage,
    private readonly key: string = CLUSTER_SETTINGS_KEY,
  ) {}

  load(): ClusterSettings {
    try {
      const raw = this.storage?.getItem(this.key);
      if (!raw) return { ...DEFAULT_CLUSTER_SETTINGS };
      return sanitizeClusterSettings(JSON.parse(raw));
    } catch {
      // Unparseable JSON is exactly the corrupt case sanitize exists for;
      // it just never gets that far, so the fallback is repeated here.
      return { ...DEFAULT_CLUSTER_SETTINGS };
    }
  }

  save(settings: ClusterSettings): void {
    try {
      this.storage?.setItem(this.key, JSON.stringify(settings));
    } catch {
      // Best-effort: a preference is not worth failing an interaction over.
    }
  }
}
