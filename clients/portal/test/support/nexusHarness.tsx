// A fake cluster serving one goal, for the Nexus page tests.
//
// Built on the SPRING CATALOG FIXTURE (src/nexus/scene/fixtures) rather than
// on rows typed out here, so the pages and the scene library are exercised
// against the same world -- a fixture that drifts from what the pages are
// tested with is a fixture that stops proving anything.
//
// The fake sits at executeNamed, the wire boundary every generated typed
// method dispatches through, which means these tests exercise the REAL
// composed call string rather than a hand-typed copy of it. Same decision
// artifacts.test.tsx and campaignAuthoring.test.tsx make.

import { vi } from "vitest";
import { act, render } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import {
  Result,
  type AccessSummary,
  type Concept,
  type Connection,
  type Event,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../../src/app/routes";
import { AuthProvider } from "../../src/auth/AuthProvider";
import { ClusterProvider } from "../../src/cluster/ClusterProvider";
import type { GoalWorld } from "../../src/nexus/scene/world";
import { springCatalogGoal } from "../../src/nexus/scene/fixtures";
import { asQueryClient } from "./queryFake";

export const OWNER_ID = "user-1";

export const ACCESS: AccessSummary = {
  requestId: "r1",
  userId: OWNER_ID,
  primaryEmail: "operator@example.test",
  clusterRole: "owner",
  sessionId: "s1",
  displayName: "Operator",
};

// The concept ids Nexus names, with display cards, so RowDetailDialog and the
// concept-browser link render the way they do in the product.
export const CONCEPTS: Concept[] = [
  { id: "v1:planner:plan", version: "v1", domain: "planner", entity: "plan", type: "concept", description: "A goal" },
  { id: "v1:planner:task", version: "v1", domain: "planner", entity: "task", type: "concept", description: "A step" },
  { id: "v1:agents:agent", version: "v1", domain: "agents", entity: "agent", type: "concept", description: "An agent" },
  { id: "v1:authoring:bundle", version: "v1", domain: "authoring", entity: "bundle", type: "concept", description: "A bundle" },
  { id: "v1:authoring:construct", version: "v1", domain: "authoring", entity: "construct", type: "concept", description: "A construct" },
  { id: "v1:authoring:dependencyEdge", version: "v1", domain: "authoring", entity: "dependencyEdge", type: "concept", description: "An edge" },
  { id: "v1:library:artifact", version: "v1", domain: "library", entity: "artifact", type: "concept", description: "An artifact" },
];

export const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
  domain: "memql.test",
};

// asRow turns a fixture record into the wire shape a query answers with.
//
// `hasTokenSpentSubscription` is DROPPED, and that is not tidying: it is a
// derived flag readPlan computes from whether the wire carried
// `tokenSpentSubscription` at all, and leaving both on the row would make a
// fixture that sets the number to 0 come back claiming the field exists.
// The receipt renders an absent line for one and a zero for the other, so
// the difference is exactly what a test about the receipt is about.
function asRow(record: Record<string, unknown>): Row {
  const out: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(record)) {
    if (key === "hasTokenSpentSubscription") continue;
    if (key === "tokenSpentSubscription" && record["hasTokenSpentSubscription"] !== true) continue;
    out[key] = value;
  }
  return out as Row;
}

export interface NexusHarness {
  query: unknown;
  subscriptions: unknown;
  // Every named call the page made, in order.
  calls: string[];
  emit: (concept: string, event: Event) => void;
  subscribed: () => boolean;
}

export interface NexusHarnessOptions {
  world?: GoalWorld;
  // The goals the picker lists. Defaults to the fixture's own plan alone.
  goals?: Row[];
  // Make a named write fail, to exercise the refusal path.
  failWrite?: string;
}

export function nexusHarness(options: NexusHarnessOptions = {}): NexusHarness {
  const world = options.world ?? springCatalogGoal();
  const calls: string[] = [];
  // A SET per concept, not one handler (memql#4528). The real
  // SubscriptionManager fans one concept's feed out to every subscriber, and
  // two hooks legitimately watch v1:planner:plan now -- useGoalWorld follows
  // the open goal, useGoals keeps the picker live. A single-handler map made
  // whichever mounted second silently replace the first, which is a fake
  // limitation that would have read as a product bug.
  const handlers = new Map<string, Set<(event: Event) => void>>();
  const empty = new Result({ bundle: { nodes: [] } });

  const planRow = world.plan === null ? null : asRow({ ...world.plan, requestedBy: OWNER_ID });
  const byId = new Map<string, Row>();
  const remember = (row: Row): Row => {
    const id = String(row["id"] ?? "");
    if (id !== "") byId.set(id, row);
    return row;
  };
  if (planRow !== null) remember(planRow);
  const taskRows = world.tasks.map((task) => remember(asRow({ ...task })));
  const agentRows = world.agents.map((agent) => remember(asRow({ ...agent })));
  const artifactRows = world.artifacts.map((artifact) => remember(asRow({ ...artifact })));
  const bundleRow = world.bundle === null ? null : remember(asRow({ ...world.bundle }));
  const constructRows = world.constructs.map((construct) => remember(asRow({ ...construct })));
  const edgeRows = world.edges.map((edge) => remember(asRow({ ...edge })));

  function bundleOf(rows: readonly Row[]): Result {
    return new Result({
      bundle: { nodes: rows.map((row) => ({ id: String(row["id"] ?? ""), payload: row })) },
    });
  }

  const query = asQueryClient({
    listConcepts: vi.fn(async () => CONCEPTS),
    getMyAccess: vi.fn(async () => ACCESS),
    executeNamed: vi.fn(async (name: string, call: string) => {
      calls.push(call);
      if (options.failWrite === name) throw new Error(`refused: ${name}`);
      switch (name) {
        case "conceptRow": {
          const id = /id==(\S+)/.exec(call)?.[1] ?? "";
          const row = byId.get(id);
          return row === undefined ? empty : new Result({ bundle: { nodes: [{ id, payload: row }] } });
        }
        case "plansForUser":
          return bundleOf(options.goals ?? (planRow === null ? [] : [planRow]));
        case "planById":
          return planRow === null ? empty : bundleOf([planRow]);
        case "tasksForPlan":
          return bundleOf(taskRows);
        case "agentsForPlan":
          return bundleOf(agentRows);
        case "artifactsForPlan":
          return bundleOf(artifactRows);
        case "authoringBundleForPlan":
          return bundleRow === null ? empty : bundleOf([bundleRow]);
        case "authoringConstructsForBundle":
          return bundleOf(constructRows);
        case "dependencyEdgesForBundle":
          return bundleOf(edgeRows);
        case "agentById": {
          const id = /agentId: "([^"]*)"/.exec(call)?.[1] ?? "";
          const row = byId.get(id);
          return row === undefined ? empty : bundleOf([row]);
        }
        default:
          return empty;
      }
    }),
  });

  const subscriptions = {
    subscribeGraph: (fn: (event: Event) => void, opts?: { concept?: string }) => {
      const concept = opts?.concept ?? "";
      const set = handlers.get(concept) ?? new Set<(event: Event) => void>();
      set.add(fn);
      handlers.set(concept, set);
      return () => {
        set.delete(fn);
        if (set.size === 0) handlers.delete(concept);
      };
    },
  };

  return {
    query,
    subscriptions,
    calls,
    subscribed: () => handlers.size > 0,
    emit: (concept, event) => {
      act(() => {
        for (const fn of handlers.get(concept) ?? []) fn(event);
      });
    },
  };
}

// LocationProbe renders the current address as text.
//
// The pages under test put real state in the URL -- which goal, which node,
// which moment -- and a test that cannot read the address back can only
// assert what the page DREW, which is the weaker half of the claim. Mounted
// inside the router and outside the app, so it observes without taking part.
function LocationProbe(): React.ReactElement {
  const location = useLocation();
  return <span data-testid="location">{`${location.pathname}${location.search}`}</span>;
}

export function renderNexus(h: NexusHarness, path: string) {
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
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("the Nexus tests must make no identity calls");
        }}
        storage={null}
        navigate={() => {}}
        redirectUri="https://portal.example.com/auth/callback"
      >
        <ClusterProvider dial={dial}>
          <LocationProbe />
          <AppRoutes />
        </ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

// callsNamed pulls the composed calls for one construct out of the log, so an
// assertion can say "the page asked for exactly this" rather than matching a
// substring that another call happens to contain.
export function callsNamed(calls: readonly string[], construct: string): string[] {
  return calls.filter((call) => call.includes(`${construct}(`));
}
