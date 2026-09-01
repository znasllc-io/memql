// A handoff that has to survive a window reload (design 4.4).
//
// Opening the checkout folder in this window restarts the extension host, so
// the request is parked in globalState with a short TTL and taken exactly once
// on the next activation. The TTL is what keeps a stale request from opening a
// file an hour later in a window nobody asked it to.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4251

import type { OpenRequest } from "./openRequest.js";

export const PENDING_HANDOFF_KEY = "memql.handoff.pending";
export const PENDING_HANDOFF_TTL_MS = 120_000;

export interface PendingHandoff {
  request: OpenRequest;
  storedAt: number;
}

interface MementoLike {
  get<T>(key: string): T | undefined;
  update(key: string, value: unknown): Thenable<void>;
}

export function storePending(memento: Pick<MementoLike, "update">, request: OpenRequest, now: number): Thenable<void> {
  const pending: PendingHandoff = { request, storedAt: now };
  return memento.update(PENDING_HANDOFF_KEY, pending);
}

export async function takePending(memento: MementoLike, now: number): Promise<OpenRequest | undefined> {
  const raw = memento.get<unknown>(PENDING_HANDOFF_KEY);
  await memento.update(PENDING_HANDOFF_KEY, undefined);
  if (!isPending(raw)) return undefined;
  if (now - raw.storedAt > PENDING_HANDOFF_TTL_MS || now < raw.storedAt) return undefined;
  return raw.request;
}

/**
 * Whether globalState is holding a request THIS build understands.
 *
 * A parked request is JSON that outlived the process that wrote it, so it is
 * validated rather than cast -- and validated against the shape of the whole
 * union, not against a common subset of it. A stored request whose `target`
 * says `artifact` but carries no `id` is not a construct request with a field
 * missing; it is not a request at all, and replaying it as one would open
 * something nobody asked for.
 *
 * NO TOLERANCE FOR AN OLDER SHAPE, and none is needed. A parked request lives
 * for two minutes (PENDING_HANDOFF_TTL_MS), so the only way one written by a
 * build predating `target` reaches here is an extension upgrade landing inside
 * that window -- where the honest answer is the one this gives: drop it, and
 * the operator clicks the link again.
 */
function isPending(value: unknown): value is PendingHandoff {
  if (value === null || typeof value !== "object") return false;
  const v = value as { request?: unknown; storedAt?: unknown };
  if (typeof v.storedAt !== "number") return false;
  const r = v.request as
    | { version?: unknown; target?: unknown; domain?: unknown; kind?: unknown; name?: unknown; id?: unknown }
    | null
    | undefined;
  if (r === undefined || r === null) return false;
  if (r.version !== "1" || typeof r.domain !== "string" || typeof r.kind !== "string") return false;
  if (r.target === "artifact") return r.kind === "artifact" && typeof r.id === "string";
  return r.target === "construct" && typeof r.name === "string";
}
