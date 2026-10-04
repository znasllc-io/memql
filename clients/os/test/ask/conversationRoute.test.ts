import { describe, expect, it, vi } from "vitest";
import type { Dispatcher } from "@znasllc-io/memql-sdk-core/client";

import { AUTO_ROUTING, LocalAskRouteStore, type AskRouteStore, type AskRouting } from "../../src/ask/askRoute";
import type { AskCallbacks, AskTransport } from "../../src/ask/askController";
import { ConversationSession, type AskTurn } from "../../src/ask/conversationSession";
import { SdkAskTransport } from "../../src/ask/sdkTransport";

// A conversation's route (design brief section 6): chosen per conversation,
// carried on every send, remembered with the conversation, and Auto for a new
// one.

class MemoryRoutes implements AskRouteStore {
  readonly saved = new Map<string, AskRouting>();
  load(id: string) { return this.saved.get(id) ?? { ...AUTO_ROUTING }; }
  save(id: string, routing: AskRouting) {
    if (routing.source === "" && routing.level === "") this.saved.delete(id);
    else this.saved.set(id, routing);
  }
}

function flush() { return new Promise<void>((resolve) => setTimeout(resolve, 0)); }

function setup(routes: AskRouteStore = new MemoryRoutes()) {
  const calls: { prompt: string; options: Parameters<AskTransport["ask"]>[3] }[] = [];
  let last!: AskCallbacks;
  let created = 0;
  const transport: AskTransport = {
    ask: (prompt, _context, on, options) => { calls.push({ prompt, options }); last = on; return { cancel: vi.fn() }; },
    conversations: {
      list: vi.fn(async () => []),
      create: vi.fn(async () => ({ id: `created-${++created}`, title: "New conversation" })),
      read: vi.fn(async () => []),
    },
  };
  const session = new ConversationSession(transport, routes);
  return { session, routes, calls, finish: () => { last.delta("ok"); last.done(); } };
}

const CLAUDE_STRONG: AskRouting = { source: "app:claude-code", level: "strong" };

describe("the conversation's route", () => {
  it("starts on Auto and sends Auto as an explicit, empty routing", async () => {
    const { session, calls } = setup();
    expect(session.getSnapshot().routing).toEqual(AUTO_ROUTING);
    session.send("hi", null);
    await flush();
    expect(calls[0]?.options).toEqual({ conversationId: "created-1", turnId: expect.any(String), routing: AUTO_ROUTING });
  });

  it("carries a choice made BEFORE the first message onto the conversation that message creates", async () => {
    const { session, calls, routes } = setup();
    session.setRouting(CLAUDE_STRONG);
    session.send("hi", null);
    await flush();
    expect(calls[0]?.options?.routing).toEqual(CLAUDE_STRONG);
    expect(routes.load("created-1")).toEqual(CLAUDE_STRONG);
  });

  it("applies to the selected conversation, is remembered with it, and a new conversation starts on Auto", async () => {
    const { session, calls, routes, finish } = setup();
    await session.select("c1");
    session.setRouting(CLAUDE_STRONG);
    expect(routes.load("c1")).toEqual(CLAUDE_STRONG);
    session.send("one", null);
    await flush();
    expect(calls.at(-1)?.options).toMatchObject({ conversationId: "c1", routing: CLAUDE_STRONG });
    finish();
    await flush();

    session.newConversation();
    expect(session.getSnapshot().routing).toEqual(AUTO_ROUTING);

    await session.select("c2");
    expect(session.getSnapshot().routing).toEqual(AUTO_ROUTING);
    await session.select("c1");
    expect(session.getSnapshot().routing).toEqual(CLAUDE_STRONG);
  });

  it("a choice changed while a reply streams applies to the NEXT message, not the running one", async () => {
    const { session, calls, finish } = setup();
    await session.select("c1");
    session.send("one", null);
    await flush();
    session.setRouting({ source: "fleet:fastest", level: "fast" });
    expect(calls).toHaveLength(1);
    expect(calls[0]?.options?.routing).toEqual(AUTO_ROUTING);
    finish();
    await flush();
    session.send("two", null);
    await flush();
    expect(calls[1]?.options?.routing).toEqual({ source: "fleet:fastest", level: "fast" });
  });
});

describe("a choice made while another conversation opens", () => {
  function deferredRead(routes: AskRouteStore) {
    const reads = new Map<string, { resolve: () => void; reject: (error: Error) => void }>();
    const transport: AskTransport = {
      ask: () => ({ cancel: vi.fn() }),
      conversations: {
        list: vi.fn(async () => []),
        create: vi.fn(async () => ({ id: "created", title: "New conversation" })),
        read: vi.fn((id: string) => new Promise<AskTurn[]>((resolve, reject) => { reads.set(id, { resolve: () => resolve([]), reject }); })),
      },
    };
    return { session: new ConversationSession(transport, routes), reads };
  }

  it("belongs to the conversation being opened, never to the one being left", async () => {
    const routes = new MemoryRoutes();
    routes.save("c1", { source: "app:codex", level: "" });
    const { session, reads } = deferredRead(routes);
    const first = session.select("c1");
    reads.get("c1")!.resolve();
    await first;
    expect(session.getSnapshot().routing).toEqual({ source: "app:codex", level: "" });

    const opening = session.select("c2");
    // The pill names the conversation being opened from the moment it is asked for.
    expect(session.getSnapshot().routing).toEqual(AUTO_ROUTING);
    session.setRouting(CLAUDE_STRONG);
    expect(routes.load("c1")).toEqual({ source: "app:codex", level: "" });
    reads.get("c2")!.resolve();
    await opening;
    expect(session.getSnapshot().selectedId).toBe("c2");
    expect(session.getSnapshot().routing).toEqual(CLAUDE_STRONG);
    expect(routes.load("c2")).toEqual(CLAUDE_STRONG);
  });

  it("a conversation that fails to open gives the pill back to the one still showing", async () => {
    const routes = new MemoryRoutes();
    routes.save("c1", { source: "app:codex", level: "" });
    const { session, reads } = deferredRead(routes);
    const first = session.select("c1");
    reads.get("c1")!.resolve();
    await first;

    const opening = session.select("c2");
    reads.get("c2")!.reject(new Error("gone"));
    await opening;
    expect(session.getSnapshot().selectedId).toBe("c1");
    expect(session.getSnapshot().routing).toEqual({ source: "app:codex", level: "" });
  });
});

describe("with browser storage unavailable", () => {
  it("keeps each conversation's choice for the session", async () => {
    const blocked = {
      getItem: () => { throw new Error("SecurityError"); },
      setItem: () => { throw new Error("SecurityError"); },
    } as unknown as Storage;
    const { session } = setup(new LocalAskRouteStore(blocked));
    await session.select("c1");
    session.setRouting(CLAUDE_STRONG);
    await session.select("c2");
    expect(session.getSnapshot().routing).toEqual(AUTO_ROUTING);
    await session.select("c1");
    expect(session.getSnapshot().routing).toEqual(CLAUDE_STRONG);
  });
});

describe("the SDK transport", () => {
  it("sends the source as provider and the level beside it", async () => {
    const stream = vi.fn(() => ({ result: Promise.resolve({ message: { content: "Done" } }), deltas: (async function* () { yield { textDelta: "Done" }; })() }));
    await new Promise<void>((done, error) => new SdkAskTransport(() => ({} as Dispatcher), stream).ask("hi", null, { delta: () => {}, done, error }, { conversationId: "c1", turnId: "t1", routing: CLAUDE_STRONG }));
    expect(stream).toHaveBeenCalledWith(expect.anything(), [{ role: "user", content: "hi" }], expect.objectContaining({ conversationId: "c1", requestId: "t1", provider: "app:claude-code", level: "strong" }));
  });

  it("leaves both unset on Auto so the cluster's rules decide", async () => {
    const stream = vi.fn(() => ({ result: Promise.resolve({ message: { content: "Done" } }), deltas: (async function* () { yield { textDelta: "Done" }; })() }));
    await new Promise<void>((done, error) => new SdkAskTransport(() => ({} as Dispatcher), stream).ask("hi", null, { delta: () => {}, done, error }, { conversationId: "c1", turnId: "t1", routing: AUTO_ROUTING }));
    const opts = (stream.mock.calls[0] as unknown as [unknown, unknown, Record<string, unknown>])[2];
    expect(opts.provider ?? "").toBe("");
    expect(opts.level ?? "").toBe("");
  });
});
