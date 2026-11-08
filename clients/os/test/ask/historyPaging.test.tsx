import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import type { Dispatcher } from "@znasllc-io/memql-sdk-core/client";
import { AskConversations } from "../../src/ask/AskConversations";
import { AskProvider, useAsk } from "../../src/ask/AskProvider";
import { ConversationSession, type AskConversationStore, type ConversationSummary } from "../../src/ask/conversationSession";
import { SdkAskTransport } from "../../src/ask/sdkTransport";
import { READY_ASK } from "../../src/ask/useAskReadiness";

afterEach(() => { cleanup(); vi.useRealTimers(); });
const rows = Array.from({ length: 25 }, (_, n) => ({ id: `chat-${n}`, title: `Conversation ${n}` }));
function fixture() {
  const page = vi.fn<NonNullable<AskConversationStore["page"]>>(async ({ cursor, search }) => {
    const filtered = rows.filter(row => !search || row.title.toLocaleLowerCase().includes(search));
    const offset = Number(cursor ?? 0);
    return { rows: filtered.slice(offset, offset + 10), cursor: offset + 10 < filtered.length ? String(offset + 10) : "" };
  });
  const store = { page, list: vi.fn(async () => []), create: vi.fn(async () => rows[0]!), read: vi.fn(async () => []) };
  const transport = { conversations: store, ask: vi.fn(() => ({ cancel() {} })) };
  return { page, store, transport, session: new ConversationSession(transport) };
}
it("does no history work on shell mount or while the panel is closed", async () => {
  const f = fixture(); let session!: ConversationSession;
  function Capture() { session = useAsk().conversation; return null; }
  const view = render(<AskProvider transport={f.transport} availability={READY_ASK}><Capture /></AskProvider>);
  await act(async () => { window.dispatchEvent(new Event("focus")); session.refreshHistory(); await session.refresh(); });
  expect(f.page).not.toHaveBeenCalled(); expect(f.store.list).not.toHaveBeenCalled();
  view.unmount(); f.session.dispose();
});
it("loads ten, appends ten, restores both pages, and searches older titles on the server", async () => {
  const f = fixture(); const props = { conversation: f.session, onOpen: vi.fn() };
  let view = render(<AskConversations {...props} />);
  await screen.findByRole("button", { name: "Conversation 9" });
  expect(screen.queryByRole("button", { name: "Conversation 10" })).toBeNull();
  expect(f.page).toHaveBeenCalledOnce();
  fireEvent.click(screen.getByRole("button", { name: "Load more" }));
  await screen.findByRole("button", { name: "Conversation 19" });
  expect(f.page).toHaveBeenCalledTimes(2); view.unmount();
  view = render(<AskConversations {...props} />);
  expect(screen.getByRole("button", { name: "Conversation 19" })).toBeTruthy(); expect(f.page).toHaveBeenCalledTimes(2);
  fireEvent.change(screen.getByRole("textbox", { name: "Find a conversation" }), { target: { value: "  Conversation 24  " } });
  await screen.findByRole("button", { name: "Conversation 24" });
  expect(f.page).toHaveBeenLastCalledWith(expect.objectContaining({ search: "conversation 24", cursor: undefined }));
  fireEvent.change(screen.getByRole("textbox", { name: "Find a conversation" }), { target: { value: "" } });
  await screen.findByRole("button", { name: "Conversation 19" }); expect(f.page).toHaveBeenCalledTimes(3);
  view.unmount(); f.session.dispose();
});
it("coalesces event bursts and retains loaded depth during refresh", async () => {
  vi.useFakeTimers(); const f = fixture(); const release = f.session.retainHistory();
  await f.session.refresh(); await f.session.loadMoreHistory(); f.page.mockClear();
  f.session.refreshHistory(); f.session.refreshHistory(); f.session.refreshHistory();
  expect(f.page).not.toHaveBeenCalled();
  await vi.advanceTimersByTimeAsync(100);
  expect(f.page).toHaveBeenCalledTimes(2); expect(f.session.getSnapshot().conversations).toHaveLength(20);
  release(); f.page.mockClear(); f.session.refreshHistory();
  await vi.advanceTimersByTimeAsync(1000); expect(f.page).not.toHaveBeenCalled(); f.session.dispose();
});
it("cancels the final consumer's read and discards late rows from an old search", async () => {
  const f = fixture(); let resolve!: (page: { rows: ConversationSummary[]; cursor: string }) => void;
  f.page.mockImplementationOnce(() => new Promise(r => { resolve = r; }));
  const release = f.session.retainHistory(); const pending = f.session.refresh();
  const signal = f.page.mock.calls[0]![0].signal!; release(); expect(signal.aborted).toBe(true);
  f.session.setHistorySearch("conversation 24"); const nextRelease = f.session.retainHistory();
  await f.session.refresh(); resolve({ rows, cursor: "old" }); await pending;
  expect(f.session.getSnapshot().conversations.map(row => row.id)).toEqual(["chat-24"]);
  nextRelease(); f.session.dispose();
});
it("does not share rows between owner transports", async () => {
  const one = fixture(); const two = fixture(); two.page.mockResolvedValue({ rows: [], cursor: "" });
  const release = one.session.retainHistory(); await one.session.refresh();
  expect(two.session.getSnapshot().conversations).toEqual([]);
  const otherRelease = two.session.retainHistory(); await two.session.refresh();
  expect(two.session.getSnapshot().conversations).toEqual([]); release(); otherRelease(); one.session.dispose(); two.session.dispose();
});
it("does not turn a transport interruption into a retry loop", async () => {
  vi.useFakeTimers(); const f = fixture();
  f.page.mockRejectedValue(Object.assign(new Error("stream closed"), { reason: "transport" }));
  const release = f.session.retainHistory(); await f.session.refresh();
  await vi.advanceTimersByTimeAsync(10_000); expect(f.page).toHaveBeenCalledOnce();
  expect(f.session.getSnapshot().historyError).toBe("");
  f.page.mockResolvedValue({ rows: rows.slice(0, 10), cursor: "10" });
  f.session.refreshHistory(); await vi.advanceTimersByTimeAsync(100);
  expect(f.page).toHaveBeenCalledTimes(2); expect(f.session.getSnapshot().conversations).toHaveLength(10);
  release(); f.session.dispose();
});
it("passes title search and cursor through the generated SDK without draining history", async () => {
  const sendAndWait = vi.fn(async () => ({ queryResult: { result: { data: [] }, meta: { cursor: "next" } } }));
  const transport = new SdkAskTransport(() => ({ sendAndWait } as unknown as Dispatcher));
  const signal = new AbortController().signal;
  await transport.conversations.page!({ search: "older report", cursor: "page-2", signal });
  expect(sendAndWait).toHaveBeenCalledOnce();
  expect(sendAndWait).toHaveBeenCalledWith(expect.objectContaining({ executeQuery: expect.objectContaining({ query: 'query myAskConversationsBrowse(search: "older report")', cursor: "page-2" }) }), signal);
});
