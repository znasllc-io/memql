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

/**
 * The shipped rules (dsl/rules/rules.memql), engine names and all -- ALL of
 * them. A literal because the QA harness shares this file and cannot read the
 * DSL; routingVocabulary.test.tsx holds it to the file (shippedRulesDsl.ts),
 * so a rule added there fails the suite until it is added here.
 */
export function shippedRules(): Row[] {
  return [
    ruleRow({ name: "default", locked: true, precedence: 0 }),
    ruleRow({ name: "backgroundLane", when: { tag: "background" }, locked: true, precedence: 40 }),
    ruleRow({ name: "fastLane", when: { level: "fast" }, policy: "fastLocalFirst", locked: true, precedence: 45 }),
    ruleRow({ name: "backgroundEscalation", when: { tag: "backgroundEscalation" }, level: "strong", locked: true, precedence: 50 }),
    ruleRow({ name: "operatorReasoning", when: { prompt: "agentReply", role: "operator" }, level: "reasoning", locked: true, precedence: 60 }),
    ruleRow({ name: "reasoningParks", when: { level: "reasoning" }, policy: "federationStrongest", onUnavailable: "park", locked: true, precedence: 100 }),
    ruleRow({ name: "embeddingsBound", when: { level: "embeddings" }, policy: "embeddingsBinding", onUnavailable: "park", locked: true, precedence: 110 }),
    ruleRow({ name: "compilerLocalOnly", when: { prompt: "compileRule" }, policy: "localOnly", onUnavailable: "park", locked: true, precedence: 120 }),
    ruleRow({ name: "policyCompilerLocalOnly", when: { prompt: "composeRoutingPolicy" }, policy: "localOnly", onUnavailable: "park", locked: true, precedence: 121 }),
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
  // THE ENGINE'S ONE REVISION, ENFORCED AS THE ENGINE ENFORCES IT
  // (component/memql/policy_customization.go, routing_configuration_rules.go):
  // a route save or reset, a rule validate, save or removal naming any other
  // revision is refused, in the engine's own words. A double that accepted a
  // stale revision would pass a surface that never re-reads after a write in
  // the other tab.
  const stale = (args: Record<string, unknown>, words: string) => {
    if (args.expectedRevision !== state.revision) throw new Error(words.replace("%d", String(args.expectedRevision)));
  };
  const q = connection.query as unknown as Record<string, unknown>;
  Object.assign(q, {
    routerListPolicies: vi.fn(async () => builtinReply("routerListPolicies", withRevision(state.routes))),
    routingPolicySave: vi.fn(async (args: Record<string, unknown>) => {
      if (state.saveError) throw state.saveError;
      stale(args, "routing policies changed since revision %d; refresh and review before saving");
      state.revision += 1;
      const name = args.name as string;
      const primary = args.primary as string;
      const fallbacks = (args.fallbacks as string[]) ?? [];
      const held = state.routes.find((r) => r.name === name);
      const next = held
        ? { ...held, description: args.description as string, primary, fallbacks, chain: [primary, ...fallbacks], customized: held.shipped === true }
        : routeRow(name, primary, fallbacks, { shipped: false, description: args.description as string, defaultChain: [] });
      state.routes = held ? state.routes.map((r) => (r.name === name ? next : r)) : [...state.routes, next];
      return builtinReply("routingPolicySave", [{ id: "saved", name, revision: state.revision, status: "saved" }]);
    }),
    routingPolicyReset: vi.fn(async (args: Record<string, unknown>) => {
      stale(args, "routing policies changed since revision %d; refresh and review before saving");
      state.revision += 1;
      state.routes = state.routes.map((r) => {
        if (r.name !== args.name) return r;
        const shipped = (r.defaultChain as string[] | undefined) ?? [];
        return { ...r, primary: shipped[0] ?? "", fallbacks: shipped.slice(1), chain: shipped, customized: false };
      });
      return builtinReply("routingPolicyReset", [{ id: "reset", name: args.name as string, revision: state.revision, status: "reset" }]);
    }),
    routingRules: vi.fn(async () => builtinReply("routingRules", withRevision(state.rules))),
    routingRuleValidate: vi.fn(async (args: Record<string, unknown>) => {
      // The engine checks the revision only when the draft names one.
      if (args.expectedRevision !== undefined && args.expectedRevision !== null) stale(args, "routing changed since this draft was opened; cancel and refresh before editing");
      return builtinReply("routingRuleValidate", [{ id: "validation", validationOnly: true, revision: state.revision, considered: 0, changed: 0 }]);
    }),
    routingRuleSave: vi.fn(async (args: Record<string, unknown>) => {
      if (state.ruleSaveError) throw state.ruleSaveError;
      stale(args, "routing configuration changed; refresh and validate again");
      state.revision += 1;
      const held = state.rules.find((r) => r.name === args.name);
      const next = ruleRow({ ...(held ?? {}), name: args.name as string, when: (args.conditions as Row) ?? {}, policy: args.policy as string, precedence: args.precedence as number, level: (args.level as string) ?? "", onUnavailable: (args.onUnavailable as string) ?? "degrade", locked: false });
      state.rules = [...state.rules.filter((r) => r.name !== args.name), next];
      return builtinReply("routingRuleSave", [{ ...next, revision: state.revision }]);
    }),
    routingRuleRemove: vi.fn(async (args: Record<string, unknown>) => {
      stale(args, "routing configuration changed; refresh before removing");
      state.revision += 1;
      state.rules = state.rules.filter((r) => r.name !== args.name);
      return builtinReply("routingRuleRemove", [{ id: "removed", name: args.name as string, status: "removed" }]);
    }),
    routingRuleDescribe: vi.fn(async () => builtinReply("routingRuleDescribe", [ruleRow({ name: "planningIsLocal", when: { tag: "planning" }, policy: "localOnly", precedence: 20, problem: "" })])),
    routerDecisionsRecent: vi.fn(async () => builtinReply("routerDecisionsRecent", state.decisions)),
  });
  const query = new Proxy(q, {
    get: (target, prop) => (typeof prop === "string" && state.missing.has(prop) ? undefined : Reflect.get(target, prop)),
  });
  return { connection: { ...connection, query: query as unknown as typeof connection.query }, state, calls: q as unknown as RoutingCalls };
}

