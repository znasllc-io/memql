import type { ReactNode } from "react";
import { vi } from "vitest";
import { Result, type Concept, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";

// The Concepts app's harness: a connection-shaped double, the way Fleet's is.
//
// Two seams matter here and neither is a generated query method, which is why
// this harness is not a copy of Fleet's:
//
//   - `query.subscribeConceptRegistry(onDelta, opts)` -- the follow
//     subscription. `pushDelta` is the test's hand on it.
//   - `query.executeNamed(name, call, opts)` -- what `browseConceptPage` and
//     `getRowByConceptAndId` dispatch through. Answering it with a BUNDLE
//     envelope is load-bearing: `rawNodes()` reads `payload.bundle.nodes`
//     and the cursor comes off `payload.meta`, so a `{ data: rows }` result
//     (Fleet's shape, for shaped reads) would make every walk look empty.

export function bundleResult(rows: Row[], cursor = ""): Result {
  return new Result({
    bundle: { nodes: rows },
    ...(cursor === "" ? {} : { meta: { cursor } }),
  } as never);
}

export interface RegistryDelta {
  generation: number;
  added: Concept[];
  removed: string[];
  reset: boolean;
}

export interface FakeEvent {
  subscriptionId: string;
  kind: string;
  timestamp: Date | null;
  payload: Row | null;
  payloadOmitted: boolean;
  seq: number;
  gapBefore: boolean;
}

export interface FakeConnection {
  query: {
    subscribeConceptRegistry: ReturnType<typeof vi.fn>;
    executeNamed: ReturnType<typeof vi.fn>;
  };
  subscriptions: {
    subscribeGraph: (h: (e: FakeEvent) => void, opts: { concept?: string }) => () => void;
    emit: (concept: string, payload: Row | null, over?: Partial<FakeEvent>) => void;
  };
  dispatcher: { sendAndWait: ReturnType<typeof vi.fn> };
  /** Push a registry delta to every live follower. */
  pushDelta: (delta: RegistryDelta) => void;
  /** How many follows are open right now -- a re-snapshot opens a second. */
  followCount: () => number;
  /** Registry follows that have been torn down. */
  unsubscribed: () => number;
}

export function fakeConnection(
  opts: {
    /** Pages answered in order; the last is reused once exhausted. */
    pages?: { rows: Row[]; cursor: string }[];
    /** Make every walk fail with this sentence. */
    walkError?: string;
  } = {},
): FakeConnection {
  const followers = new Set<(d: RegistryDelta) => void>();
  let unsubscribes = 0;
  const graphHandlers = new Map<string, Set<(e: FakeEvent) => void>>();
  const pages = opts.pages ?? [{ rows: [], cursor: "" }];
  let pageIndex = 0;

  return {
    query: {
      subscribeConceptRegistry: vi.fn((onDelta: (d: RegistryDelta) => void) => {
        followers.add(onDelta);
        return {
          unsubscribe: () => {
            if (followers.delete(onDelta)) unsubscribes += 1;
          },
        };
      }),
      executeNamed: vi.fn(async (_name: string, _call: string) => {
        if (opts.walkError !== undefined) throw new Error(opts.walkError);
        const page = pages[Math.min(pageIndex, pages.length - 1)] ?? { rows: [], cursor: "" };
        pageIndex += 1;
        return bundleResult(page.rows, page.cursor);
      }),
    },
    subscriptions: {
      subscribeGraph(handler, subOpts) {
        const concept = subOpts.concept ?? "*";
        const set = graphHandlers.get(concept) ?? new Set();
        set.add(handler);
        graphHandlers.set(concept, set);
        return () => set.delete(handler);
      },
      emit(concept, payload, over = {}) {
        for (const handler of graphHandlers.get(concept) ?? []) {
          handler({
            subscriptionId: "sub-1",
            kind: "NODE_CREATED",
            timestamp: new Date(),
            payload,
            payloadOmitted: false,
            seq: 0,
            gapBefore: false,
            ...over,
          });
        }
      },
    },
    dispatcher: { sendAndWait: vi.fn() },
    pushDelta(delta) {
      for (const follower of [...followers]) follower(delta);
    },
    followCount: () => followers.size,
    unsubscribed: () => unsubscribes,
  };
}

/** A concept descriptor with sane defaults, overridable field by field. */
export function conceptOf(over: Partial<Concept> & { id: string }): Concept {
  const parts = over.id.split(":");
  return {
    version: "v1",
    domain: parts[1] ?? "",
    entity: parts[2] ?? "",
    description: "",
    type: "",
    fields: [],
    relationships: [],
    ...over,
  } as Concept;
}

/** A raw browse node: intrinsics at the top, payload nested. */
export function nodeOf(id: string, payload: Record<string, unknown>): Row {
  return { id, concept: "v1:test:thing", createdAt: "2026-09-01T00:00:00Z", payload };
}

export function withSession(
  children: ReactNode,
  overrides: { userId?: string; domain?: string; clusterRole?: string } = {},
) {
  const config: OsRuntimeConfig = {
    ...UNKNOWN_RUNTIME_CONFIG,
    domain: overrides.domain ?? "memql.example.com",
  };
  return (
    <SessionProvider
      value={{
        access: {
          userId: overrides.userId ?? "v1:identity:user:me",
          primaryEmail: "me@example.com",
          clusterRole: overrides.clusterRole ?? "owner",
        },
        config,
      }}
    >
      {children}
    </SessionProvider>
  );
}
