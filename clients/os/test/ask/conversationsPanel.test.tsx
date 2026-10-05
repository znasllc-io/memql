import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AskSurface } from "../../src/ask/AskSurface";
import { ConversationSession, type AskTurn } from "../../src/ask/conversationSession";
import type { AskCallbacks } from "../../src/ask/askController";
import { READY_ASK } from "../../src/ask/useAskReadiness";

afterEach(cleanup);
const items = [{ id: "current", title: "Current thread", createdAt: "2026-10-05T12:00:00Z" }, { id: "older", title: "Older conversation", createdAt: "2026-10-04T12:00:00Z" }];
function turns(prompt: string): AskTurn[] { return [{ id: prompt, prompt, answer: "Saved answer", state: "done", startedAt: "2026-10-04T12:00:00Z", endedAt: "2026-10-04T12:00:30Z", activity: [] }]; }
function deferred<T>() { let resolve!: (value: T) => void; let reject!: (error: Error) => void; const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }
async function setup() {
 let callbacks!: AskCallbacks;
 const store = { list: vi.fn(async () => items), create: vi.fn(async () => items[0]!), read: vi.fn(async (id: string) => turns(id === "current" ? "Current message" : "Older message")) };
 const cancel = vi.fn();
 const transport = { conversations: store, ask: vi.fn((_p: string, _c: string | null, on: AskCallbacks) => { callbacks = on; return { cancel }; }) };
 const session = new ConversationSession(transport);
 await session.select("current");
 const onClose = vi.fn();
 render(<AskSurface transport={transport} conversation={session} availability={READY_ASK} variant="sheet" onClose={onClose} />);
 return { session, store, transport, cancel, onClose, callbacks: () => callbacks };
}
async function openList() { await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Conversations" })); }); }

it("opens a searchable full-panel list and switches directly between it and Activity without back buttons", async () => {
 await setup(); await openList();
 expect(screen.getByRole("region", { name: "Conversations" })).toBeTruthy();
 expect(screen.queryByRole("log")).toBeNull();
 expect(screen.queryByRole("button", { name: /back|close activity/i })).toBeNull();
 fireEvent.change(screen.getByRole("textbox", { name: "Find a conversation" }), { target: { value: " OLDER " } });
 expect(screen.queryByRole("button", { name: "Current thread" })).toBeNull();
 expect(screen.getByRole("button", { name: "Older conversation" })).toBeTruthy();
 fireEvent.click(screen.getByRole("button", { name: "Activity" }));
 expect(screen.getByRole("region", { name: "Conversation activity" })).toBeTruthy();
 expect(screen.getByRole("button", { name: "Conversations" })).toBeTruthy();
 fireEvent.click(screen.getByRole("button", { name: "Conversation" }));
 expect(screen.getByText("Current message")).toBeTruthy();
});

it("reads a saved conversation without resubmitting and focuses its composer on success", async () => {
 const w = await setup(); await openList();
 const read = deferred<AskTurn[]>(); w.store.read.mockReturnValueOnce(read.promise);
 fireEvent.click(screen.getByRole("button", { name: "Older conversation" }));
 expect(screen.getByText("Opening conversation…")).toBeTruthy();
 expect(w.session.getSnapshot().selectedId).toBe("current");
 await act(async () => read.resolve(turns("Older message")));
 expect(screen.getByText("Older message")).toBeTruthy();
 expect(screen.queryByText("Current message")).toBeNull();
 expect(document.activeElement).toBe(screen.getByRole("textbox", { name: "Ask" }));
 expect(w.transport.ask).not.toHaveBeenCalled();
 expect([...screen.getByRole("log").querySelectorAll("time")].at(-1)?.dateTime).toBe("2026-10-04T12:00:30Z");
});

it("cancels a pending selection when returning and ignores its late response", async () => {
 const w = await setup();
 fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "My draft" } });
 await openList(); const read = deferred<AskTurn[]>(); w.store.read.mockReturnValueOnce(read.promise);
 fireEvent.click(screen.getByRole("button", { name: "Older conversation" }));
 const toggle = screen.getByRole("button", { name: "Conversation" }); toggle.focus(); fireEvent.keyDown(toggle, { key: "Escape" });
 await act(async () => read.resolve(turns("Late message")));
 expect(w.session.getSnapshot().selectedId).toBe("current");
 expect(screen.getByText("Current message")).toBeTruthy();
 expect(screen.queryByText("Late message")).toBeNull();
 expect((screen.getByRole("textbox", { name: "Ask" }) as HTMLTextAreaElement).value).toBe("My draft");
 expect(document.activeElement).toBe(screen.getByRole("button", { name: "Conversations" }));
 expect(w.onClose).not.toHaveBeenCalled(); expect(w.cancel).not.toHaveBeenCalled();
});

it("keeps a failed selection in the list with footer Retry and preserves the current transcript", async () => {
 const w = await setup(); await openList(); w.store.read.mockRejectedValueOnce(new Error("Conversation could not be opened."));
 await act(async () => fireEvent.click(screen.getByRole("button", { name: "Older conversation" })));
 expect(screen.getByRole("alert").textContent).toContain("Conversation could not be opened.");
 expect(w.session.getSnapshot().selectedId).toBe("current");
 const retry = within(screen.getByRole("group", { name: "What you can do with this" })).getByRole("button", { name: "Retry" });
 await act(async () => fireEvent.click(retry));
 expect(screen.getByText("Older message")).toBeTruthy();
 expect(screen.queryByRole("alert")).toBeNull();
});

it("shows loading and a retryable list failure without flashing the empty state", async () => {
 const w = await setup(); const list = deferred<typeof items>(); w.store.list.mockReturnValueOnce(list.promise);
 fireEvent.click(screen.getByRole("button", { name: "Conversations" }));
 expect(screen.getByText("Loading conversations")).toBeTruthy(); expect(screen.queryByText("No conversations yet.")).toBeNull();
 await act(async () => list.reject(new Error("Could not load conversations.")));
 expect(screen.getByRole("alert")).toBeTruthy(); expect(screen.queryByText("No conversations yet.")).toBeNull();
 await act(async () => fireEvent.click(screen.getByRole("button", { name: "Retry" })));
 expect(screen.getByRole("button", { name: "Older conversation" })).toBeTruthy();
});

it("keeps existing rows during refresh and distinguishes no results from no conversations", async () => {
 const w = await setup(); await openList();
 fireEvent.click(screen.getByRole("button", { name: "Conversation" }));
 const list = deferred<typeof items>(); w.store.list.mockReturnValueOnce(list.promise);
 fireEvent.click(screen.getByRole("button", { name: "Conversations" }));
 expect(screen.getByRole("button", { name: "Older conversation" })).toBeTruthy();
 expect(screen.getByText("Refreshing conversations")).toBeTruthy();
 fireEvent.change(screen.getByRole("textbox", { name: "Find a conversation" }), { target: { value: "nothing matches" } });
 expect(screen.getByText("No conversations match.")).toBeTruthy();
 await act(async () => list.resolve([]));
 expect(screen.getByText("No conversations yet.")).toBeTruthy();
 expect(screen.queryByText("No conversations match.")).toBeNull();
});

it("allows browsing during a reply, but does not switch away from its active conversation", async () => {
 const w = await setup();
 fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "Hello" } });
 fireEvent.click(screen.getByRole("button", { name: "Send" })); await openList();
 expect((screen.getByRole("button", { name: "Older conversation" }) as HTMLButtonElement).disabled).toBe(true);
 expect(screen.getByRole("button", { name: "Stop reply" })).toBeTruthy();
 await act(async () => fireEvent.click(screen.getByRole("button", { name: "Current thread" })));
 expect(screen.getByRole("log")).toBeTruthy();
 act(() => { w.callbacks().delta("Hello Jose"); w.callbacks().done(); });
 expect(screen.getByText("Hello Jose")).toBeTruthy(); expect(w.cancel).not.toHaveBeenCalled();
});
