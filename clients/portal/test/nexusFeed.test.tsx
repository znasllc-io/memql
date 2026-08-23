// The Nexus feed: how a goal's world gets in and stays current.
//
// Two altitudes, and the split is the point. The FOLD is pure (src/nexus/
// feed/merge.ts), so the invariants that are really about a reducer --
// a duplicate `created` leaves one node, an out-of-order copy does not roll a
// row backwards, a row from someone else's goal is not in this one -- are
// asserted directly on it. The HOOK's own behaviour -- every event resolved
// through the authorized read, a refused read dropped, the seed-then-follow
// race -- needs the wiring, so those run against a fake cluster.
//
// The design's section 1 names three landmines in this feed and each one has
// a test here by name.

import { describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import {
  Result,
  type AccessSummary,
  type Connection,
  type Event,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { useGoalWorld } from "../src/nexus/feed/useGoalWorld";
import {
  EMPTY_FEED,
  belongsToPlan,
  foldRow,
  supersedes,
  watermark,
} from "../src/nexus/feed/merge";
import { asQueryClient } from "./support/queryFake";

const PLAN_ID = "plan-1";
const BUNDLE_ID = "bundle-1";

function taskRow(id: string, over: Record<string, unknown> = {}): Row {
  return {
    id,
    planId: PLAN_ID,
    category: "semantic",
    kind: id,
    status: "queued",
    seq: 1,
    phase: "gather",
    createdAt: "2026-08-20T09:00:00Z",
    ...over,
  };
}

describe("the fold", () => {
  it("leaves one node for a duplicate created (the event fires on updates too)", () => {
    // graph.node.created fires on EVERY write including updates
    // (component/memql/executor_mutation.go), so the same row arrives twice.
    let state = foldRow(EMPTY_FEED, "task", taskRow("t1"));
    state = foldRow(state, "task", taskRow("t1"));
    expect(state.tasks.size).toBe(1);
  });

  it("does not roll a row backwards when copies arrive out of order", () => {
    const started = taskRow("t1", { status: "running", startedAt: "2026-08-20T09:05:00Z" });
    const finished = taskRow("t1", {
      status: "succeeded",
      startedAt: "2026-08-20T09:05:00Z",
      completedAt: "2026-08-20T09:09:00Z",
    });

    // Newest first, then the older copy settles late.
    let state = foldRow(EMPTY_FEED, "task", finished);
    state = foldRow(state, "task", started);
    expect(state.tasks.get("t1")?.["status"]).toBe("succeeded");

    // ...and the ordinary direction still updates.
    let forward = foldRow(EMPTY_FEED, "task", started);
    forward = foldRow(forward, "task", finished);
    expect(forward.tasks.get("t1")?.["status"]).toBe("succeeded");
  });

  it("reads a watermark off each concept's own latest dated transition", () => {
    expect(watermark("task", taskRow("t1"))).toBe("2026-08-20T09:00:00Z");
    expect(watermark("task", taskRow("t1", { completedAt: "2026-08-20T10:00:00Z" }))).toBe(
      "2026-08-20T10:00:00Z",
    );
    expect(
      watermark("bundle", { id: BUNDLE_ID, createdAt: "a", activatedAt: "b", retiredAt: "c" }),
    ).toBe("c");
    // A copy with NO watermark always wins -- otherwise a row whose
    // projection dropped the timestamps could never be updated again.
    expect(supersedes("task", { id: "t1" }, taskRow("t1"))).toBe(true);
  });

  it("keeps rows from other goals out", () => {
    expect(belongsToPlan("task", taskRow("t1"), PLAN_ID, "")).toBe(true);
    expect(belongsToPlan("task", taskRow("t1", { planId: "someone-else" }), PLAN_ID, "")).toBe(false);

    // An agent's pointer is NESTED under `lineage`, and the flattened CDC
    // envelope keeps the nested object rather than hoisting its leaves.
    expect(
      belongsToPlan("agent", { id: "a1", lineage: { originatingPlanId: PLAN_ID } }, PLAN_ID, ""),
    ).toBe(true);
    expect(belongsToPlan("agent", { id: "a1", "lineage.originatingPlanId": PLAN_ID }, PLAN_ID, "")).toBe(
      false,
    );

    // A construct points at its BUNDLE, so it cannot be placed before the
    // bundle is known -- and is refused rather than guessed at.
    expect(belongsToPlan("construct", { id: "c1", bundleId: BUNDLE_ID }, PLAN_ID, "")).toBe(false);
    expect(belongsToPlan("construct", { id: "c1", bundleId: BUNDLE_ID }, PLAN_ID, BUNDLE_ID)).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// The hook, against a fake cluster.
// ---------------------------------------------------------------------------

const ACCESS: AccessSummary = {
  requestId: "r1",
  userId: "user-1",
  primaryEmail: "operator@example.test",
  clusterRole: "owner",
  sessionId: "s1",
  displayName: "Operator",
};

const PLAN_ROW: Row = {
  id: PLAN_ID,
  goal: "Build a spring catalog",
  kind: "userGoal",
  status: "running",
  requestedBy: "user-1",
  ownerAgentId: "",
  createdAt: "2026-08-20T09:00:00Z",
  startedAt: "2026-08-20T09:01:00Z",
};

interface Deferred<T> {
  promise: Promise<T>;
  resolve: (value: T) => void;
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

interface FeedHarness {
  query: unknown;
  subscriptions: unknown;
  emit: (concept: string, event: Event) => void;
  // Whether the follow has opened yet. The race test has to emit BEFORE the
  // seed settles, so it cannot wait on anything the seed produces.
  subscribed: () => boolean;
  rowReads: string[];
}

function harness(
  options: {
    // Rows the authoritative single-row read answers with, by id.
    rows?: Record<string, Row>;
    // Ids whose re-read is REFUSED -- the answer a caller gets for a row
    // they may not see.
    refuse?: readonly string[];
    // Holds the seed's task read open so a follow event can land first.
    gateTasks?: Deferred<Result>;
    plan?: Row;
  } = {},
): FeedHarness {
  const handlers = new Map<string, (event: Event) => void>();
  const rowReads: string[] = [];
  const empty = new Result({ bundle: { nodes: [] } });

  const query = asQueryClient({
    listConcepts: vi.fn(async () => []),
    getMyAccess: vi.fn(async () => ACCESS),
    executeNamed: vi.fn(async (name: string, call: string) => {
      if (name === "conceptRow") {
        const id = /id==(\S+)/.exec(call)?.[1] ?? "";
        rowReads.push(id);
        if ((options.refuse ?? []).includes(id)) {
          throw new Error(`refused: ${id}`);
        }
        const row = options.rows?.[id];
        // A read returns the NESTED shape; the hook flattens it, and a test
        // that handed it a flat row would not exercise that.
        return row === undefined
          ? new Result({ bundle: { nodes: [] } })
          : new Result({
              bundle: { nodes: [{ id: String(row["id"] ?? ""), concept: "c", payload: row }] },
            });
      }
      if (name === "planById") {
        return new Result({ bundle: { nodes: [{ id: PLAN_ID, payload: options.plan ?? PLAN_ROW }] } });
      }
      if (name === "tasksForPlan" && options.gateTasks !== undefined) {
        return options.gateTasks.promise;
      }
      return empty;
    }),
  });

  const subscriptions = {
    subscribeGraph: (fn: (event: Event) => void, opts?: { concept?: string }) => {
      const concept = opts?.concept ?? "";
      handlers.set(concept, fn);
      return () => handlers.delete(concept);
    },
  };

  return {
    query,
    subscriptions,
    rowReads,
    subscribed: () => handlers.size > 0,
    emit: (concept, event) => {
      act(() => {
        handlers.get(concept)?.(event);
      });
    },
  };
}

// A probe component: renders what the hook returns as text, so the assertions
// read the DOM rather than reaching into a hook result object. Same shape the
// rest of this suite uses.
function Probe({ planId }: { planId: string }): React.ReactElement {
  const state = useGoalWorld(planId);
  return (
    <div>
      <span data-testid="tasks">{state.world.tasks.map((t) => t.id).sort().join(",")}</span>
      <span data-testid="goal">{state.world.plan?.goal ?? ""}</span>
      <span data-testid="refused">{state.refused}</span>
      <span data-testid="missing">{String(state.missing)}</span>
    </div>
  );
}

function renderFeed(h: FeedHarness, planId = PLAN_ID) {
  const dial = vi.fn(
    async () =>
      ({
        nodeId: "bff-test",
        serverVersion: "0.0.0-test",
        query: h.query,
        subscriptions: h.subscriptions,
        close: vi.fn(),
        done: vi.fn(() => new Promise<void>(() => {})),
      }) as unknown as Connection,
  ) as unknown as typeof Connection.dial;

  return render(
    <MemoryRouter>
      <ClusterProvider dial={dial}>
        <Probe planId={planId} />
      </ClusterProvider>
    </MemoryRouter>,
  );
}

const CREATED: Omit<Event, "payload" | "payloadOmitted"> = {
  subscriptionId: "sub",
  kind: "NODE_CREATED",
  timestamp: null,
};

describe("useGoalWorld", () => {
  it("resolves an ID-ONLY event through the authorized read", async () => {
    const h = harness({ rows: { t9: taskRow("t9") } });
    renderFeed(h);
    await waitFor(() => expect(screen.getByTestId("goal").textContent).not.toBe(""));

    h.emit("v1:planner:task", {
      ...CREATED,
      // No payload at all -- the granted-tier shape (memql#4309).
      payload: { id: "t9", concept: "v1:planner:task" },
      payloadOmitted: true,
    });

    await waitFor(() => expect(screen.getByTestId("tasks").textContent).toBe("t9"));
    expect(h.rowReads).toContain("t9");
  });

  it("re-reads a FULL-payload event too, rather than trusting the payload", async () => {
    // Design D6: one code path. The proof is that a full-payload event whose
    // row the read does not return leaves NO node -- if the payload had been
    // trusted, the task would be on the map.
    const h = harness({ rows: {} });
    renderFeed(h);
    await waitFor(() => expect(screen.getByTestId("goal").textContent).not.toBe(""));

    h.emit("v1:planner:task", {
      ...CREATED,
      payload: taskRow("t-ghost"),
      payloadOmitted: false,
    });

    await waitFor(() => expect(h.rowReads).toContain("t-ghost"));
    expect(screen.getByTestId("tasks").textContent).toBe("");
  });

  it("drops an event whose re-read is refused", async () => {
    const h = harness({ rows: { "t-secret": taskRow("t-secret") }, refuse: ["t-secret"] });
    renderFeed(h);
    await waitFor(() => expect(screen.getByTestId("goal").textContent).not.toBe(""));

    h.emit("v1:planner:task", {
      ...CREATED,
      payload: { id: "t-secret", concept: "v1:planner:task" },
      payloadOmitted: true,
    });

    await waitFor(() => expect(h.rowReads).toContain("t-secret"));
    expect(screen.getByTestId("tasks").textContent).toBe("");
  });

  it("leaves exactly one node when a follow event beats the seed", async () => {
    // The third landmine: CDC has no replay, so the world is seeded and then
    // followed -- and the seed can settle AFTER the first events land.
    const gate = deferred<Result>();
    const h = harness({ rows: { t1: taskRow("t1") }, gateTasks: gate });
    renderFeed(h);
    // Wait for the FOLLOW, not for the seed: the seed is deliberately held
    // open below, and waiting on anything it produces would defeat the test.
    await waitFor(() => expect(h.subscribed()).toBe(true));

    h.emit("v1:planner:task", {
      ...CREATED,
      payload: { id: "t1", concept: "v1:planner:task" },
      payloadOmitted: true,
    });
    await waitFor(() => expect(screen.getByTestId("tasks").textContent).toBe("t1"));

    // Now the seed settles, carrying the same row.
    await act(async () => {
      gate.resolve(new Result({ bundle: { nodes: [{ id: "t1", payload: taskRow("t1") }] } }));
      await Promise.resolve();
    });

    await waitFor(() => expect(screen.getByTestId("tasks").textContent).toBe("t1"));
  });

  it("refuses to draw someone else's goal", async () => {
    const h = harness({ plan: { ...PLAN_ROW, requestedBy: "another-user" } });
    renderFeed(h);
    await waitFor(() => expect(screen.getByTestId("refused").textContent).not.toBe(""));
    expect(screen.getByTestId("goal").textContent).toBe("");
  });

  it("tells a missing goal apart from a refused one", async () => {
    const h = harness();
    // A plan read that finds nothing.
    (h.query as { executeNamed: ReturnType<typeof vi.fn> }).executeNamed = vi.fn(
      async (name: string) =>
        name === "planById" ? new Result({ bundle: { nodes: [] } }) : new Result({ bundle: { nodes: [] } }),
    );
    renderFeed(h);
    await waitFor(() => expect(screen.getByTestId("missing").textContent).toBe("true"));
    expect(screen.getByTestId("refused").textContent).toBe("");
  });
});
