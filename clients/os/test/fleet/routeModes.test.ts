import { describe, expect, it } from "vitest";

import { capacityFrom, capacityReason, gigabytes } from "../../src/apps/fleet/routing/capacity";
import { jobPlacementProblem, jobStatus, sentence } from "../../src/apps/fleet/routing/JobBoard";
import {
  autoBalanceFrom,
  budgetFrom,
  canEditRoute,
  embedderReading,
  jobSourcesOf,
  jobsComplete,
  routeModeBar,
  routeModeFrom,
  routeModesFrom,
  routeNameProblem,
  routeRefusal,
  shippedRouteModes,
  usd,
} from "../../src/apps/fleet/routing/routeModes";
import { readSource, type MachineFacts, type RoutingFacts } from "../../src/apps/fleet/routing/sources";
import { STALE_SENTENCE } from "../../src/apps/fleet/routing/vocabulary";

// Routes as arithmetic (design record 2026-10-18-routes-design.md, "Fleet,
// Routing, Routes"): what the engine's replies read as, which acts a route's
// bar offers in each state, which names a person may give a route, and what a
// machine's capacity says about a source. The pages draw these; here each
// claim is asserted over the facts that make it true and those that make it
// false.

const chat = (modelId: string, params = 0) => ({
  modelId,
  params,
  quant: "",
  contextWindow: 32_768,
  structuredOutput: true,
  tools: true,
  embeddings: false,
  vision: false,
  audioIn: false,
  audioOut: false,
  imageGen: false,
  maxConcurrent: 0,
});

function machine(over: Partial<MachineFacts> & { id: string }): MachineFacts {
  return { name: over.id, online: true, models: [], apps: [], ...over };
}

function facts(over: Partial<RoutingFacts> = {}): RoutingFacts {
  return { machinesRead: true, machines: [], vendor: "idle", routes: [], preference: [], ...over };
}

const summary = (over: Record<string, unknown> & { id: string }) => ({
  name: "",
  kind: "",
  scope: "",
  description: "",
  ready: true,
  reason: "",
  customized: false,
  jobs: [],
  ...over,
});

describe("a routesForActor reply", () => {
  it("reads the one row the builtin answers, routes and revision together, in the page's order", () => {
    const read = routeModesFrom([
      {
        id: "routes",
        routes: [
          summary({ id: "r9", name: "Night shift", kind: "custom", scope: "personal" }),
          summary({ id: "apps", name: "Apps", kind: "apps", scope: "cluster" }),
          summary({ id: "auto", name: "Auto", kind: "auto", scope: "cluster" }),
          summary({ id: "r2", name: "Emergency", kind: "custom", scope: "personal" }),
          summary({ id: "local", name: "Local", kind: "local", scope: "cluster" }),
        ],
        revision: 12,
      },
    ]);
    expect(read.revision).toBe(12);
    expect(read.routes.map((r) => r.id)).toEqual(["auto", "local", "apps", "r2", "r9"]);
  });

  it("reads a reply that lists the routes as rows the same way", () => {
    const read = routeModesFrom([summary({ id: "local", kind: "local", revision: 3 }), summary({ id: "auto", kind: "auto", revision: 3 })]);
    expect(read.routes.map((r) => r.id)).toEqual(["auto", "local"]);
    expect(read.revision).toBe(3);
  });

  it("completes a route's four jobs, and keeps Search and memory the cluster's", () => {
    const local = routeModeFrom(summary({ id: "local", kind: "local", jobs: [{ job: "strong", sources: ["fleet:strongest", "app:*"], editable: true, servesNow: "", ready: false }] }))!;
    expect(local.jobs.map((j) => j.job)).toEqual(["fast", "strong", "reasoning", "embeddings"]);
    expect(jobSourcesOf(local)).toEqual({ fast: [], strong: ["fleet:strongest", "app:*"], reasoning: [] });
    const embeddings = local.jobs.find((j) => j.job === "embeddings")!;
    expect(embeddings.sources).toEqual(["embedder:active"]);
    expect(embeddings.editable).toBe(false);
  });

  it("keeps Auto's jobs as reported -- its picks, read-only, and often none", () => {
    const auto = routeModeFrom(summary({ id: "auto", kind: "auto", jobs: [] }))!;
    expect(auto.jobs).toEqual([]);
    const picked = routeModeFrom(summary({ id: "auto", kind: "auto", jobs: [{ job: "fast", sources: ["app:*"], editable: true, servesNow: "Claude Code", ready: true }] }))!;
    expect(picked.jobs[0]!.editable).toBe(false);
  });

  it("names a route the engine left unnamed, and reads an unknown kind from the id", () => {
    expect(routeModeFrom(summary({ id: "local" }))).toMatchObject({ name: "Local", kind: "local", scope: "cluster" });
    expect(routeModeFrom(summary({ id: "abc" }))).toMatchObject({ name: "abc", kind: "custom", scope: "personal" });
    expect(routeModeFrom(summary({ id: "" }))).toBeNull();
  });

  it("draws the built-ins as shipped for a cluster without the builtins", () => {
    const shipped = shippedRouteModes();
    expect(shipped.map((r) => r.id)).toEqual(["auto", "local", "apps"]);
    expect(jobSourcesOf(shipped[1]!)).toEqual({ fast: ["fleet:fastest"], strong: ["fleet:strongest"], reasoning: ["fleet:strongest"] });
    expect(jobSourcesOf(shipped[2]!)).toEqual({ fast: ["app:*"], strong: ["app:*"], reasoning: ["app:*"] });
  });
});

describe("who may change a route", () => {
  const personal = { kind: "custom", scope: "personal" } as const;
  const local = { kind: "local", scope: "cluster" } as const;
  it("lets anyone change their own, an owner or developer the built-ins, and nobody Auto's jobs", () => {
    expect(canEditRoute(personal, false)).toBe(true);
    expect(canEditRoute(local, false)).toBe(false);
    expect(canEditRoute(local, true)).toBe(true);
    expect(canEditRoute({ kind: "auto", scope: "cluster" }, true)).toBe(false);
  });
});

describe("a route page's bar", () => {
  const base = { editable: true, dirty: false, complete: true, busy: "" as const, asking: "" as const };
  const local = (customized: boolean) => ({ scope: "cluster" as const, customized });
  const mine = { scope: "personal" as const, customized: false };

  it("says who can change a route the reader cannot, and offers nothing", () => {
    expect(routeModeBar({ ...base, route: local(true), editable: false })).toEqual({ state: "Changed from shipped", detail: "An owner or developer can change it", tone: "none", acts: [] });
    expect(routeModeBar({ ...base, route: local(false), editable: false }).state).toBe("Shipped");
  });

  it("offers Restore default only on a changed built-in, and Delete only on a route of yours", () => {
    expect(routeModeBar({ ...base, route: local(false) }).acts).toEqual([]);
    expect(routeModeBar({ ...base, route: local(true) }).acts).toEqual(["restore"]);
    expect(routeModeBar({ ...base, route: mine })).toMatchObject({ state: "Saved", detail: "Only you use this route", acts: ["delete"] });
  });

  it("offers Save only when every job has a source", () => {
    expect(routeModeBar({ ...base, route: local(false), dirty: true })).toMatchObject({ state: "Unsaved changes", detail: "Applies to everyone on this cluster", acts: ["discard", "save"] });
    expect(routeModeBar({ ...base, route: local(false), dirty: true, complete: false })).toMatchObject({ detail: "Every job needs a source", acts: ["discard"] });
  });

  it("asks before restoring or deleting, in the bar", () => {
    expect(routeModeBar({ ...base, route: local(true), asking: "restore" })).toMatchObject({ state: "Restore the shipped sources?", acts: ["keep", "confirmRestore"] });
    expect(routeModeBar({ ...base, route: mine, asking: "delete" })).toMatchObject({ state: "Delete this route?", acts: ["keep", "confirmDelete"] });
    // A question the route cannot be asked is not asked.
    expect(routeModeBar({ ...base, route: local(true), asking: "delete" }).acts).toEqual(["restore"]);
  });

  it("offers nothing while a write is out", () => {
    expect(routeModeBar({ ...base, route: mine, busy: "deleting" })).toMatchObject({ state: "Deleting", tone: "busy", acts: [] });
  });
});

describe("a route's name", () => {
  it("is required, short, not one of yours already and not a built-in's", () => {
    expect(routeNameProblem("  ", [])).toBe("Name the route");
    expect(routeNameProblem("x".repeat(41), [])).toBe("Forty characters at most");
    expect(routeNameProblem("x".repeat(40), [])).toBe("");
    expect(routeNameProblem("out of credits", ["Out of credits"])).toBe("You have a route with that name");
    expect(routeNameProblem("LOCAL", [])).toBe("That name is a built-in route");
    expect(routeNameProblem("Emergency", ["Out of credits"])).toBe("");
  });

  it("lets a route keep its own name when renamed back", () => {
    expect(routeNameProblem("Out of credits", ["Out of credits"], "Out of credits")).toBe("");
  });
});

describe("a route's jobs", () => {
  const studio = machine({ id: "studio", name: "studio-mac", models: [chat("qwen3.8:27b", 27e9)] });

  it("are complete when every job has a source", () => {
    expect(jobsComplete({ fast: ["app:*"], strong: ["app:*"], reasoning: ["app:*"] })).toBe(true);
    expect(jobsComplete({ fast: ["app:*"], strong: [], reasoning: ["app:*"] })).toBe(false);
  });

  it("refuse a source twice, a fifth source, and anything that is not a source", () => {
    const f = facts({ machines: [studio] });
    expect(jobPlacementProblem("app:*", ["app:*"], f)).toBe("Already in this job");
    expect(jobPlacementProblem("app:codex", ["fleet:fastest", "fleet:strongest", "app:*", "federation:cheapest"], f)).toBe("A job holds 4 sources at most");
    expect(jobPlacementProblem("policy:localFirst", [], f)).toBe("Not a source a route can use");
    expect(jobPlacementProblem("fleet:qwen3.8:27b", ["app:*"], f)).toBe("");
  });

  it("say what serves them now, or why nothing does", () => {
    const f = facts({ machines: [studio] });
    expect(jobStatus([], f)).toEqual({ text: "No source yet", ready: false });
    expect(jobStatus(["fleet:strongest"], f)).toEqual({ text: "Serves with qwen3.8:27b", ready: true });
    expect(jobStatus(["app:codex"], f)).toEqual({ text: "Not installed on any machine", ready: false });
  });
});

describe("the fastest local model", () => {
  it("names the model the engine takes: the fewest parameters at or above its fast floor", () => {
    const studio = machine({ id: "studio", models: [chat("tiny:1b", 1e9), chat("qwen3.8:27b", 27e9), chat("qwen3.5:9b", 9e9)] });
    const fastest = readSource("fleet:fastest", facts({ machines: [studio] }));
    expect(fastest.readiness).toBe("ready");
    expect(fastest.serves).toBe("qwen3.5:9b");
  });

  it("says how many models could answer when no size is reported", () => {
    const studio = machine({ id: "studio", models: [chat("a:latest"), chat("b:latest")] });
    expect(readSource("fleet:fastest", facts({ machines: [studio] })).fact).toBe("2 models");
  });
});

describe("a reason on its own line", () => {
  it("starts with a capital, unless it starts with a name", () => {
    expect(sentence("busy, 2 of 2")).toBe("Busy, 2 of 2");
    expect(sentence("not installed on any machine")).toBe("Not installed on any machine");
    expect(sentence("qwen3.8:27b")).toBe("qwen3.8:27b");
    expect(sentence("studio-mac is offline")).toBe("studio-mac is offline");
    expect(sentence("")).toBe("");
  });
});

describe("a refused route write", () => {
  it("reads a stale revision as the shared stale sentence", () => {
    const refusal = routeRefusal(new Error("routing changed since revision 7; refresh and review before saving"));
    expect(refusal.stale).toBe(true);
    expect(STALE_SENTENCE).toBe("Routing changed since this page read it.");
  });
});

describe("Auto's balance and the person's budget", () => {
  it("reads the balance with MemQL's recommended values, clamped to 0 to 100", () => {
    expect(autoBalanceFrom([{ id: "auto", speed: 70, cost: 140, quality: -3, recommended: { speed: 50, cost: 50, quality: 50 }, customized: true, revision: 9 }])).toEqual({
      speed: 70,
      cost: 100,
      quality: 0,
      recommended: { speed: 50, cost: 50, quality: 50 },
      customized: true,
      revision: 9,
    });
    expect(autoBalanceFrom([])).toBeNull();
  });

  it("reads no budget as no budget, and where the spend stands", () => {
    expect(budgetFrom([{ id: "budget", monthlyLimitUSD: null, spentThisMonthUSD: 3.5, resetsAt: "2026-11-01T00:00:00Z", state: "none" }])).toEqual({
      monthlyLimitUSD: null,
      spentThisMonthUSD: 3.5,
      resetsAt: "2026-11-01T00:00:00Z",
      state: "none",
    });
    expect(budgetFrom([{ id: "budget", monthlyLimitUSD: 50, spentThisMonthUSD: 51, state: "over" }])).toMatchObject({ monthlyLimitUSD: 50, state: "over" });
    expect(budgetFrom([{ id: "budget", monthlyLimitUSD: 50, spentThisMonthUSD: 1, state: "sideways" }])!.state).toBe("");
  });

  it("writes dollars the way a person reads them", () => {
    expect(usd(12.4)).toBe("$12.40");
    expect(usd(50)).toBe("$50.00");
    expect(usd(1200)).toBe("$1,200");
  });
});

describe("the cluster's embedding model, as read", () => {
  const read = (over: Partial<{ value: string | null; loading: boolean; error: string; available: boolean }>) =>
    embedderReading({ value: null, loading: false, error: "", available: true, ...over });
  it("tells none bound from not read", () => {
    expect(read({ value: "nomic-embed" })).toEqual({ state: "bound", name: "nomic-embed" });
    expect(read({ value: "" })).toEqual({ state: "none" });
    expect(read({ loading: true })).toEqual({ state: "loading" });
    expect(read({ error: "denied" })).toEqual({ state: "unread", error: "denied" });
    expect(read({ available: false })).toEqual({ state: "unread", error: "" });
  });
});

describe("fleet capacity", () => {
  const studio = (over: Record<string, unknown> = {}) => ({
    machineId: "v1:worker:registration:studio",
    memory: { totalBytes: 64e9, freeBytes: 9e9, kind: "unified" },
    loaded: ["qwen3.8:27b"],
    models: [
      { modelId: "qwen3.8:27b", inFlight: 2, slots: 2, tokensPerSecond: 31.4, medianLatencyMs: 900 },
      { modelId: "qwen3.5:9b", inFlight: 0, slots: 2, needBytes: 20e9 },
    ],
    ...over,
  });

  it("reads one row carrying the machines, or a row per machine, with bare ids", () => {
    const one = capacityFrom([{ id: "fleetCapacity", machines: [studio()] }]);
    const rows = capacityFrom([studio()]);
    expect(one).toEqual(rows);
    expect(one.machines[0]).toMatchObject({ machineId: "studio", memoryTotalBytes: 64e9, memoryFreeBytes: 9e9, memoryKind: "unified", loaded: ["qwen3.8:27b"] });
    expect(one.machines[0]!.models[0]).toMatchObject({ modelId: "qwen3.8:27b", inFlight: 2, slots: 2, loaded: true });
  });

  it("reads memory written flat, and the other names a reply may use", () => {
    const read = capacityFrom([{ registrationId: "studio", memoryFreeBytes: 3e9, loadedModels: [], models: [{ model: "m", active: 1, maxConcurrent: 4, memoryNeedBytes: 5e9 }] }]);
    expect(read.machines[0]).toMatchObject({ machineId: "studio", memoryFreeBytes: 3e9 });
    expect(read.machines[0]!.models[0]).toMatchObject({ modelId: "m", inFlight: 1, slots: 4, needBytes: 5e9 });
  });

  it("says a model is busy when every slot is in use", () => {
    const capacity = capacityFrom([studio()]);
    expect(capacityReason(capacity, "studio", "qwen3.8:27b")).toBe("busy, 2 of 2");
    expect(capacityReason(capacity, "v1:worker:registration:studio", "qwen3.8:27b")).toBe("busy, 2 of 2");
  });

  it("says a model that is not loaded needs its memory and headroom free", () => {
    const capacity = capacityFrom([studio()]);
    expect(capacityReason(capacity, "studio", "qwen3.5:9b")).toBe("needs 20 GB, 9 GB free");
    // Loaded already: nothing to fit.
    expect(capacityReason(capacityFrom([studio({ loaded: ["qwen3.5:9b"] })]), "studio", "qwen3.5:9b")).toBe("");
    // Room with headroom to spare.
    expect(capacityReason(capacityFrom([studio({ memory: { freeBytes: 40e9 } })]), "studio", "qwen3.5:9b")).toBe("");
  });

  it("gives no reason from a figure that was not reported", () => {
    const silent = capacityFrom([{ machineId: "studio", models: [{ modelId: "qwen3.5:9b", needBytes: 20e9 }, { modelId: "qwen3.8:27b", inFlight: 5 }] }]);
    expect(capacityReason(silent, "studio", "qwen3.5:9b")).toBe("");
    expect(capacityReason(silent, "studio", "qwen3.8:27b")).toBe("");
    expect(capacityReason(null, "studio", "qwen3.8:27b")).toBe("");
    expect(capacityReason(silent, "other", "qwen3.8:27b")).toBe("");
  });

  it("writes gigabytes as the router's reasons do", () => {
    expect(gigabytes(20e9)).toBe("20 GB");
    expect(gigabytes(9e9)).toBe("9 GB");
    expect(gigabytes(0.6e9)).toBe("0.6 GB");
  });

  it("makes a source that cannot take the call say why, and serve with one that can", () => {
    const capacity = capacityFrom([studio()]);
    const box = machine({ id: "studio", name: "studio-mac", models: [chat("qwen3.8:27b", 27e9), chat("qwen3.5:9b", 9e9)] });
    // Neither model can take a call: each selector says why the model it
    // would have taken cannot.
    const strongest = readSource("fleet:strongest", facts({ machines: [box], capacity }));
    expect(strongest.readiness).toBe("idle");
    expect(strongest.reason).toBe("busy, 2 of 2");
    expect(readSource("fleet:fastest", facts({ machines: [box], capacity })).reason).toBe("needs 20 GB, 9 GB free");
    expect(readSource("fleet:qwen3.8:27b", facts({ machines: [box], capacity })).reason).toBe("busy, 2 of 2");
    // Without the measurement, readiness is as it was.
    expect(readSource("fleet:strongest", facts({ machines: [box] })).serves).toBe("qwen3.8:27b");
    // With room for the smaller model, the selector serves with it.
    const roomy = capacityFrom([studio({ memory: { freeBytes: 40e9 } })]);
    expect(readSource("fleet:strongest", facts({ machines: [box], capacity: roomy })).serves).toBe("qwen3.5:9b");
  });
});
