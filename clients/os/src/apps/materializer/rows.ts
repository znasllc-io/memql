import { rowBool, rowNumber, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

// rows.ts -- the row shapes this app reads, and the two things about
// reading them that go wrong by default.
//
// ===========================================================================
// ABSENT IS NOT FALSE, AND ABSENT IS NOT ZERO
// ===========================================================================
// The SDK's `rowBool` answers FALSE for a missing key and `rowNumber`
// answers 0. Both are right for a flag somebody set and a count of
// things, and both are wrong here in one specific place that matters
// more than the rest of this file:
//
// `provenanceEmbedded` is the answer to "does this file carry its own
// provenance", and there are THREE states -- yes, no, and not decided
// yet. A composition still composing has not reached the stamp step, so
// nothing has answered; reading that as `false` puts "the record is the
// only copy" on a file that will carry its own the moment it is written.
// So it is read through `optionalBool`, which keeps the third state, and
// the surface renders nothing rather than a wrong claim.
//
// ===========================================================================
// A HEARTBEAT IS NOT NEWS
// ===========================================================================
// `compositionFingerprint` names what a PERSON would call a change. It
// deliberately excludes `runId`, which is re-stamped on every attempt,
// and `statement`, which never moves after creation. The counters a
// running composition writes are not on the card at all, so there is
// nothing here that ticks on a timer -- which is the state this app is
// lucky to be in, and the reason to check before adding a field.

export interface CompositionRow {
  id: string;
  createdAt: string;
  ownerUserId: string;
  name: string;
  statement: string;
  status: CompositionStatus;
  format: string;
  templateId: string;
  outputFileId: string;
  folderId: string;
  accountIds: string[];
  goalId: string;
  runId: string;
  recipeId: string;
  /** null while nothing has answered yet -- see the header. */
  provenanceEmbedded: boolean | null;
  provenanceNote: string;
  deployableKind: string;
  failureReason: string;
  archived: boolean;
}

export type CompositionStatus =
  | "draft"
  | "composing"
  | "rendering"
  | "ready"
  | "failed"
  | "cancelled"
  | "";

export interface TemplateRow {
  id: string;
  createdAt: string;
  name: string;
  description: string;
  fileId: string;
  format: string;
  placeholders: PlaceholderRow[];
  accountIds: string[];
  archived: boolean;
}

export interface PlaceholderRow {
  name: string;
  description: string;
  required: boolean;
}

export interface RecipeRow {
  id: string;
  createdAt: string;
  name: string;
  description: string;
  sourceSelectors: SelectorRow[];
  templateId: string;
  format: string;
  folderId: string;
  accountIds: string[];
  lastRunAt: string;
  runCount: number;
  archived: boolean;
}

export interface SelectorRow {
  kind: string;
  selector: string;
  label: string;
}

/** One entry of a composition's `sources`, as the record stored it. */
export interface SourceRow {
  kind: string;
  ref: string;
  label: string;
  capturedAt: string;
}

/** One entry of `modelsUsed`. */
export interface ModelRow {
  provider: string;
  model: string;
  calls: number;
  tokens: number;
}

export function compositionFromRow(row: Row): CompositionRow {
  return {
    id: rowString(row, "id"),
    createdAt: rowString(row, "createdAt"),
    ownerUserId: rowString(row, "ownerUserId"),
    name: rowString(row, "name"),
    statement: rowString(row, "statement"),
    status: (rowString(row, "status") || "") as CompositionStatus,
    format: rowString(row, "format"),
    templateId: rowString(row, "templateId"),
    outputFileId: rowString(row, "outputFileId"),
    folderId: rowString(row, "folderId"),
    accountIds: stringArray(row, "accountIds"),
    goalId: rowString(row, "goalId"),
    runId: rowString(row, "runId"),
    recipeId: rowString(row, "recipeId"),
    provenanceEmbedded: optionalBool(row, "provenanceEmbedded"),
    provenanceNote: rowString(row, "provenanceNote"),
    deployableKind: rowString(row, "deployableKind"),
    failureReason: rowString(row, "failureReason"),
    archived: rowBool(row, "archived"),
  };
}

export function templateFromRow(row: Row): TemplateRow {
  return {
    id: rowString(row, "id"),
    createdAt: rowString(row, "createdAt"),
    name: rowString(row, "name"),
    description: rowString(row, "description"),
    fileId: rowString(row, "fileId"),
    format: rowString(row, "format"),
    placeholders: placeholdersOf(row),
    accountIds: stringArray(row, "accountIds"),
    archived: rowBool(row, "archived"),
  };
}

export function recipeFromRow(row: Row): RecipeRow {
  return {
    id: rowString(row, "id"),
    createdAt: rowString(row, "createdAt"),
    name: rowString(row, "name"),
    description: rowString(row, "description"),
    sourceSelectors: selectorsOf(row),
    templateId: rowString(row, "templateId"),
    format: rowString(row, "format"),
    folderId: rowString(row, "folderId"),
    accountIds: stringArray(row, "accountIds"),
    lastRunAt: rowString(row, "lastRunAt"),
    runCount: rowNumber(row, "runCount"),
    archived: rowBool(row, "archived"),
  };
}

/**
 * The arrival cue's fingerprint: what a PERSON would call a change.
 *
 * `runId` is out because it is re-stamped on every attempt of one
 * composition, which is a fact about the machinery rather than news.
 * `createdAt` is out because MemQL is append-only, so it moves on every
 * write -- naming it would ring every row on every status transition,
 * twice.
 */
export function compositionFingerprint(c: CompositionRow): string {
  return [
    c.name,
    c.status,
    c.format,
    c.outputFileId,
    c.templateId,
    String(c.provenanceEmbedded),
    c.deployableKind,
    c.failureReason,
    String(c.archived),
  ].join("|");
}

export function templateFingerprint(t: TemplateRow): string {
  return [t.name, t.description, t.fileId, t.format, String(t.archived)].join("|");
}

/**
 * A recipe's fingerprint EXCLUDES `lastRunAt` and `runCount`.
 *
 * Both move every time the recipe runs, and a recipe on a schedule would
 * move them forever -- so naming either would strobe the list on that
 * schedule's own cycle. They are rendered CONTINUOUSLY beside the row
 * instead, which is the right home for something always true and never
 * news (the heartbeat rule, clients/os/README.md).
 */
export function recipeFingerprint(r: RecipeRow): string {
  return [r.name, r.description, r.templateId, r.format, String(r.archived)].join("|");
}

/** Newest first, by the id's own creation order, stable. */
export function newestFirst<T extends { createdAt: string }>(rows: T[]): T[] {
  return [...rows].sort((a, b) => b.createdAt.localeCompare(a.createdAt));
}

/**
 * `rowBool` answers `false` for a missing key, which collapses "no" and
 * "nothing has answered yet". This keeps them apart.
 */
export function optionalBool(row: Row, key: string): boolean | null {
  const raw = (row as Record<string, unknown>)[key];
  if (typeof raw === "boolean") return raw;
  return null;
}

export function sourcesOf(row: Row): SourceRow[] {
  return objectArray(row, "sources").map((s) => ({
    kind: str(s["kind"]),
    ref: str(s["ref"]),
    label: str(s["label"]),
    capturedAt: str(s["capturedAt"]),
  }));
}

export function modelsOf(row: Row): ModelRow[] {
  return objectArray(row, "modelsUsed").map((m) => ({
    provider: str(m["provider"]),
    model: str(m["model"]),
    calls: num(m["calls"]),
    tokens: num(m["tokens"]),
  }));
}

function placeholdersOf(row: Row): PlaceholderRow[] {
  return objectArray(row, "placeholders").map((p) => ({
    name: str(p["name"]),
    description: str(p["description"]),
    required: p["required"] === true,
  }));
}

function selectorsOf(row: Row): SelectorRow[] {
  return objectArray(row, "sourceSelectors").map((s) => ({
    kind: str(s["kind"]),
    selector: str(s["selector"]),
    label: str(s["label"]),
  }));
}

function objectArray(row: Row, key: string): Record<string, unknown>[] {
  const raw = (row as Record<string, unknown>)[key];
  if (!Array.isArray(raw)) return [];
  return raw.filter((item): item is Record<string, unknown> => typeof item === "object" && item !== null);
}

function stringArray(row: Row, key: string): string[] {
  const raw = (row as Record<string, unknown>)[key];
  if (!Array.isArray(raw)) return [];
  return raw.filter((v): v is string => typeof v === "string" && v.trim() !== "");
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

function num(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}
