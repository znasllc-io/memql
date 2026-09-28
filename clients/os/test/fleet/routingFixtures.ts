import { vi, type Mock } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { builtinReply, fakeConnection, machineRow } from "./harness";

// The routing surfaces' connection double -- shared by the suite
// (routingHarness.tsx) and the browser QA harness (qa/main.tsx), so a
// screenshot cannot disagree with what the tests assert. No testing-library
// here: the QA harness runs this in a browser.
//
// The double: the Fleet harness's reads (machines,
// the machine-choice row, inferenceStatus) plus the routing builtins, each
// answering in the builtin wire shape the engine sends (builtinReply).
//
// The routes and rules live in ONE revisioned document on the engine
// (router_policy_revision), so this double keeps one `revision` and bumps it
// on every write -- which is what lets a suite assert that a reorder's second
// save names the revision the first one produced.

export interface RoutingState {
  revision: number;
  routes: Row[];
  rules: Row[];
  decisions: Row[];
  /** Builtins this cluster does not have: absent methods, not failing ones. */
  missing: Set<string>;
  saveError: Error | null;
  ruleSaveError: Error | null;
}

export function routeRow(name: string, primary: string, fallbacks: string[] = [], over: Row = {}): Row {
  return {
    id: `policy:${name}`,
    name,
    description: "",
    primary,
    fallbacks,
    chain: [primary, ...fallbacks],
    defaultChain: [primary, ...fallbacks],
    shipped: true,
    customized: false,
    protected: name === "embeddingsBinding",
    revision: 7,
    ...over,
  };
}

export function ruleRow(over: Row & { name: string }): Row {
  return {
    id: over.name,
    when: {},
    level: "",
    policy: "localFirst",
    precedence: 10,
    onUnavailable: "degrade",
    excludes: [],
    locked: false,
    described: "",
    revision: 7,
    ...over,
  };
}

/** The shipped routes, as `routerListPolicies` answers them. */
export function shippedRoutes(): Row[] {
  return [
    routeRow("localFirst", "fleet:strongest", ["app:*", "federation:cheapest"]),
    routeRow("fastLocalFirst", "fleet:fastest", ["app:*", "federation:cheapest"]),
    routeRow("localOnly", "fleet:strongest"),
    routeRow("federationStrongest", "fleet:strongest", ["app:*", "federation:strongest"]),
    routeRow("embeddingsBinding", "embedder:active"),
  ];
}

/** The shipped rules (dsl/rules/rules.memql), engine names and all. */
export function shippedRules(): Row[] {
  return [
    ruleRow({ name: "default", locked: true, precedence: 0 }),
    ruleRow({ name: "backgroundLane", when: { tag: "background" }, locked: true, precedence: 40 }),
    ruleRow({ name: "fastLane", when: { level: "fast" }, policy: "fastLocalFirst", locked: true, precedence: 45 }),
    ruleRow({ name: "reasoningParks", when: { level: "reasoning" }, policy: "federationStrongest", locked: true, precedence: 100 }),
    ruleRow({ name: "embeddingsBound", when: { level: "embeddings" }, policy: "embeddingsBinding", locked: true, precedence: 110 }),
  ];
}

/** A studio machine: Claude Code signed in, Codex absent, one chat model and one embedder. */
export function studioMachine(over: Row = {}): Row {
  return machineRow({
    id: "v1:worker:registration:studio",
    displayName: "studio-mac",
    labels: {
      "model:qwen3.8:27b": "ctx=32768,structured=1,tools=1,params=27000000000",
      "model:nomic-embed": "ctx=8192,embeddings=1",
    },
    apps: [{ id: "claude-code", signedIn: true, allowed: true, version: "1.0" }],
    ...over,
  });
}

/** The routing builtins this double answers, as the spies a suite asserts on. */
export interface RoutingCalls {
  routerListPolicies: Mock;
  routingPolicySave: Mock;
  routingPolicyReset: Mock;
  routingRules: Mock;
  routingRuleValidate: Mock;
  routingRuleSave: Mock;
  routingRuleRemove: Mock;
  routingRuleDescribe: Mock;
  routerDecisionsRecent: Mock;
}

export function routingConnection(seed: { machines?: Row[]; federationConfigured?: boolean; routes?: Row[]; rules?: Row[]; decisions?: Row[] } = {}) {
  const state: RoutingState = {
    revision: 7,
    routes: seed.routes ?? shippedRoutes(),
    rules: seed.rules ?? shippedRules(),
    decisions: seed.decisions ?? [],
    missing: new Set(),
    saveError: null,
    ruleSaveError: null,
  };
  const connection = fakeConnection({
    myWorkersWithStatus: seed.machines ?? [studioMachine()],
    myRoutingPolicies: [],
    inferenceStatus: [{ id: "inferenceStatus", federationConfigured: seed.federationConfigured ?? false, eligible: true }],
  });
  const withRevision = (rows: Row[]) => rows.map((r) => ({ ...r, revision: state.revision }));
  const q = connection.query as unknown as Record<string, unknown>;
  Object.assign(q, {
    routerListPolicies: vi.fn(async () => builtinReply("routerListPolicies", withRevision(state.routes))),
    routingPolicySave: vi.fn(async (args: Record<string, unknown>) => {
      if (state.saveError) throw state.saveError;
      state.revision += 1;
      return builtinReply("routingPolicySave", [{ id: "saved", name: args.name as string }]);
    }),
    routingPolicyReset: vi.fn(async (args: Record<string, unknown>) => {
      state.revision += 1;
      return builtinReply("routingPolicyReset", [{ id: "reset", name: args.name as string }]);
    }),
    routingRules: vi.fn(async () => builtinReply("routingRules", withRevision(state.rules))),
    routingRuleValidate: vi.fn(async () => builtinReply("routingRuleValidate", [{ id: "validation", validationOnly: true, revision: state.revision, considered: 0, changed: 0 }])),
    routingRuleSave: vi.fn(async (args: Record<string, unknown>) => {
      if (state.ruleSaveError) throw state.ruleSaveError;
      if (args.expectedRevision !== state.revision) throw new Error("routing configuration changed; refresh and validate again");
      state.revision += 1;
      const held = state.rules.find((r) => r.name === args.name);
      const next = ruleRow({ ...(held ?? {}), name: args.name as string, when: (args.conditions as Row) ?? {}, policy: args.policy as string, precedence: args.precedence as number, level: (args.level as string) ?? "", onUnavailable: (args.onUnavailable as string) ?? "degrade", locked: false });
      state.rules = [...state.rules.filter((r) => r.name !== args.name), next];
      return builtinReply("routingRuleSave", [{ ...next, revision: state.revision }]);
    }),
    routingRuleRemove: vi.fn(async (args: Record<string, unknown>) => {
      state.revision += 1;
      state.rules = state.rules.filter((r) => r.name !== args.name);
      return builtinReply("routingRuleRemove", [{ id: "removed", name: args.name as string, status: "removed" }]);
    }),
    routingRuleDescribe: vi.fn(async () => builtinReply("routingRuleDescribe", [ruleRow({ name: "planningIsLocal", when: { level: "reasoning" }, policy: "localOnly", precedence: 20, problem: "" })])),
    routerDecisionsRecent: vi.fn(async () => builtinReply("routerDecisionsRecent", state.decisions)),
  });
  const query = new Proxy(q, {
    get: (target, prop) => (typeof prop === "string" && state.missing.has(prop) ? undefined : Reflect.get(target, prop)),
  });
  return { connection: { ...connection, query: query as unknown as typeof connection.query }, state, calls: q as unknown as RoutingCalls };
}

