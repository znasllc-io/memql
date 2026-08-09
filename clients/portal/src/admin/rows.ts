import type { Row } from "@znasllc-io/memql-sdk-core/client";
import type { ConceptLike } from "@znasllc-io/memql-view-kit";

import type { SigningKey } from "./wire";

// Row sets this console ADAPTS, and the descriptors the element library reads
// them through.
//
// Three of the four admin screens render something that is not a concept row
// as it left the engine: a token joined to its owner's email, a key off the
// public JWKS feed, a settings row turned inside out into one row per setting.
// view-kit takes a ConceptLike (id, entity, displayCard) and nothing else, so
// each adapted set declares one here -- the same arrangement src/deploy/rows.ts
// uses for DeployControlService's payloads, and for the same reason: the
// element library stays the only thing drawing rows, without pretending these
// are concepts.
//
// THE IDS ARE NOT MEMQL IDS. `admin.token`, not `v1:admin:token`. A canonical
// id would claim there is a concept behind it that an operator could open in
// the concept browser, and there is not. Reshaping lives here rather than in a
// component so the screens stay composition-only.

// ---------------------------------------------------------------------------
// Personal access tokens
// ---------------------------------------------------------------------------

// A PAT as the console shows it: the credential's own patSummary fields, plus
// the owner's email resolved from the people read. `owner` leads the card
// because incident response starts from a person, not from an identity id.
export const TOKEN_CONCEPT: ConceptLike = {
  id: "admin.token",
  entity: "token",
  displayCard: {
    primary: "owner",
    secondary: "label",
    tertiary: "lastUsedAt",
    status: "state",
  },
};

export interface TokenRow extends Record<string, unknown> {
  id: string;
  owner: string;
  label: string;
  state: string;
  lastUsedAt: string;
  createdAt: string;
  usableByAgents: boolean;
}

function str(row: Row, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}

// tokenRows joins one user's PAT rows to that user's display identity.
//
// `state` rather than the raw `active` boolean: the rail and the status chip
// both read this slot, and "active / revoked" is the operator's vocabulary
// while "true / false" is the column's.
export function tokenRows(rows: readonly Row[], owner: string): TokenRow[] {
  const out: TokenRow[] = [];
  for (const row of rows) {
    const id = str(row, "id");
    if (id === "") continue;
    out.push({
      id,
      owner,
      label: str(row, "label") || "(no label)",
      state: row["active"] === false ? "revoked" : "active",
      lastUsedAt: str(row, "lastUsedAt") || "never",
      createdAt: str(row, "createdAt"),
      usableByAgents: row["usableByAgents"] === true,
    });
  }
  return out;
}

// ---------------------------------------------------------------------------
// Signing keys
// ---------------------------------------------------------------------------

// A key from the public JWKS feed. `role` is the console's word for the
// key's position in the set -- the feed itself carries no such field, and the
// distinction between the key being minted with and the one still accepted
// during the overlap window is the single most useful thing on the page.
export const SIGNING_KEY_CONCEPT: ConceptLike = {
  id: "admin.signingKey",
  entity: "key",
  displayCard: {
    primary: "kid",
    secondary: "algorithm",
    tertiary: "curve",
    status: "role",
  },
};

export interface SigningKeyRow extends Record<string, unknown> {
  id: string;
  kid: string;
  algorithm: string;
  curve: string;
  role: string;
  purpose: string;
}

// signingKeyRows names the first key "signing" and the rest "accepted".
//
// That ordering is the feed's contract rather than a guess: BuildJWKS emits
// KeyManager.PublicKeySet, which puts the current key first and appends the
// previous one only while its retirement deadline is in the future
// (component/identity/jwks.go). A second key IS the overlap window, which is
// why its presence is worth a word on the page.
export function signingKeyRows(keys: readonly SigningKey[]): SigningKeyRow[] {
  const out: SigningKeyRow[] = [];
  for (let i = 0; i < keys.length; i += 1) {
    const key = keys[i];
    if (key === undefined) continue;
    out.push({
      id: key.kid,
      kid: key.kid,
      algorithm: key.alg || key.kty,
      curve: key.crv,
      role: i === 0 ? "signing" : "accepted",
      purpose: key.use || "sig",
    });
  }
  return out;
}

// ---------------------------------------------------------------------------
// Cluster settings
// ---------------------------------------------------------------------------

// One setting, as a row. The settings concept is a single wide row and a
// person reads it as a list of decisions, so the console transposes it: one
// row per knob, grouped by the question it answers.
export const SETTING_CONCEPT: ConceptLike = {
  id: "admin.setting",
  entity: "setting",
  displayCard: {
    primary: "setting",
    secondary: "value",
    tertiary: "group",
  },
};

export interface SettingRow extends Record<string, unknown> {
  id: string;
  group: string;
  setting: string;
  value: string;
}

interface SettingSpec {
  readonly group: string;
  readonly field: string;
  readonly setting: string;
  // How to say the stored value in the operator's units. Seconds and days are
  // how the row stores a lifetime; minutes and days are how an operator sets
  // one, and 0 means "whatever the environment says" rather than "no time at
  // all".
  readonly render?: (raw: unknown) => string;
}

function seconds(unit: "minutes" | "days"): (raw: unknown) => string {
  const per = unit === "minutes" ? 60 : 86400;
  return (raw) => {
    const n = typeof raw === "number" ? raw : 0;
    if (n <= 0) return "inherited from the environment";
    return `${n / per} ${unit}`;
  };
}

function days(raw: unknown): string {
  const n = typeof raw === "number" ? raw : 0;
  return n <= 0 ? "inherited from the environment" : `${n} days`;
}

function present(raw: unknown): string {
  if (typeof raw === "string") return raw === "" ? "not set" : raw;
  if (typeof raw === "boolean") return raw ? "on" : "off";
  if (typeof raw === "number") return String(raw);
  return "not set";
}

// The knobs, in the order an operator asks about them: who gets in, how long
// they stay, what the cluster looks like, and the one kill switch.
const SETTINGS: readonly SettingSpec[] = [
  { group: "Registration", field: "registrationMode", setting: "Who may register" },
  { group: "Registration", field: "registrationDomains", setting: "Allowed email domains" },
  { group: "Registration", field: "internalDomains", setting: "Internal email domains" },
  { group: "Registration", field: "internalDefaultRole", setting: "Role granted to internal users" },
  { group: "Registration", field: "accessRequestNotifyEmails", setting: "Waitlist notifications go to" },
  {
    group: "Lifetimes",
    field: "accessTokenTTLSeconds",
    setting: "Access token lifetime",
    render: seconds("minutes"),
  },
  {
    group: "Lifetimes",
    field: "refreshTokenTTLSeconds",
    setting: "Refresh token lifetime",
    render: seconds("days"),
  },
  {
    group: "Lifetimes",
    field: "magicLinkTTLSeconds",
    setting: "Magic-link lifetime",
    render: seconds("minutes"),
  },
  { group: "Lifetimes", field: "invitationTTLDays", setting: "Invitation lifetime", render: days },
  { group: "Lifetimes", field: "refreshCookieSameSite", setting: "Refresh cookie SameSite" },
  { group: "Identity", field: "clusterDomain", setting: "Cluster domain" },
  { group: "Identity", field: "brandName", setting: "Brand name" },
  { group: "Identity", field: "brandPrimaryColor", setting: "Brand colour" },
  { group: "Identity", field: "bootstrapEmail", setting: "Bootstrapped by" },
  { group: "Identity", field: "bootstrappedAt", setting: "Bootstrapped at" },
  {
    group: "Automations",
    field: "authoredAutomationsEnabled",
    setting: "Authored automations",
  },
];

export function settingRows(row: Row | null): SettingRow[] {
  if (row === null) return [];
  const out: SettingRow[] = [];
  for (const spec of SETTINGS) {
    const raw = row[spec.field];
    out.push({
      id: spec.field,
      group: spec.group,
      setting: spec.setting,
      value: spec.render ? spec.render(raw) : present(raw),
    });
  }
  return out;
}

// The two data-URI brand fields are deliberately absent from SETTINGS: a
// megabyte of base64 in a table cell is not a value anyone reads. The page
// reports whether one is set instead.
export function brandAssetSummary(row: Row | null): string {
  if (row === null) return "";
  const logo = typeof row["brandLogoDataURI"] === "string" && row["brandLogoDataURI"] !== "";
  const icon = typeof row["brandIconDataURI"] === "string" && row["brandIconDataURI"] !== "";
  if (logo && icon) return "a logo and an icon are uploaded";
  if (logo) return "a logo is uploaded, no icon";
  if (icon) return "an icon is uploaded, no logo";
  return "no brand images uploaded";
}
