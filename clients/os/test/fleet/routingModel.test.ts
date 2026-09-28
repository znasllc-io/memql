import { describe, expect, it } from "vitest";

import {
  RETIRED_ROUTING_WORDS,
  humanize,
  retiredWordsIn,
  routeIdFrom,
  routeTitle,
  sourceLabel,
  whenWords,
} from "../../src/apps/fleet/routing/vocabulary";
import {
  factsFrom,
  placementProblem,
  readSource,
  routeStatus,
  servingIndex,
  trayGroups,
  type MachineFacts,
  type RoutingFacts,
} from "../../src/apps/fleet/routing/sources";
import { equip, moveSlot, removeSlot, routeBar } from "../../src/apps/fleet/routing/routeDraft";
import { reorderPlan } from "../../src/apps/fleet/routing/ruleOrder";

// The routing model, as arithmetic (Fleet > Routing, design brief sections 1-5).
//
// Everything the composer DRAWS is a function of these: which slot serves, why a
// source cannot, whether a placement is refused, which acts the bar offers and
// what a reorder writes. A circuit lit by a render test alone would be lit by
// whatever the fixture happened to make true; here each claim is asserted over
// the facts that make it true, and over the facts that make it false.

function machine(over: Partial<MachineFacts> & { id: string }): MachineFacts {
  return { name: over.id, online: true, models: [], apps: [], ...over };
}

const chat = (modelId: string, params = 0) => ({
  modelId,
  params,
  quant: "",
  contextWindow: 32_768,
  structuredOutput: true,
  tools: true,
  embeddings: false,
  maxConcurrent: 0,
});
const embedder = (modelId: string) => ({ ...chat(modelId), embeddings: true, structuredOutput: false, tools: false });
const app = (id: string, runnable = true, why = "") => ({
  id,
  label: id === "claude-code" ? "Claude Code" : id === "codex" ? "Codex" : id,
  version: "1",
  signedIn: runnable,
  allowed: true,
  subscription: "present",
  runnable,
  why,
});

function facts(over: Partial<RoutingFacts> = {}): RoutingFacts {
  return {
    machinesRead: true,
    machines: [],
    vendor: "idle",
    routes: [],
    preference: [],
    ...over,
  };
}

describe("the routing vocabulary", () => {
  it("names shipped routes in words, never by their engine ids", () => {
    expect(routeTitle("localFirst")).toBe("Local first");
    expect(routeTitle("fastLocalFirst")).toBe("Fast local first");
    expect(routeTitle("localOnly")).toBe("Local only");
    // The engine's own id carries a retired word; the title must not.
    expect(retiredWordsIn(routeTitle("federationStrongest"))).toEqual([]);
    expect(routeTitle("embeddingsBinding")).toBe("Embeddings");
    expect(routeTitle("nightShift")).toBe("Night shift");
  });

  it("round-trips a name a person types into an id the engine accepts", () => {
    expect(routeIdFrom("Night shift")).toBe("nightShift");
    expect(routeIdFrom("  planning  stays local ")).toBe("planningStaysLocal");
    expect(routeIdFrom("GPU box 2")).toBe("gpuBox2");
    expect(routeIdFrom("--")).toBe("");
    expect(humanize(routeIdFrom("Night shift"))).toBe("Night shift");
    for (const id of ["nightShift", "planningStaysLocal", "gpuBox2"]) expect(id).toMatch(/^[a-z][A-Za-z0-9]*$/);
  });

  it("reads sources as sources", () => {
    expect(sourceLabel("fleet:strongest")).toBe("Strongest local model");
    expect(sourceLabel("fleet:fastest")).toBe("Fastest local model");
    expect(sourceLabel("app:*")).toBe("Any signed-in app");
    expect(sourceLabel("app:claude-code")).toBe("Claude Code");
    expect(sourceLabel("app:codex:gpt-5")).toBe("Codex · gpt-5");
    expect(sourceLabel("federation:cheapest")).toBe("Cheapest vendor");
    expect(sourceLabel("federation:strongest")).toBe("Strongest vendor");
    expect(sourceLabel("fleet:qwen3.8:27b")).toBe("qwen3.8:27b");
    expect(sourceLabel("policy:localFirst")).toBe("Local first");
  });

  it("writes a rule's condition as the work it matches", () => {
    expect(whenWords({ when: { level: "fast" }, locked: true })).toBe("Fast work");
    expect(whenWords({ when: {}, locked: true })).toBe("Everything else");
    expect(whenWords({ when: {}, locked: false })).toBe("Every call");
    expect(whenWords({ when: { modality: "structured" }, locked: false })).toBe("Structured output");
    expect(whenWords({ when: { prompt: "agentReply", role: "operator" }, locked: true })).toBe("Agent reply prompt · operator acting");
    expect(whenWords({ when: { tag: "" }, locked: false })).toBe("No tag");
    // A shipped rule's engine name carries a retired word; its words must not.
    expect(retiredWordsIn(whenWords({ when: { tag: "background" }, locked: true }))).toEqual([]);
  });

  it("catches every retired word, in any case", () => {
    expect(RETIRED_ROUTING_WORDS.length).toBeGreaterThanOrEqual(5);
    expect(retiredWordsIn("Save policy")).toEqual(["policy"]);
    expect(retiredWordsIn("Two doors")).toEqual(["door"]);
    expect(retiredWordsIn("Add a Task rule")).toEqual(["task rule"]);
    expect(retiredWordsIn("Federation strongest")).toEqual(["federation"]);
    expect(retiredWordsIn("fastLane")).toEqual(["lane"]);
    expect(retiredWordsIn("Local first. Serves with Claude Code.")).toEqual([]);
    // "planes" and "airplane" are not lanes.
    expect(retiredWordsIn("planes of glass")).toEqual([]);
  });
});

describe("a source's readiness, from real facts", () => {
  const studio = machine({ id: "studio", name: "studio-mac", models: [chat("qwen3.5:4b", 4e9), chat("qwen3.8:27b", 27e9), embedder("nomic-embed")], apps: [app("claude-code")] });

  it("is ready for an app signed in on an online machine, and names where", () => {
    const reading = readSource("app:claude-code", facts({ machines: [studio] }));
    expect(reading.readiness).toBe("ready");
    expect(reading.fact).toBe("on studio-mac");
    expect(reading.reason).toBe("");
  });

  it("says why an app cannot serve, and keeps the reasons apart", () => {
    expect(readSource("app:codex", facts({ machines: [studio] })).reason).toBe("not installed on any machine");
    const asleep = machine({ id: "a", name: "laptop", online: false, apps: [app("codex")] });
    expect(readSource("app:codex", facts({ machines: [asleep] })).reason).toBe("laptop is offline");
    const signedOut = machine({ id: "b", name: "laptop", apps: [app("codex", false, "Not signed in")] });
    expect(readSource("app:codex", facts({ machines: [signedOut] })).reason).toBe("not signed in on laptop");
    for (const r of [asleep, signedOut]) expect(readSource("app:codex", facts({ machines: [r] })).readiness).toBe("idle");
  });

  it("names the model the strongest selector would take, and never an embedder", () => {
    const reading = readSource("fleet:strongest", facts({ machines: [studio] }));
    expect(reading.readiness).toBe("ready");
    expect(reading.fact).toBe("qwen3.8:27b");
    const onlyEmbeds = machine({ id: "e", models: [embedder("nomic-embed")] });
    expect(readSource("fleet:strongest", facts({ machines: [onlyEmbeds] })).readiness).toBe("idle");
  });

  it("follows the owner's model preference, as the router does", () => {
    const reading = readSource("fleet:strongest", facts({ machines: [studio], preference: ["qwen3.5:4b"] }));
    expect(reading.fact).toBe("qwen3.5:4b");
  });

  it("is ready for a named local model only while a machine offering it is online", () => {
    expect(readSource("fleet:qwen3.8:27b", facts({ machines: [studio] })).readiness).toBe("ready");
    const off = { ...studio, online: false };
    const reading = readSource("fleet:qwen3.8:27b", facts({ machines: [off] }));
    expect(reading.readiness).toBe("idle");
    expect(reading.reason).toBe("studio-mac is offline");
    expect(readSource("fleet:llama", facts({ machines: [studio] })).reason).toBe("not on any machine");
  });

  it("reads a vendor from whether one is set up", () => {
    expect(readSource("federation:cheapest", facts({ vendor: "ready" })).readiness).toBe("ready");
    const none = readSource("federation:cheapest", facts({ vendor: "idle" }));
    expect(none.readiness).toBe("idle");
    expect(none.reason).toBe("no vendor set up");
    expect(readSource("federation:cheapest", facts({ vendor: "unknown" })).readiness).toBe("unknown");
  });

  it("is unknown, not idle, before the machines have been read", () => {
    const reading = readSource("app:claude-code", facts({ machinesRead: false, machines: [] }));
    expect(reading.readiness).toBe("unknown");
  });

  it("reads another route through its own chain, and survives a cycle", () => {
    const routes = [
      { name: "a", entries: ["policy:b"] },
      { name: "b", entries: ["policy:a", "app:claude-code"] },
    ];
    const reading = readSource("policy:a", facts({ machines: [studio], routes }));
    expect(reading.readiness).toBe("ready");
    expect(reading.fact).toBe("via Claude Code");
  });
});

describe("the circuit", () => {
  const studio = machine({ id: "studio", name: "studio-mac", models: [chat("qwen3.8:27b", 27e9)], apps: [app("claude-code")] });

  it("lights up to the FIRST ready slot, passing over the ones that cannot serve", () => {
    const f = facts({ machines: [studio], vendor: "idle" });
    expect(servingIndex(["app:codex", "app:claude-code", "fleet:strongest"], f)).toEqual({ index: 1, blockedAt: -1 });
    expect(servingIndex(["federation:cheapest", "app:codex"], f)).toEqual({ index: -1, blockedAt: -1 });
  });

  it("stops at a slot whose readiness is not known, rather than guessing past it", () => {
    const f = facts({ machines: [studio], vendor: "unknown" });
    expect(servingIndex(["federation:cheapest", "app:claude-code"], f)).toEqual({ index: -1, blockedAt: 0 });
  });

  it("says what serves in few words, naming what actually answers", () => {
    const f = facts({ machines: [studio] });
    expect(routeStatus(["app:codex", "app:claude-code"], f).word).toBe("Serves with Claude Code");
    expect(routeStatus(["fleet:strongest"], f).word).toBe("Serves with qwen3.8:27b");
    expect(routeStatus(["app:*"], f).word).toBe("Serves with Claude Code");
    expect(routeStatus(["app:codex"], f).word).toBe("Nothing ready");
    expect(routeStatus([], f).word).toBe("No sources");
    expect(routeStatus(["app:claude-code"], facts({ machinesRead: false })).word).toBe("");
  });
});

describe("placing a source", () => {
  const studio = machine({ id: "studio", models: [chat("qwen3.8:27b"), embedder("nomic-embed")] });
  const f = facts({ machines: [studio], routes: [{ name: "localFirst", entries: ["fleet:strongest"] }, { name: "wraps", entries: ["policy:mine"] }] });

  it("refuses an embeddings-only source in a chat route, with a reason", () => {
    expect(placementProblem("fleet:nomic-embed", ["fleet:strongest"], "mine", f)).toBe("nomic-embed only makes embeddings.");
    expect(placementProblem("embedder:active", ["fleet:strongest"], "mine", f)).toMatch(/only makes embeddings/);
    expect(placementProblem("fleet:qwen3.8:27b", ["fleet:strongest"], "mine", f)).toBe("");
  });

  it("refuses a duplicate, a route inside itself, and a loop through another route", () => {
    expect(placementProblem("fleet:strongest", ["fleet:strongest"], "mine", f)).toBe("Already in this route.");
    // Replacing a slot with what it already holds is not a duplicate.
    expect(placementProblem("fleet:strongest", ["fleet:strongest"], "mine", f, 0)).toBe("");
    expect(placementProblem("policy:mine", [], "mine", f)).toBe("A route cannot include itself.");
    expect(placementProblem("policy:wraps", [], "mine", f)).toBe("Wraps already includes this route.");
    expect(placementProblem("policy:localFirst", [], "mine", f)).toBe("");
  });

  it("refuses a seventeenth fallback, which the engine would refuse at save", () => {
    const full = Array.from({ length: 17 }, (_, i) => `fleet:m${i}`);
    expect(placementProblem("app:*", full, "mine", f)).toBe("A route holds at most 17 sources.");
  });
});

describe("editing a chain", () => {
  it("appends, replaces, moves and removes without touching the input", () => {
    const start = ["a", "b", "c"];
    expect(equip(start, "d", 3)).toEqual(["a", "b", "c", "d"]);
    expect(equip(start, "d", 1)).toEqual(["a", "d", "c"]);
    expect(moveSlot(start, 0, 2)).toEqual(["b", "c", "a"]);
    expect(moveSlot(start, 2, 1)).toEqual(["a", "c", "b"]);
    expect(moveSlot(start, 0, -1)).toEqual(start);
    expect(removeSlot(start, 1)).toEqual(["a", "c"]);
    expect(start).toEqual(["a", "b", "c"]);
  });
});

describe("the route's action bar", () => {
  const base = { shipped: true, customized: false, protected: false, dirty: false, valid: true, busy: false, confirmingRestore: false, serving: "serves with Claude Code" };

  it("offers nothing on an untouched shipped route, and says the scope once", () => {
    const bar = routeBar(base);
    expect(bar.state).toBe("Shipped");
    expect(bar.acts).toEqual([]);
    expect(bar.detail).toBe("Applies to every rule that takes this route");
  });

  it("offers Restore shipped only on a changed shipped route", () => {
    expect(routeBar({ ...base, customized: true })).toMatchObject({ state: "Changed from shipped", acts: ["restore"] });
    expect(routeBar({ ...base, shipped: false }).acts).toEqual([]);
    expect(routeBar({ ...base, shipped: false }).state).toBe("Saved");
  });

  it("puts Save last, and only when the draft can be saved", () => {
    expect(routeBar({ ...base, dirty: true })).toMatchObject({ state: "Unsaved", detail: "serves with Claude Code", acts: ["cancel", "save"] });
    expect(routeBar({ ...base, dirty: true, customized: true }).acts).toEqual(["cancel", "restore", "save"]);
    // ABSENT, never disabled.
    expect(routeBar({ ...base, dirty: true, valid: false }).acts).toEqual(["cancel"]);
  });

  it("asks before restoring, in the bar", () => {
    expect(routeBar({ ...base, customized: true, confirmingRestore: true })).toMatchObject({ acts: ["keep", "confirmRestore"] });
  });

  it("offers no act on the protected embeddings route", () => {
    expect(routeBar({ ...base, protected: true, dirty: true }).acts).toEqual([]);
  });

  it("offers no act while a write is in flight", () => {
    expect(routeBar({ ...base, dirty: true, busy: true })).toMatchObject({ tone: "busy", acts: [] });
  });
});

describe("reordering custom rules", () => {
  // Precedence descending: the order the engine evaluates the custom tier in.
  const custom = [
    { name: "a", precedence: 50 },
    { name: "b", precedence: 30 },
    { name: "c", precedence: 10 },
  ];

  it("writes ONE rule when there is room between its new neighbours", () => {
    expect(reorderPlan(custom, 2, 1)).toEqual([{ name: "c", precedence: 40 }]);
    expect(reorderPlan(custom, 0, 1)).toEqual([{ name: "a", precedence: 20 }]);
    expect(reorderPlan(custom, 2, 0)).toEqual([{ name: "c", precedence: 60 }]);
    expect(reorderPlan(custom, 0, 2)).toEqual([{ name: "a", precedence: 0 }]);
  });

  it("renumbers above every current value when there is no room, so no write ever ties", () => {
    const tight = [
      { name: "a", precedence: 11 },
      { name: "b", precedence: 10 },
      { name: "c", precedence: 9 },
    ];
    const plan = reorderPlan(tight, 2, 1);
    const after = new Map(tight.map((r) => [r.name, r.precedence]));
    // Apply the writes one at a time: no intermediate state may hold a tie.
    for (const change of plan) {
      after.set(change.name, change.precedence);
      const values = [...after.values()];
      expect(new Set(values).size).toBe(values.length);
    }
    const order = [...after.entries()].sort((x, y) => y[1] - x[1]).map(([name]) => name);
    expect(order).toEqual(["a", "c", "b"]);
  });

  it("writes nothing for a move that goes nowhere", () => {
    expect(reorderPlan(custom, 1, 1)).toEqual([]);
    expect(reorderPlan(custom, 0, -1)).toEqual([]);
    expect(reorderPlan(custom, 2, 3)).toEqual([]);
  });
});

describe("the tray", () => {
  it("lists every source the cluster knows, grouped, INCLUDING the ones that cannot serve", () => {
    const studio = machine({ id: "studio", name: "studio-mac", models: [chat("qwen3.5:4b")], apps: [app("claude-code")] });
    const groups = trayGroups(facts({ machines: [studio], routes: [{ name: "localFirst", entries: ["fleet:strongest"] }, { name: "mine", entries: [] }] }), "mine");
    expect(groups.map((g) => g.title)).toEqual(["On your machines", "Choices", "Vendors", "Routes"]);
    const machines = groups[0]!.sources.map((s) => `${s.label}:${s.readiness}`);
    // Codex is on no machine and is still listed -- the person has to see why
    // a route through it would not connect.
    expect(machines).toEqual(["Claude Code:ready", "Codex:idle", "qwen3.5:4b:ready"]);
    expect(groups[1]!.sources.map((s) => s.entry)).toEqual(["fleet:strongest", "fleet:fastest", "app:*"]);
    expect(groups[2]!.sources.map((s) => s.entry)).toEqual(["federation:cheapest", "federation:strongest"]);
    // A route never offers itself.
    expect(groups[3]!.sources.map((s) => s.entry)).toEqual(["policy:localFirst"]);
  });

  it("builds its facts from machine rows, leaving revoked machines out", () => {
    const f = factsFrom({
      machines: [
        { id: "m1", name: "", displayName: "Studio", online: true, revoked: false, labels: { "model:qwen3.5:4b": "ctx=32768" }, apps: [] },
        { id: "m2", name: "old", displayName: "", online: true, revoked: true, labels: {}, apps: [] },
      ],
      machinesRead: true,
      inference: { read: true, federationConfigured: false, error: "" },
      routes: [],
      preference: [],
    });
    expect(f.machines.map((m) => m.name)).toEqual(["Studio"]);
    expect(f.machines[0]!.models.map((m) => m.modelId)).toEqual(["qwen3.5:4b"]);
    expect(f.vendor).toBe("idle");
  });
});
