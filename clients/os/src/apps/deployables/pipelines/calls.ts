import { rowString, type QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../../kit/rows";
import { copyFor } from "../packages/refusals";
import type { Compute, Delivery } from "./rows";

// Every pipelines write and on-demand read this app makes, in one place
// (epic memql#5479), through the generated typed builders -- which are the
// point of sdk-gen, and which quote every value.
//
// A BUILTIN'S REFUSAL ARRIVES AS AN ERROR whose message carries the code the
// engine refused with (`pipeline_nothing_to_rerun: ...`, a grant's
// `credential_revoked: ...`). `problemFrom` reads the code back out so the
// surface renders it through `ProblemNotice` -- the copy keyed by code, and
// the server's sentence verbatim beneath -- in place, beside the control that
// produced it. A message carrying no code this build knows keeps its own
// words under the neutral heading, never a guessed one.

export interface Problem {
  code: string;
  message: string;
  scope: string;
}

const CODED = /(?:^|[^a-z_])([a-z]+(?:_[a-z]+)+)(?: \(([^)]*)\))?: ([\s\S]*)$/;

/** A refusal's code, scope and sentence, read back out of an error. */
export function problemFrom(err: unknown): Problem {
  const message = (err instanceof Error ? err.message : String(err)).trim();
  // The FIRST known code wins: an outer wrapper ("builtin failed: ...") is
  // never one, and the engine's own code is the one the copy is keyed by.
  let rest = message;
  for (let guard = 0; guard < 4; guard++) {
    const m = CODED.exec(rest);
    if (!m) break;
    const code = m[1]!;
    if (copyFor(code) !== null || code.startsWith("pipeline_")) {
      return { code, scope: m[2] ?? "", message: (m[3] ?? "").trim() };
    }
    rest = m[3] ?? "";
  }
  return { code: "", scope: "", message };
}

function firstRow(result: { rows(): Row[] }): Row | null {
  const row = result.rows()[0];
  return row ? flatten(row) : null;
}

export async function rerunRun(query: QueryClient, runId: string, failedOnly: boolean): Promise<{ runId: string; attempt: number }> {
  const result = await query.pipelinesRerun({ runId, failedOnly });
  const row = firstRow(result);
  const attempt = row ? Number(row["attempt"] ?? 0) : 0;
  return { runId: row ? rowString(row, "runId") : "", attempt: Number.isFinite(attempt) ? attempt : 0 };
}

export async function cancelRun(query: QueryClient, runId: string): Promise<void> {
  await query.pipelinesCancel({ runId });
}

export interface ConnectInput {
  packageId: string;
  delivery: Delivery;
  compute: Compute;
  /** The globalSecret NAMES the steps may resolve: the allowlist. Never values. */
  secretNames: readonly string[];
}

export async function connectPipeline(query: QueryClient, input: ConnectInput): Promise<{ pipelineId: string; reconnected: boolean }> {
  const result = await query.pipelinesConnect({
    packageId: input.packageId,
    delivery: input.delivery,
    compute: input.compute,
    secretNames: [...input.secretNames],
  });
  const row = firstRow(result);
  return { pipelineId: row ? rowString(row, "pipelineId") : "", reconnected: row?.["reconnected"] === true };
}

export async function disconnectPipeline(query: QueryClient, pipelineId: string): Promise<void> {
  await query.pipelinesDisconnect({ pipelineId });
}

// ---------------------------------------------------------------------------
// The preview: what connecting a source would act on (pipelinesPreview)
// ---------------------------------------------------------------------------

export interface PreviewStep {
  execution: string;
  platform: string;
  requiresFleet: boolean;
  name: string;
  packages: string;
  only: string;
  shards: number;
  bucket: string;
  needs: string[];
  secrets: string[];
  services: string[];
}

export interface PreviewStage {
  name: string;
  /** Events and modes the stage runs for; empty is every run. */
  on: string[];
  /** A notify stage's channel; "" for a stage of steps. */
  channel: string;
  steps: PreviewStep[];
}

export interface PipelinePreview {
  repository: string;
  defaultBranch: string;
  sha: string;
  name: string;
  checkName: string;
  stages: PreviewStage[];
  needs: string[];
  secrets: string[];
  suggestedDelivery: Delivery;
  existing: { pipelineId: string; status: "active" | "disconnected"; delivery: Delivery; compute: Compute; secretNames: string[] } | null;
  refusal: Problem | null;
}

function strings(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((m): m is string => typeof m === "string") : [];
}

function objects(v: unknown): Row[] {
  return Array.isArray(v) ? v.filter((m): m is Row => m !== null && typeof m === "object" && !Array.isArray(m)) : [];
}

export function previewFromRow(row: Row): PipelinePreview {
  const existing = row["existing"] && typeof row["existing"] === "object" ? (row["existing"] as Row) : null;
  const refusal = row["refusal"] && typeof row["refusal"] === "object" ? (row["refusal"] as Row) : null;
  return {
    repository: rowString(row, "repository"),
    defaultBranch: rowString(row, "defaultBranch"),
    sha: rowString(row, "sha"),
    name: rowString(row, "name"),
    checkName: rowString(row, "checkName"),
    stages: objects(row["stages"]).map((st) => ({
      name: rowString(st, "name"),
      on: strings(st["on"]),
      channel: rowString(st, "channel"),
      steps: objects(st["steps"]).map((sp) => ({
        execution: rowString(sp, "execution"),
        platform: rowString(sp, "platform"),
        requiresFleet: sp["requiresFleet"] === true,
        name: rowString(sp, "name"),
        packages: rowString(sp, "packages"),
        only: rowString(sp, "only"),
        shards: typeof sp["shards"] === "number" ? (sp["shards"] as number) : 0,
        bucket: rowString(sp, "bucket"),
        needs: strings(sp["needs"]),
        secrets: strings(sp["secrets"]),
        services: strings(sp["services"]),
      })),
    })),
    needs: strings(row["needs"]),
    secrets: strings(row["secrets"]),
    suggestedDelivery: rowString(row, "suggestedDelivery") === "webhook" ? "webhook" : "poll",
    existing: existing === null ? null : {
      pipelineId: rowString(existing, "pipelineId"),
      status: rowString(existing, "status") === "disconnected" ? "disconnected" : "active",
      delivery: rowString(existing, "delivery") === "poll" ? "poll" : "webhook",
      compute: rowString(existing, "compute") === "cluster_and_fleet" ? "cluster_and_fleet" : "cluster",
      secretNames: strings(existing["secretNames"]),
    },
    refusal: refusal === null || rowString(refusal, "code") === "" ? null : {
      code: rowString(refusal, "code"),
      message: rowString(refusal, "message"),
      scope: rowString(refusal, "scope"),
    },
  };
}

/** Read what connecting `packageId` would act on. Writes nothing. Null when the cluster answered no row. */
export async function previewPipeline(query: QueryClient, packageId: string, signal?: AbortSignal): Promise<PipelinePreview | null> {
  const result = await query.pipelinesPreview({ packageId }, signal ? { signal } : undefined);
  const row = firstRow(result);
  return row ? previewFromRow(row) : null;
}

// ---------------------------------------------------------------------------
// Installations whose permissions lag the app's (pipelinesInstallations)
// ---------------------------------------------------------------------------

export interface LaggingInstallation {
  installationId: number;
  account: string;
  accountType: string;
  /** The installation's settings page on GitHub, where the change waits to be accepted. */
  htmlUrl: string;
  /** "permission:level", e.g. "checks:write". */
  missingPermissions: string[];
  suspended: boolean;
}

export async function readLaggingInstallations(query: QueryClient, signal?: AbortSignal): Promise<LaggingInstallation[]> {
  const result = await query.pipelinesInstallations({}, signal ? { signal } : undefined);
  const row = firstRow(result);
  if (!row) return [];
  return objects(row["installations"]).map((i) => ({
    installationId: typeof i["installationId"] === "number" ? (i["installationId"] as number) : Number(i["installationId"] ?? 0),
    account: rowString(i, "account"),
    accountType: rowString(i, "accountType"),
    htmlUrl: rowString(i, "htmlUrl"),
    missingPermissions: strings(i["missingPermissions"]),
    suspended: i["suspended"] === true,
  }));
}
