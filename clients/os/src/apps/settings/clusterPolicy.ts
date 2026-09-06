import { useCallback, useEffect, useState } from "react";
import type { ClusterSettingsEdit } from "@znasllc-io/memql-sdk-core/identityadmin";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { readClusterSettings } from "./adminWire";

// The editable half of the cluster's runtime settings (epic memql#4984).
//
// ===========================================================================
// THE FORM CARRIES MINUTES AND DAYS; THE WIRE CARRIES SECONDS
// ===========================================================================
// `accessTokenTtlSeconds` is seconds and `invitationTtlDays` is days, which is
// the shape the row and the boot envelope agree on. Nobody reasons about a
// session in seconds, so the form asks in the unit a person would say it in
// and converts at exactly one seam -- here. A second conversion anywhere else
// is a second place to be off by sixty.
//
// ZERO IS "THE BOOT DEFAULT", NOT "NOTHING". Every TTL treats 0 as "fall back
// to the environment, or to the built-in", so the form renders a blank box
// with a `Cluster default` placeholder rather than a literal 0 -- a person who
// reads `0` in a session-lifetime field reasonably concludes sessions expire
// immediately.
//
// THE BOUNDS ARE THE ADMIN FORM LAYER'S, and they are stated on the concept:
// access [60, 86400], refresh [86400, 31536000], magic link [60, 3600],
// invitation [1, 90]. They are checked here so the person is told which field
// is out of range while looking at it, and again server-side, which remains
// the authority.

export interface PolicyDraft {
  registrationMode: string;
  registrationDomains: string;
  internalDomains: string;
  internalDefaultRole: string;
  accessRequestNotifyEmails: string;
  /** Minutes. "" means the boot default. */
  accessTokenMinutes: string;
  /** Days. "" means the boot default. */
  refreshTokenDays: string;
  /** Minutes. "" means the boot default. */
  magicLinkMinutes: string;
  /** Days. "" means the boot default. */
  invitationDays: string;
  refreshCookieSameSite: string;
  brandName: string;
  brandPrimaryColor: string;
}

export const EMPTY_DRAFT: PolicyDraft = {
  registrationMode: "open",
  registrationDomains: "",
  internalDomains: "",
  internalDefaultRole: "writer",
  accessRequestNotifyEmails: "",
  accessTokenMinutes: "",
  refreshTokenDays: "",
  magicLinkMinutes: "",
  invitationDays: "",
  refreshCookieSameSite: "",
  brandName: "",
  brandPrimaryColor: "",
};

function str(row: Row, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}

function num(row: Row, key: string): number {
  const v = row[key];
  if (typeof v === "number") return v;
  if (typeof v === "string" && v.trim() !== "") {
    const parsed = Number(v);
    return Number.isFinite(parsed) ? parsed : 0;
  }
  return 0;
}

/** A stored 0 renders as "", which is what the placeholder explains. */
function unit(seconds: number, per: number): string {
  return seconds > 0 ? String(Math.round(seconds / per)) : "";
}

export function draftFromRow(row: Row | null): PolicyDraft {
  if (row === null) return EMPTY_DRAFT;
  return {
    registrationMode: str(row, "registrationMode") || "open",
    registrationDomains: str(row, "registrationDomains"),
    internalDomains: str(row, "internalDomains"),
    internalDefaultRole: str(row, "internalDefaultRole") || "writer",
    accessRequestNotifyEmails: str(row, "accessRequestNotifyEmails"),
    accessTokenMinutes: unit(num(row, "accessTokenTTLSeconds"), 60),
    refreshTokenDays: unit(num(row, "refreshTokenTTLSeconds"), 86400),
    magicLinkMinutes: unit(num(row, "magicLinkTTLSeconds"), 60),
    invitationDays: unit(num(row, "invitationTTLDays"), 1),
    refreshCookieSameSite: str(row, "refreshCookieSameSite"),
    brandName: str(row, "brandName"),
    brandPrimaryColor: str(row, "brandPrimaryColor"),
  };
}

interface Bound {
  field: keyof PolicyDraft;
  /** In the unit the FORM uses, so the message can quote what is on screen. */
  min: number;
  max: number;
  unit: string;
}

const BOUNDS: Bound[] = [
  { field: "accessTokenMinutes", min: 1, max: 1440, unit: "minutes" },
  { field: "refreshTokenDays", min: 1, max: 365, unit: "days" },
  { field: "magicLinkMinutes", min: 1, max: 60, unit: "minutes" },
  { field: "invitationDays", min: 1, max: 90, unit: "days" },
];

/**
 * What is wrong with this draft, in the person's own units, or "" when
 * nothing is. Blank is always legal: it means the boot default.
 */
export function draftProblem(draft: PolicyDraft): string {
  for (const bound of BOUNDS) {
    const raw = draft[bound.field].trim();
    if (raw === "") continue;
    const value = Number(raw);
    if (!Number.isFinite(value) || !Number.isInteger(value)) {
      return `${labelOf(bound.field)} has to be a whole number of ${bound.unit}, or blank for the cluster default.`;
    }
    if (value < bound.min || value > bound.max) {
      return `${labelOf(bound.field)} has to be between ${bound.min} and ${bound.max} ${bound.unit}, or blank for the cluster default.`;
    }
  }
  if (draft.registrationMode === "domain_restricted" && draft.registrationDomains.trim() === "") {
    // The refusal a person would otherwise meet later: domain_restricted with
    // no allowlist admits nobody, which reads as "sign-up is broken".
    return "Domain-restricted registration needs at least one allowed email domain, or nobody can sign up.";
  }
  return "";
}

const LABELS: Partial<Record<keyof PolicyDraft, string>> = {
  accessTokenMinutes: "Session length",
  refreshTokenDays: "Stay signed in for",
  magicLinkMinutes: "Sign-in link expires after",
  invitationDays: "Invitation expires after",
};

function labelOf(field: keyof PolicyDraft): string {
  return LABELS[field] ?? field;
}

/** Convert a form value in `per`-second units to the wire's seconds. Blank is
 *  0, which the server reads as "the boot default". */
function toSeconds(value: string, per: number): number {
  const raw = value.trim();
  if (raw === "") return 0;
  const parsed = Number(raw);
  return Number.isFinite(parsed) ? Math.round(parsed) * per : 0;
}

export function editFromDraft(draft: PolicyDraft): ClusterSettingsEdit {
  return {
    registrationMode: draft.registrationMode,
    internalDefaultRole: draft.internalDefaultRole,
    registrationDomains: draft.registrationDomains.trim(),
    internalDomains: draft.internalDomains.trim(),
    accessRequestNotifyEmails: draft.accessRequestNotifyEmails.trim(),
    accessTokenTtlSeconds: toSeconds(draft.accessTokenMinutes, 60),
    refreshTokenTtlSeconds: toSeconds(draft.refreshTokenDays, 86400),
    magicLinkTtlSeconds: toSeconds(draft.magicLinkMinutes, 60),
    invitationTtlDays: toSeconds(draft.invitationDays, 1),
    refreshCookieSameSite: draft.refreshCookieSameSite,
    brandName: draft.brandName.trim(),
    brandPrimaryColor: draft.brandPrimaryColor.trim(),
  };
}

export interface PolicyFacts {
  draft: PolicyDraft;
  /** What the cluster last told us, so Revert has something to go back to. */
  stored: PolicyDraft;
  set: (next: Partial<PolicyDraft>) => void;
  revert: () => void;
  loading: boolean;
  error: string;
  /** True once nothing in the draft differs from what was read. */
  clean: boolean;
  reload: () => void;
}

export function useClusterPolicy(enabled: boolean): PolicyFacts {
  const connection = useOsConnection();
  const [stored, setStored] = useState<PolicyDraft>(EMPTY_DRAFT);
  const [draft, setDraft] = useState<PolicyDraft>(EMPTY_DRAFT);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);
  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  useEffect(() => {
    if (!enabled || connection === null) return;
    const controller = new AbortController();
    let stale = false;
    setLoading(true);
    setError("");
    void readClusterSettings(connection, controller.signal)
      .then((row) => {
        if (stale) return;
        const next = draftFromRow(row);
        setStored(next);
        // The DRAFT is replaced too. A reload after a save has to show what
        // the cluster stored, and holding a stale edit over a fresh read is
        // how a form tells somebody their change took when it did not.
        setDraft(next);
      })
      .catch((err: unknown) => {
        if (stale) return;
        setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!stale) setLoading(false);
      });
    return () => {
      stale = true;
      controller.abort();
    };
  }, [connection, enabled, epoch]);

  const set = useCallback((next: Partial<PolicyDraft>) => {
    setDraft((held) => ({ ...held, ...next }));
  }, []);
  const revert = useCallback(() => setDraft(stored), [stored]);

  const clean = (Object.keys(stored) as (keyof PolicyDraft)[]).every(
    (key) => stored[key] === draft[key],
  );

  return { draft, stored, set, revert, loading, error, clean, reload };
}
