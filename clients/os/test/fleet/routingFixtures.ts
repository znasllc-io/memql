import { vi, type Mock } from "vitest";
import { QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

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
  /** Routes as `routesForActor` answers them: Auto, Local, Apps and the person's own. */
  modes: Row[];
  balance: Row;
  /** Null: the person has no budget row yet (the builtin still answers). */
  budget: Row;
  /** What each machine can take now, as `fleetCapacity` answers it. */
  capacity: Row[];
  /** The bound embedder's model id, "" for none. */
  embedder: string;
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
    ruleRow({ name: "documentResearchHarness", when: { prompt: "libraryRevisionResearch" }, policy: "localOnly", onUnavailable: "park", locked: true, precedence: 150 }),
    ruleRow({ name: "documentAppResearch", when: { prompt: "libraryRevisionAppResearch" }, policy: "researchApps", onUnavailable: "park", locked: true, precedence: 151 }),
    ruleRow({ name: "documentResearchIntent", when: { prompt: "libraryRevisionIntent" }, policy: "localOnly", onUnavailable: "park", locked: true, precedence: 152 }),
    ruleRow({ name: "documentResearchEvidence", when: { prompt: "libraryRevisionEvidence" }, policy: "localOnly", onUnavailable: "park", locked: true, precedence: 153 }),
    ruleRow({ name: "documentImageGeneration", when: { prompt: "libraryRevisionImageGeneration" }, policy: "localOnly", onUnavailable: "park", locked: true, precedence: 154 }),
    ruleRow({ name: "savedImageGeneration", when: { prompt: "libraryImageGeneration" }, policy: "localOnly", onUnavailable: "park", locked: true, precedence: 156 }),
    ruleRow({ name: "documentImagePlanning", when: { prompt: "libraryRevisionImagePlan" }, policy: "localOnly", onUnavailable: "park", locked: true, precedence: 155 }),
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

/** One route as `routesForActor` answers it. */
export function routeModeRow(over: Row & { id: string }): Row {
  const kind = (over.kind as string | undefined) ?? (["auto", "local", "apps"].includes(over.id) ? over.id : "custom");
  return {
    name: kind === "custom" ? over.id : kind.charAt(0).toUpperCase() + kind.slice(1),
    kind,
    scope: kind === "custom" ? "personal" : "cluster",
    description: "",
    ready: true,
    reason: "",
    customized: false,
    jobs: [],
    ...over,
  };
}

/** A route's four jobs on the wire; embeddings is always the bound embedder. */
export function routeJobs(fast: string[], strong: string[], reasoning: string[], servesNow: Partial<Record<string, string>> = {}): Row[] {
  const job = (id: string, sources: string[]) => ({ job: id, sources, editable: true, servesNow: servesNow[id] ?? "", ready: (servesNow[id] ?? "") !== "" });
  return [
    job("fast", fast),
    job("strong", strong),
    job("reasoning", reasoning),
    { job: "embeddings", sources: ["embedder:active"], editable: false, servesNow: servesNow.embeddings ?? "", ready: true },
  ];
}

/** Auto, Local and Apps as shipped, ready on the studio machine. */
export function shippedRouteModes(): Row[] {
  return [
    routeModeRow({ id: "auto", description: "Picks the best balance of speed, cost and quality for every call" }),
    routeModeRow({ id: "local", description: "Local models for everything", jobs: routeJobs(["fleet:fastest"], ["fleet:strongest"], ["fleet:strongest"]) }),
    routeModeRow({ id: "apps", description: "Claude Code or Codex, with your embedding model", jobs: routeJobs(["app:*"], ["app:*"], ["app:*"]) }),
  ];
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
  /** What the generated methods send: `(name, MemQL text, options)`. */
  executeNamed: Mock;
  routesForActor: Mock;
  routeSave: Mock;
  routeDelete: Mock;
  routeReset: Mock;
  autoBalance: Mock;
  autoBalanceSave: Mock;
  autoBalanceReset: Mock;
  myBudget: Mock;
  fleetCapacity: Mock;
  activeEmbedderBinding: Mock;
}

export function routingConnection(seed: { machines?: Row[]; federationConfigured?: boolean; routes?: Row[]; rules?: Row[]; decisions?: Row[]; modes?: Row[]; balance?: Row; budget?: Row; capacity?: Row[]; embedder?: string } = {}) {
  const state: RoutingState = {
    revision: 7,
    routes: seed.routes ?? shippedRoutes(),
    rules: seed.rules ?? shippedRules(),
    decisions: seed.decisions ?? [],
    modes: seed.modes ?? shippedRouteModes(),
    balance: seed.balance ?? { speed: 50, cost: 50, quality: 50, recommended: { speed: 50, cost: 50, quality: 50 }, customized: false },
    budget: seed.budget ?? { monthlyLimitUSD: null, spentThisMonthUSD: 0, resetsAt: "2026-11-01T00:00:00Z" },
    capacity: seed.capacity ?? [],
    embedder: seed.embedder ?? "nomic-embed",
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
    // THE GENERATED METHOD ITSELF, answered at `executeNamed` below. Every
    // method on QueryClient.prototype calls `this.executeNamed`, so a surface
    // that takes one off its client and calls it bare fails here exactly as
    // it fails in the browser ("Cannot read properties of undefined (reading
    // 'executeNamed')"). A closure that never read `this` let the History tab
    // ship that crash through a green suite.
    routerDecisionsRecent: vi.fn(QueryClient.prototype.routerDecisionsRecent),
    executeNamed: vi.fn(async (name: string) => {
      if (name === "routerDecisionsRecent") return builtinReply("routerDecisionsRecent", state.decisions);
      throw new Error(`the routing double does not answer ${name} at executeNamed`);
    }),
    // ROUTES (design record 2026-10-18-routes-design.md): one object per read,
    // sharing the routing document's revision with chains and rules.
    routesForActor: vi.fn(async () => builtinReply("routesForActor", [{ id: "routesForActor", routes: state.modes, revision: state.revision }])),
    routeSave: vi.fn(async (args: Record<string, unknown>) => {
      if (state.saveError) throw state.saveError;
      const id = typeof args.id === "string" ? args.id : "";
      const jobs = routeJobs(args.fast as string[], args.strong as string[], args.reasoning as string[]);
      if (id === "local" || id === "apps") {
        if (args.revision !== state.revision) throw new Error(`routing changed since revision ${String(args.revision)}; refresh and review before saving`);
        state.revision += 1;
        state.modes = state.modes.map((m) => (m.id === id ? { ...m, jobs, customized: true } : m));
      } else if (id !== "") {
        state.modes = state.modes.map((m) => (m.id === id ? { ...m, jobs, name: (args.name as string | undefined) ?? m.name } : m));
      } else {
        const created = routeModeRow({ id: `r${state.modes.length}`, name: args.name as string, jobs });
        state.modes = [...state.modes, created];
        return builtinReply("routeSave", [created]);
      }
      return builtinReply("routeSave", [state.modes.find((m) => m.id === id)!]);
    }),
    routeDelete: vi.fn(async (args: Record<string, unknown>) => {
      state.modes = state.modes.filter((m) => m.id !== args.id);
      return builtinReply("routeDelete", []);
    }),
    routeReset: vi.fn(async (args: Record<string, unknown>) => {
      if (args.revision !== state.revision) throw new Error(`routing changed since revision ${String(args.revision)}; refresh and review before saving`);
      state.revision += 1;
      const shipped = shippedRouteModes().find((m) => m.id === args.id)!;
      state.modes = state.modes.map((m) => (m.id === args.id ? { ...m, jobs: shipped.jobs, customized: false } : m));
      return builtinReply("routeReset", [state.modes.find((m) => m.id === args.id)!]);
    }),
    autoBalance: vi.fn(async () => builtinReply("autoBalance", [{ id: "autoBalance", ...state.balance, revision: state.revision }])),
    autoBalanceSave: vi.fn(async (args: Record<string, unknown>) => {
      if (args.revision !== state.revision) throw new Error(`routing changed since revision ${String(args.revision)}; refresh and review before saving`);
      state.revision += 1;
      const recommended = state.balance.recommended as Row;
      const next = { speed: args.speed, cost: args.cost, quality: args.quality };
      state.balance = { ...state.balance, ...next, customized: next.speed !== recommended.speed || next.cost !== recommended.cost || next.quality !== recommended.quality };
      return builtinReply("autoBalanceSave", [{ id: "autoBalance", ...state.balance, revision: state.revision }]);
    }),
    autoBalanceReset: vi.fn(async (args: Record<string, unknown>) => {
      if (args.revision !== state.revision) throw new Error(`routing changed since revision ${String(args.revision)}; refresh and review before saving`);
      state.revision += 1;
      state.balance = { ...state.balance, ...(state.balance.recommended as Row), customized: false };
      return builtinReply("autoBalanceReset", [{ id: "autoBalance", ...state.balance, revision: state.revision }]);
    }),
    myBudget: vi.fn(async () => builtinReply("myBudget", [{ id: "myBudget", ...state.budget }])),
    fleetCapacity: vi.fn(async () => builtinReply("fleetCapacity", [{ id: "fleetCapacity", machines: state.capacity }])),
    activeEmbedderBinding: vi.fn(async () =>
      builtinReply("activeEmbedderBinding", state.embedder === "" ? [] : [{ id: "primary", providerRef: `fleet:${state.embedder}`, activatedAt: "2026-10-18T10:00:00Z" }]),
    ),
  });
  const query = new Proxy(q, {
    get: (target, prop) => (typeof prop === "string" && state.missing.has(prop) ? undefined : Reflect.get(target, prop)),
  });
  return { connection: { ...connection, query: query as unknown as typeof connection.query }, state, calls: q as unknown as RoutingCalls };
}
