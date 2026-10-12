import { versionOrder } from "@znasllc-io/memql-sdk-core/client";
import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

// An account's reserved MemQL name, as the MemQL address stop understands it
// (epic memql#5168).
//
// Everything here is PURE -- a row in, a string or a record out -- so the
// vocabulary, the guidance and the failure sentences are testable without a
// DOM, a cluster or a subscription. The stop is presentation over server law:
// v1:platform:accountFrontDoor is clusterOwner tier, every mutation on it is
// @serverOnly, and the guards run in Go beside executeWrite. Nothing in this
// file is a check.

/** The doors themselves (`dsl/platform/concepts.memql`). */
export const ACCOUNT_FRONT_DOOR_CONCEPT = "v1:platform:accountFrontDoor";

/**
 * The three hosts a reserved name serves, in the order they are shown.
 *
 * ONE PLACE, mirroring `frontdoor.AccountRoles()` in Go -- and the mirror is
 * checked by `TestTheOsSpellsTheSameThreeLabelsTheEngineDoes`, which reads
 * this array out of the source rather than restating it. A client's employee
 * reads these labels on their own company's domain; they are `app` / `api` /
 * `id` rather than the cluster's own `os` / `api` / `identity` because that is
 * what a person expects to find there (design D of the access program).
 */
export const DOOR_ROLES = [
  { role: "app", label: "app", purpose: "MemQL OS, with this account in context" },
  { role: "api", label: "api", purpose: "the API, for anything you connect" },
  { role: "id", label: "id", purpose: "sign-in" },
] as const;

export type DoorRole = (typeof DOOR_ROLES)[number]["role"];

export interface HostCheck {
  ok: boolean;
  reason: string;
  detail: string;
}

export interface FrontDoorRow {
  id: string;
  accountId: string;
  reservedName: string;
  status: string;
  hostChecks: Record<string, HostCheck>;
  failureReason: string;
  failureDetail: string;
  lastCheckedAt: string;
  verifiedAt: string;
  issuedAt: string;
  removedAt: string;
  createdAt: string;
}

/** Projects a raw wire row. Ids arrive bare; the fold does no projection. */
export function frontDoorFromRow(row: Row): FrontDoorRow {
  return {
    id: rowString(row, "id"),
    accountId: rowString(row, "accountId"),
    reservedName: rowString(row, "reservedName"),
    status: rowString(row, "status"),
    hostChecks: hostChecksOf(row),
    failureReason: rowString(row, "failureReason"),
    failureDetail: rowString(row, "failureDetail"),
    lastCheckedAt: rowString(row, "lastCheckedAt"),
    verifiedAt: rowString(row, "verifiedAt"),
    issuedAt: rowString(row, "issuedAt"),
    removedAt: rowString(row, "removedAt"),
    createdAt: rowString(row, "createdAt"),
  };
}

function hostChecksOf(row: Row): Record<string, HostCheck> {
  const raw = (row as Record<string, unknown>).hostChecks;
  if (!raw || typeof raw !== "object") return {};
  const out: Record<string, HostCheck> = {};
  for (const [role, value] of Object.entries(raw as Record<string, unknown>)) {
    if (!value || typeof value !== "object") continue;
    const v = value as Record<string, unknown>;
    out[role] = {
      ok: v.ok === true,
      reason: typeof v.reason === "string" ? v.reason : "",
      detail: typeof v.detail === "string" ? v.detail : "",
    };
  }
  return out;
}

/**
 * What counts as NEWS on a door, for the arrival cue.
 *
 * A HEARTBEAT IS NOT NEWS. `lastCheckedAt` moves on every sweep pass for every
 * door that is not settled, every two minutes, forever -- so naming it here
 * would turn the stop into a strobe on a two-minute cycle
 * (clients/os/README.md). The same is true of `hostChecks`, whose DETAIL
 * strings carry whatever a resolver said and can differ between two passes
 * that mean the same thing.
 *
 * What a person would call a change on a door is its status flipping, or the
 * reason it is stuck changing. Both are here; the timestamps are not, and the
 * stop displays them continuously instead -- which is the right home for
 * something that is always true and never news.
 */
export function frontDoorFingerprint(d: FrontDoorRow): string {
  return [d.id, d.status, d.failureReason].join("\u0000");
}

/** The host one role is served at under a reserved name. */
export function hostFor(role: DoorRole, reservedName: string): string {
  return `${role}.${reservedName}`;
}

/**
 * Every host a reserved name serves, with what the last pass saw at it.
 *
 * A role with NO recorded check reads as `pending` rather than as a failure.
 * Absent is "nobody has looked yet", which is a different statement from "we
 * looked and it is wrong" -- and the first one is what a door looks like in
 * the seconds between being opened and the sweep's first pass.
 */
export interface DoorHost {
  role: DoorRole;
  host: string;
  purpose: string;
  state: "ok" | "wrong" | "pending";
  /** What the cluster saw, when it saw something. */
  observed: string;
}

export function hostsFor(d: FrontDoorRow): DoorHost[] {
  return DOOR_ROLES.map(({ role, purpose }) => {
    const check = d.hostChecks[role];
    const state: DoorHost["state"] = check ? (check.ok ? "ok" : "wrong") : "pending";
    return {
      role,
      host: hostFor(role, d.reservedName),
      purpose,
      state,
      observed: check && !check.ok ? check.detail : "",
    };
  });
}

/**
 * The target every one of the three names points at.
 *
 * ONE TARGET FOR ALL THREE, because one ingress controller terminates them all
 * and routes by Host -- which is why this stop states the value once instead of
 * repeating a Type/Name/Value strip per host the way the Deployables Domains
 * panel does for a client's own domains. There, every field varies; here, five
 * of six do not.
 */
export function pointingTarget(clusterDomain: string): string {
  return `os.${clusterDomain}`;
}

export const DOOR_TERMINAL_STATUSES = new Set(["removed"]);

/** Whether a door is answering right now. */
export function isServing(d: FrontDoorRow | null): boolean {
  return d?.status === "live";
}

/**
 * The one sentence at the top of the stop: what is true, in the operator's
 * terms.
 *
 * EVERY STATE ANSWERS "WHAT NOW", including the settled ones -- `live`'s answer
 * is "nothing, and here is how to undo it", which is stated in the stop rather
 * than left to be inferred from the absence of a control.
 */
export function doorSentence(d: FrontDoorRow | null): string {
  if (!d) return "";
  switch (d.status) {
    case "pending_dns":
      return "Not served yet. Create the three records below and it comes up on its own.";
    case "verifying":
      return "Not served yet. Some of the three names do not point here.";
    case "issuing":
      if (d.failureReason === "no_acme_issuer") {
        return "All three names point here. This cluster issues no certificates, so nothing can be served.";
      }
      return "All three names point here. Waiting for the certificate.";
    case "live":
      return "Serving.";
    case "removing":
      return "No longer served. Removing the routes and the certificate.";
    case "removed":
      return "No longer served.";
    default:
      return "";
  }
}

export type DoorTone = "ok" | "warn" | "error" | "muted";

export function doorTone(d: FrontDoorRow | null): DoorTone {
  if (!d) return "muted";
  if (d.status === "live") return "ok";
  if (d.status === "removed" || d.status === "removing") return "muted";
  if (d.failureReason === "no_acme_issuer" || d.failureReason === "issuance_failed") return "error";
  return "warn";
}

/**
 * Why a name is not held, in the operator's terms.
 *
 * THE ASK THIS FIELD ANSWERS: an absent `memqlReservedAt` used to mean two
 * different things -- ownership unproven, or a name that was refused -- and the
 * rail inferred which by reading the ownership stop beside it. The engine now
 * says which.
 *
 * `domain_is_front_door_host` IS WRITTEN BY NOTHING TODAY, and that is worth
 * knowing rather than discovering. Every front-door host is a single label
 * under the cluster's own domain, so the under-domain branch fires first and
 * the front-door branch is unreachable for any name a person would type. The
 * sentence is here because the code that would set it is here; if that
 * ordering ever changes, this is already right.
 */
export function reservationReasonSentence(reason: string): string {
  switch (reason) {
    case "ownership_unproven":
      return "This account's domain is not verified yet. Prove it above and the name is held automatically.";
    case "domain_under_cluster_domain":
      return "That name sits under this cluster's own domain, which it already answers on. Choose a name under the client's domain.";
    case "domain_is_front_door_host":
      return "That name is one this cluster answers on itself. Choose another.";
    case "name_collides_with_site":
      return "A deployable is already served at one of those hosts.";
    case "name_collides_with_custom_domain":
      return "A custom domain is already bound to one of those hosts.";
    default:
      return "";
  }
}

export function isKnownReservationReason(reason: string): boolean {
  return reservationReasonSentence(reason) !== "";
}

/** The most recent door for an account -- there is one live, and any number removed. */
export function currentDoor(rows: FrontDoorRow[], accountId: string): FrontDoorRow | null {
  const mine = rows.filter((r) => r.accountId === accountId);
  if (mine.length === 0) return null;
  const live = mine.find((r) => !DOOR_TERMINAL_STATUSES.has(r.status));
  if (live) return live;
  // ALL REMOVED: show the most recent one rather than nothing. "We served this
  // and stopped" is a different answer from "we never did", and the row that
  // survives removal is what makes it sayable.
  return [...mine].sort((a, b) => (versionOrder(b.createdAt, a.createdAt) ?? 0))[0] ?? null;
}
