import { describe, expect, it } from "vitest";

import {
  AUTO_ROUTING,
  ASK_ROUTES_STORAGE,
  LocalAskRouteStore,
  routeLabel,
  routingFor,
  sanitizeRouting,
  whereOf,
} from "../../src/ask/askRoute";

// The Ask route picker's contract with the engine (design brief section 6):
// AiChatMsg.provider is the SOURCE selector and AiChatMsg.level the Level.
// Both empty is Auto -- the cluster's rules decide.

describe("a choice becomes the wire's source and level", () => {
  it("maps each Where row to the engine's selector", () => {
    expect(routingFor("auto", "")).toEqual({ source: "", level: "" });
    expect(routingFor("claude-code", "strong")).toEqual({ source: "app:claude-code", level: "strong" });
    expect(routingFor("codex", "")).toEqual({ source: "app:codex", level: "" });
    // Local and Vendor are each ONE row, and the Effort names which end of
    // them serves: Fast asks for the fastest local model, and only Strong or
    // Reasoning is worth the strongest (most expensive) vendor.
    expect(routingFor("local", "")).toEqual({ source: "fleet:strongest", level: "" });
    expect(routingFor("local", "fast")).toEqual({ source: "fleet:fastest", level: "fast" });
    expect(routingFor("local", "reasoning")).toEqual({ source: "fleet:strongest", level: "reasoning" });
    expect(routingFor("vendor", "")).toEqual({ source: "federation:cheapest", level: "" });
    expect(routingFor("vendor", "fast")).toEqual({ source: "federation:cheapest", level: "fast" });
    expect(routingFor("vendor", "strong")).toEqual({ source: "federation:strongest", level: "strong" });
    expect(routingFor("vendor", "reasoning")).toEqual({ source: "federation:strongest", level: "reasoning" });
  });

  it("reads a selector back to its row", () => {
    expect(whereOf("")).toBe("auto");
    expect(whereOf("app:claude-code")).toBe("claude-code");
    expect(whereOf("app:codex")).toBe("codex");
    expect(whereOf("fleet:fastest")).toBe("local");
    expect(whereOf("fleet:qwen3.5:4b")).toBe("local");
    expect(whereOf("federation:strongest")).toBe("vendor");
    // Nothing on this picker writes a route by name; it is not one of its rows.
    expect(whereOf("policy:localFirst")).toBeNull();
  });
});

describe("the pill's words", () => {
  it("says Auto, the source's short name, and the level only when it is not Auto", () => {
    expect(routeLabel(AUTO_ROUTING)).toBe("Auto");
    expect(routeLabel({ source: "", level: "fast" })).toBe("Auto · Fast");
    expect(routeLabel({ source: "app:claude-code", level: "" })).toBe("Claude Code");
    expect(routeLabel({ source: "app:claude-code", level: "strong" })).toBe("Claude Code · Strong");
    expect(routeLabel({ source: "fleet:fastest", level: "fast" })).toBe("Local · Fast");
    expect(routeLabel({ source: "federation:strongest", level: "reasoning" })).toBe("Vendor · Reasoning");
    expect(routeLabel({ source: "fleet:qwen3.5:4b", level: "" })).toBe("qwen3.5:4b");
    expect(routeLabel({ source: "policy:localFirst", level: "" })).toBe("localFirst");
  });
});

describe("sanitizing a stored choice", () => {
  it("keeps a well-formed choice and repairs everything else to Auto", () => {
    expect(sanitizeRouting({ source: "app:codex", level: "fast" })).toEqual({ source: "app:codex", level: "fast" });
    expect(sanitizeRouting({ source: "app:codex", level: "turbo" })).toEqual({ source: "app:codex", level: "" });
    expect(sanitizeRouting({ source: 42, level: "fast" })).toEqual({ source: "", level: "fast" });
    expect(sanitizeRouting(null)).toEqual(AUTO_ROUTING);
    expect(sanitizeRouting("app:codex")).toEqual(AUTO_ROUTING);
  });
});

class MemoryStorage implements Storage {
  private items = new Map<string, string>();
  get length() { return this.items.size; }
  clear() { this.items.clear(); }
  getItem(key: string) { return this.items.get(key) ?? null; }
  key(index: number) { return [...this.items.keys()][index] ?? null; }
  removeItem(key: string) { this.items.delete(key); }
  setItem(key: string, value: string) { this.items.set(key, value); }
}

describe("the per-viewer store", () => {
  it("remembers a choice per conversation and answers Auto for any other", () => {
    const storage = new MemoryStorage();
    const store = new LocalAskRouteStore(storage);
    store.save("c1", { source: "app:claude-code", level: "strong" });
    store.save("c2", { source: "fleet:fastest", level: "fast" });
    const reread = new LocalAskRouteStore(storage);
    expect(reread.load("c1")).toEqual({ source: "app:claude-code", level: "strong" });
    expect(reread.load("c2")).toEqual({ source: "fleet:fastest", level: "fast" });
    expect(reread.load("c3")).toEqual(AUTO_ROUTING);
  });

  it("forgets a conversation that goes back to Auto rather than storing Auto", () => {
    const storage = new MemoryStorage();
    const store = new LocalAskRouteStore(storage);
    store.save("c1", { source: "app:codex", level: "" });
    store.save("c1", AUTO_ROUTING);
    expect(JSON.parse(storage.getItem(ASK_ROUTES_STORAGE)!).routes).toEqual({});
  });

  it("keeps only the most recent choices", () => {
    const storage = new MemoryStorage();
    const store = new LocalAskRouteStore(storage, 3);
    for (const id of ["a", "b", "c", "d"]) store.save(id, { source: "app:codex", level: "" });
    expect(store.load("a")).toEqual(AUTO_ROUTING);
    expect(store.load("d")).toEqual({ source: "app:codex", level: "" });
    expect(Object.keys(JSON.parse(storage.getItem(ASK_ROUTES_STORAGE)!).routes)).toEqual(["b", "c", "d"]);
  });

  it("works with corrupt or unavailable storage: Auto, never a crash", () => {
    const corrupt = new MemoryStorage();
    corrupt.setItem(ASK_ROUTES_STORAGE, "{not json");
    expect(new LocalAskRouteStore(corrupt).load("c1")).toEqual(AUTO_ROUTING);
    corrupt.setItem(ASK_ROUTES_STORAGE, JSON.stringify({ version: 9, routes: { c1: { source: "app:codex", level: "" } } }));
    expect(new LocalAskRouteStore(corrupt).load("c1")).toEqual(AUTO_ROUTING);

    const throwing = {
      getItem: () => { throw new Error("SecurityError"); },
      setItem: () => { throw new Error("QuotaExceededError"); },
    } as unknown as Storage;
    const store = new LocalAskRouteStore(throwing);
    expect(() => store.save("c1", { source: "app:codex", level: "" })).not.toThrow();
    expect(new LocalAskRouteStore(throwing).load("c1")).toEqual(AUTO_ROUTING);
    expect(new LocalAskRouteStore(null).load("c1")).toEqual(AUTO_ROUTING);
  });

  it("keeps a choice for the session when the browser refuses to store it", () => {
    // A private window, blocked site data, a full quota: the write fails, and
    // the choice still holds for as long as this page does.
    const throwing = {
      getItem: () => { throw new Error("SecurityError"); },
      setItem: () => { throw new Error("QuotaExceededError"); },
    } as unknown as Storage;
    const store = new LocalAskRouteStore(throwing);
    store.save("c1", { source: "app:codex", level: "" });
    expect(store.load("c1")).toEqual({ source: "app:codex", level: "" });
    expect(new LocalAskRouteStore(null).load("c1")).toEqual(AUTO_ROUTING);

    // Readable but full: what this page chose wins over what is stored.
    const full = new MemoryStorage();
    full.setItem(ASK_ROUTES_STORAGE, JSON.stringify({ version: 1, routes: { c2: { source: "app:codex", level: "" } } }));
    full.setItem = () => { throw new Error("QuotaExceededError"); };
    const fullStore = new LocalAskRouteStore(full);
    fullStore.save("c2", AUTO_ROUTING);
    expect(fullStore.load("c2")).toEqual(AUTO_ROUTING);
    fullStore.save("c3", { source: "fleet:fastest", level: "fast" });
    expect(fullStore.load("c3")).toEqual({ source: "fleet:fastest", level: "fast" });
  });
});
