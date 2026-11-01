import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AskSurface } from "../../src/ask/AskSurface";
import { ConversationSession, type AskTurn } from "../../src/ask/conversationSession";
import { READY_ASK } from "../../src/ask/useAskReadiness";

const clipboardDescriptor = Object.getOwnPropertyDescriptor(navigator, "clipboard");
const sessions: ConversationSession[] = [];
afterEach(() => {
  cleanup();
  sessions.splice(0).forEach(session => session.dispose());
  if (clipboardDescriptor) Object.defineProperty(navigator, "clipboard", clipboardDescriptor);
  else Reflect.deleteProperty(navigator, "clipboard");
  vi.useRealTimers();
});
const first: AskTurn = {
  id: "first", runId: "run-first", prompt: "Earlier question", answer: "Earlier reply", state: "done",
  startedAt: "2026-10-05T10:00:00Z", endedAt: "2026-10-05T10:00:01Z",
  activity: [{ id: "call-first", kind: "model", name: "Earlier call", phase: "completed", at: "2026-10-05T10:00:00Z" }],
};
const second: AskTurn = {
  id: "second", runId: "run-second", prompt: "Later question", answer: "Later reply", state: "done",
  startedAt: "2026-10-05T11:00:00Z", endedAt: "2026-10-05T11:00:01Z",
  activity: [{ id: "call-second", kind: "action", name: "Later action", phase: "completed", at: "2026-10-05T11:00:00Z" }],
};
async function setup(turns = [first, second], variant: "sheet" | "widget" = "sheet") {
  const writeText = vi.fn(async (_text: string) => {});
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const transport = {
    ask: vi.fn(() => ({ cancel: vi.fn() })),
    conversations: { list: async () => [], create: async () => ({ id: "chat", title: "Chat" }), read: async () => turns },
  };
  const session = new ConversationSession(transport); sessions.push(session);
  await session.select("chat");
  render(<AskSurface conversation={session} transport={transport} availability={READY_ASK} variant={variant} />);
  return { session, writeText, transport };
}
function actions(text: string) {
  return within(screen.getByText(text).closest<HTMLElement>(".os-ask-message")!).getByRole("button", { name: "Message actions" });
}
function open(text: string) { const trigger = actions(text); fireEvent.click(trigger); return trigger; }
async function choose(label: string) {
  await act(async () => { fireEvent.click(screen.getByRole("menuitem", { name: label })); });
}

it.each(["sheet", "widget"] as const)("offers a message menu in %s without an Activity header action or changing the clipboard on text selection", async variant => {
  const { writeText } = await setup(undefined, variant);
  expect(screen.queryByRole("button", { name: "Activity" })).toBeNull();
  expect(screen.getAllByRole("button", { name: "Message actions" })).toHaveLength(4);
  const text = screen.getByText("Earlier question");
  const selection = document.getSelection()!;
  const range = document.createRange(); range.selectNodeContents(text);
  selection.removeAllRanges(); selection.addRange(range);
  fireEvent.click(text);
  expect(selection.toString()).toBe("Earlier question");
  expect(writeText).not.toHaveBeenCalled();
  expect(screen.queryByRole("menu")).toBeNull();
  selection.removeAllRanges();
  const trigger = actions("Earlier question"); trigger.focus();
  fireEvent.keyDown(trigger, { key: "ArrowDown" });
  expect(trigger.getAttribute("aria-expanded")).toBe("true");
  expect(document.activeElement).toBe(screen.getByRole("menuitem", { name: "Copy message" }));
  fireEvent.keyDown(document.activeElement!, { key: "ArrowDown" });
  expect(document.activeElement).toBe(screen.getByRole("menuitem", { name: "Copy details" }));
  fireEvent.keyDown(document.activeElement!, { key: "Escape" });
  expect(screen.queryByRole("menu")).toBeNull();
  expect(document.activeElement).toBe(trigger);
});

it("copies the selected request or reply and includes only its turn in details", async () => {
  const { writeText } = await setup();
  open("Earlier question"); await choose("Copy message");
  expect(writeText).toHaveBeenLastCalledWith("Earlier question");
  open("Later reply"); await choose("Copy message");
  expect(writeText).toHaveBeenLastCalledWith("Later reply");
  expect(screen.getAllByRole("status").some(el => el.textContent === "Copied message")).toBe(true);
  open("Earlier question"); await choose("Copy details");
  const details = JSON.parse(writeText.mock.lastCall![0]);
  expect(details).toMatchObject({ conversationId: "chat", turnId: "first", messageKey: "first:user", role: "user", kind: "request", text: "Earlier question", at: first.startedAt, runId: "run-first", activityScope: "turn", activity: first.activity });
  expect(JSON.stringify(details)).not.toContain("Later action");
  expect(JSON.stringify(details)).not.toContain("Earlier reply");
});

it("reports clipboard refusal and allows a successful retry", async () => {
  const { writeText } = await setup();
  writeText.mockRejectedValueOnce(new Error("denied"));
  open("Earlier reply"); await choose("Copy message");
  expect(screen.getByText("The browser refused the copy — nothing was copied.")).toBeTruthy();
  expect(screen.queryByText("Copied message")).toBeNull();
  open("Earlier reply"); await choose("Copy message");
  expect(screen.queryByText("The browser refused the copy — nothing was copied.")).toBeNull();
  expect(screen.getByText("Copied message")).toBeTruthy();
});

it("reports an unavailable clipboard without claiming success", async () => {
  await setup();
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
  open("Earlier reply"); await choose("Copy details");
  expect(screen.getByText("This browser offers no clipboard — nothing was copied.")).toBeTruthy();
  expect(screen.queryByText("Copied details")).toBeNull();
});

it("scopes activity, excludes dictation and other turns, and restores draft, scroll and keyboard focus", async () => {
  const { session, transport } = await setup();
  act(() => session.recordDictation({ id: "dictation", kind: "model", name: "Unsent dictation", phase: "completed", at: first.startedAt }));
  fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "Unsent draft" } });
  const log = screen.getByRole("log");
  Object.defineProperties(log, { scrollHeight: { configurable: true, value: 1200 }, clientHeight: { configurable: true, value: 300 } });
  log.scrollTop = 125; fireEvent.scroll(log);
  const triggerKey = open("Earlier reply").getAttribute("data-ask-message-menu");
  await choose("View activity");
  const region = screen.getByRole("region", { name: "Message activity" });
  expect(document.activeElement).toBe(region);
  expect(within(region).getByText("Earlier call")).toBeTruthy();
  expect(within(region).queryByText("Later action")).toBeNull();
  expect(within(region).queryByText("Unsent dictation")).toBeNull();
  fireEvent.keyDown(region, { key: "Escape" });
  expect(screen.getByRole("log").scrollTop).toBe(125);
  expect((screen.getByRole("textbox", { name: "Ask" }) as HTMLTextAreaElement).value).toBe("Unsent draft");
  expect(document.activeElement?.getAttribute("data-ask-message-menu")).toBe(triggerKey);
  expect(transport.ask).not.toHaveBeenCalled();
});

it("copies background acknowledgment and completion separately while both open the original turn activity", async () => {
  const task = { ...first, background: true, acknowledgement: "I will check.", acknowledgedAt: first.startedAt };
  const { writeText } = await setup([task, second]);
  open("I will check."); await choose("Copy message");
  expect(writeText).toHaveBeenLastCalledWith("I will check.");
  open("Earlier reply"); await choose("Copy message");
  expect(writeText).toHaveBeenLastCalledWith("Earlier reply");
  open("Earlier reply"); await choose("Copy details");
  expect(JSON.parse(writeText.mock.lastCall![0])).toMatchObject({ messageKey: "first:result:assistant", turnId: "first", kind: "response", text: "Earlier reply", activityScope: "turn", activity: first.activity });
  open("I will check."); await choose("Copy details");
  expect(JSON.parse(writeText.mock.lastCall![0])).toMatchObject({ messageKey: "first:assistant", turnId: "first", kind: "acknowledgement", text: "I will check.", activityScope: "turn", activity: first.activity });
  open("I will check."); await choose("View activity");
  expect(within(screen.getByRole("region", { name: "Message activity" })).getByText("Earlier call")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Back to conversation" }));
  expect(document.activeElement).toBe(actions("I will check."));
});

it("shows a truthful empty activity state and closes the menu on outside interaction or Tab", async () => {
  await setup([{ ...first, activity: [] }]);
  open("Earlier reply");
  fireEvent.pointerDown(document.body);
  expect(screen.queryByRole("menu")).toBeNull();
  const trigger = open("Earlier reply");
  fireEvent.pointerDown(trigger);
  fireEvent.click(trigger);
  expect(screen.queryByRole("menu")).toBeNull();
  open("Earlier reply");
  fireEvent.keyDown(screen.getByRole("menuitem", { name: "Copy message" }), { key: "Tab" });
  expect(screen.queryByRole("menu")).toBeNull();
  expect(document.activeElement).toBe(trigger);
  open("Earlier reply"); await choose("View activity");
  expect(screen.getByText("No activity recorded for this message.")).toBeTruthy();
});

it("never offers stopping a different message's current reply from historical activity", async () => {
  await setup([first]);
  fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "Current request" } });
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Send" })); });
  open("Earlier reply"); await choose("View activity");
  expect(screen.queryByRole("button", { name: "Stop reply" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Back to conversation" }));
  open("Current request"); await choose("View activity");
  expect(screen.getByRole("button", { name: "Stop reply" })).toBeTruthy();
});
