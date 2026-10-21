import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AskActivityLog } from "../../src/ask/AskActivityLog";
import { AskSurface } from "../../src/ask/AskSurface";
import { AskWorkQueue } from "../../src/ask/AskWorkQueue";
import type { AskCallbacks, AskTransport } from "../../src/ask/askController";
import { ConversationSession, pendingStops, type AskRouted, type AskRouteTarget, type AskStopQuestion, type AskTurn } from "../../src/ask/conversationSession";
import { routeLabel } from "../../src/ask/routeLabels";
import { READY_ASK } from "../../src/ask/useAskReadiness";
import { runFromRow, runTitle } from "../../src/apps/nexus/rows";
import { readTheBarNow, useFakeDate } from "../readTheBar";

// ASK ROUTES A MESSAGE TO THE GOALS ALREADY RUNNING (design 2026-10-19, Interface).
// The cluster decides where a message goes; the conversation shows where it
// went, says the reply before a split request's own answer, and asks on a
// goal's card before stopping it.

const sessions: ConversationSession[] = [];
afterEach(() => { cleanup(); sessions.splice(0).forEach(session => session.dispose()); vi.useRealTimers(); });

const startedAt = "2026-10-19T10:00:00Z";
function goal(id: string, title: string, fields: Partial<AskTurn> = {}): AskTurn {
  return { id, runId: `run-${id}`, goalId: `goal-${id}`, prompt: `Request ${id}`, workTitle: title, answer: "I’ll look into it.", acknowledgement: "I’ll look into it.", background: true, state: "queued", workStatus: "running", startedAt, activity: [], ...fields };
}
function target(id: string, title: string, status: AskRouteTarget["status"], fields: Partial<AskRouteTarget> = {}): AskRouteTarget {
  return { goalId: `goal-${id}`, runId: `run-${id}`, title, status, text: "Actually, I’m in Phoenix.", ...fields };
}
function routedTurn(id: string, routed: AskRouted, fields: Partial<AskTurn> = {}): AskTurn {
  return { id, prompt: "Actually, I’m in Phoenix.", answer: routed.reply ?? "", state: "done", startedAt: "2026-10-19T10:01:00Z", endedAt: "2026-10-19T10:01:02Z", activity: [], routed, ...fields };
}
function deferred<T>() { let resolve!: (value: T) => void; let reject!: (reason: Error) => void; const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }

function setup(turns: AskTurn[], { canDecide = true }: { canDecide?: boolean } = {}) {
  let stored = turns;
  const ask = vi.fn<AskTransport["ask"]>(() => ({ cancel: vi.fn() }));
  const decideStop = vi.fn<NonNullable<AskTransport["decideStop"]>>(async () => {});
  const read = vi.fn(async (id: string) => id === "chat" ? stored : []);
  const transport: AskTransport = { ask, conversations: { list: vi.fn(async () => []), create: vi.fn(async () => ({ id: "new", title: "New" })), read }, ...(canDecide ? { decideStop } : {}) };
  const session = new ConversationSession(transport); sessions.push(session);
  return { session, transport, ask, decideStop, read, setStored: (next: AskTurn[]) => { stored = next; } };
}
async function mount(turns: AskTurn[], options: { canDecide?: boolean } = {}) {
  const test = setup(turns, options);
  await test.session.select("chat");
  render(<AskSurface conversation={test.session} transport={test.transport} variant="sheet" availability={READY_ASK} onOpenWork={vi.fn()} />);
  return test;
}
const mine = (prompt: string) => screen.getByText(prompt).closest<HTMLElement>(".os-ask-message")!;

describe("where a message went", () => {
  it("shows one line per goal under the person's message", async () => {
    await mount([goal("weather", "Weather in Phoenix"), routedTurn("phoenix", { reply: "Sent to all four.", targets: [
      target("weather", "Weather in Phoenix", "queued"),
      target("hike", "Hike near Phoenix", "received"),
      target("deploy", "Deploy storefront update", "notApproved", { approvalId: "approval-deploy" }),
      target("report", "Quarterly report", "notDelivered", { approvalId: "approval-report", reason: "review" }),
    ] })]);
    const message = mine("Actually, I’m in Phoenix.");
    expect(within(message).getAllByText(/^To /).map(line => line.textContent)).toEqual([
      "To Weather in Phoenix · Queued",
      "To Hike near Phoenix · Received",
      "To Deploy storefront update · Sent, not approved",
      "To Quarterly report · Not delivered",
    ]);
    expect(within(message).getByText("Guidance")).toBeTruthy();
    expect(within(message).getByRole("button", { name: "Propose lesson" })).toBeTruthy();
  });

  it("offers a lesson only for a message some work read as guidance", async () => {
    await mount([routedTurn("staging", { reply: "Answered its question.", targets: [target("deploy", "Deploy storefront update", "answered", { approvalId: "approval" })] }, { prompt: "Use the staging bucket." })]);
    const message = mine("Use the staging bucket.");
    expect(within(message).getByText("To Deploy storefront update · Answered")).toBeTruthy();
    expect(within(message).queryByRole("button", { name: "Propose lesson" })).toBeNull();
  });

  it("names a goal whose title was not recorded the way its card does", async () => {
    await mount([goal("weather", "", { prompt: "What’s the weather in Tucson?" }), routedTurn("phoenix", { reply: "Switching to Phoenix.", targets: [target("weather", "", "queued")] })]);
    expect(within(mine("Actually, I’m in Phoenix.")).getByText("To What’s the weather in Tucson? · Queued")).toBeTruthy();
  });

  it("names every route status, and an unknown one as queued", () => {
    expect(["queued", "received", "answered", "notApproved", "saved", "notDelivered", "notReceived", "later", "toString"].map(routeLabel)).toEqual([
      "Queued", "Received", "Answered", "Sent, not approved", "Saved", "Not delivered", "Not received", "Queued", "Queued",
    ]);
  });

  it("sends a message about running work as an ordinary Ask message", async () => {
    const test = await mount([goal("weather", "Weather in Tucson")]);
    fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "Actually, I’m in Phoenix." } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    expect(test.ask).toHaveBeenCalledExactlyOnceWith("Actually, I’m in Phoenix.", null, expect.anything(), expect.objectContaining({ conversationId: "chat" }));
  });
});

describe("a split message", () => {
  const split = (fields: Partial<AskTurn> = {}) => goal("dinner", "Dinner near downtown Phoenix", {
    prompt: "Actually I’m in Phoenix. Also find a dinner spot downtown.", answer: "I’ll find a few dinner options.", acknowledgement: "I’ll find a few dinner options.", startedAt: "2026-10-19T10:01:00Z",
    routed: { targets: [target("weather", "Weather in Phoenix", "queued")], reply: "Switching the weather check to Phoenix.", statement: "Find a dinner spot downtown." },
    ...fields,
  });

  it("says the reply before the new request's own answer, in one MemQL message", async () => {
    await mount([goal("weather", "Weather in Phoenix"), split()]);
    const reply = screen.getByText("Switching the weather check to Phoenix.");
    const answer = screen.getByText("I’ll find a few dinner options.");
    expect(reply.closest(".os-ask-message")).toBe(answer.closest(".os-ask-message"));
    expect(reply.compareDocumentPosition(answer) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(answer.closest<HTMLElement>(".os-ask-message")!).getByRole("button", { name: /View work: Dinner near downtown Phoenix/ })).toBeTruthy();
  });

  it("still says the reply when the new request's acknowledgement is empty", async () => {
    await mount([goal("weather", "Weather in Phoenix"), split({ answer: "", acknowledgement: "" })]);
    const reply = screen.getByText("Switching the weather check to Phoenix.");
    expect(within(reply.closest<HTMLElement>(".os-ask-message")!).getByText("MemQL")).toBeTruthy();
  });

  it("says the reply once: not again with the result, nor when it is the answer", async () => {
    await mount([
      goal("weather", "Weather in Phoenix"),
      split({ state: "done", answer: "Here are three dinner spots.", endedAt: "2026-10-19T10:09:00Z" }),
      routedTurn("only", { reply: "Switching to Phoenix.", targets: [target("weather", "Weather in Phoenix", "received")] }, { prompt: "Use Phoenix.", startedAt: "2026-10-19T10:10:00Z" }),
    ]);
    expect(screen.getAllByText("Switching the weather check to Phoenix.")).toHaveLength(1);
    expect(screen.getAllByText("Switching to Phoenix.")).toHaveLength(1);
  });
});

describe("a stop question on the goal's card", () => {
  useFakeDate();
  const question: AskStopQuestion = { goalId: "goal-tucson", runId: "run-tucson", title: "Weather in Tucson", decision: "" };
  const tucson = goal("tucson", "Weather in Tucson");
  const asked = routedTurn("never", { stops: [question], reply: "Stop the Tucson weather check?" }, { prompt: "Never mind the Tucson one." });
  const decided = (decision: AskStopQuestion["decision"]) => ({ ...asked, routed: { ...asked.routed, stops: [{ ...question, decision }] } });
  const ask = "Stop this work? It can't be resumed.";

  it("asks on that goal's card, beside the control that opens it", async () => {
    await mount([tucson, goal("hike", "Hike near Phoenix"), asked]);
    expect(screen.getAllByText(ask)).toHaveLength(1);
    const card = screen.getByText(ask).closest<HTMLElement>(".os-ask-work-card")!;
    const open = within(card).getByRole("button", { name: /Weather in Tucson/ });
    const keep = within(card).getByRole("button", { name: "Keep" });
    const stop = within(card).getByRole("button", { name: "Stop" });
    expect(open.contains(keep) || open.contains(stop)).toBe(false);
    expect(keep.compareDocumentPosition(stop) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(stop.dataset.tone).toBe("danger");
    expect(keep.dataset.tone).toBe("quiet");
    expect(within(card).getByRole("group", { name: "Stop Weather in Tucson" })).toBeTruthy();
  });

  it("takes no press in the moment after it appears, then stops the work", async () => {
    const test = await mount([tucson, asked]);
    fireEvent.click(screen.getByRole("button", { name: "Stop" }));
    expect(test.decideStop).not.toHaveBeenCalled();
    readTheBarNow();
    test.setStored([{ ...tucson, workStatus: "running" }, decided("stopped")]);
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Stop" })); });
    expect(test.decideStop).toHaveBeenCalledExactlyOnceWith({ conversationId: "chat", turnId: "never", runId: "run-tucson", decision: "stop" });
    await waitFor(() => expect(screen.queryByText(ask)).toBeNull());
  });

  it("keeps the work", async () => {
    const test = await mount([tucson, asked]);
    fireEvent.click(screen.getByRole("button", { name: "Keep" }));
    expect(test.decideStop).not.toHaveBeenCalled();
    readTheBarNow();
    test.setStored([tucson, decided("kept")]);
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Keep" })); });
    expect(test.decideStop).toHaveBeenCalledExactlyOnceWith({ conversationId: "chat", turnId: "never", runId: "run-tucson", decision: "keep" });
    await waitFor(() => expect(screen.queryByText(ask)).toBeNull());
    expect(within(screen.getByLabelText("Background work")).getByRole("button", { name: /Weather in Tucson/ })).toBeTruthy();
  });

  it("holds both answers while one is recorded and says a refusal in the error line", async () => {
    const test = await mount([tucson, asked]);
    const pending = deferred<void>();
    test.decideStop.mockReturnValueOnce(pending.promise);
    readTheBarNow();
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Stop" })); });
    expect(screen.getByRole("button", { name: "Stop" })).toHaveProperty("disabled", true);
    expect(screen.getByRole("button", { name: "Keep" })).toHaveProperty("disabled", true);
    await act(async () => { pending.reject(new Error("This work could not be stopped. Try again.")); });
    expect(screen.getByRole("alert").textContent).toBe("This work could not be stopped. Try again.");
    expect(screen.getByRole("button", { name: "Stop" })).toHaveProperty("disabled", false);
    expect(screen.getByText(ask)).toBeTruthy();
  });

  it("does not let a poll that left before the answer was recorded ask again", async () => {
    const test = await mount([tucson, asked]);
    const write = deferred<void>();
    const stale = deferred<AskTurn[]>();
    test.decideStop.mockReturnValueOnce(write.promise);
    readTheBarNow();
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Stop" })); });
    // The refresh poll reads while Stop is being recorded and answers late
    // with the snapshot from before it.
    test.read.mockReturnValueOnce(stale.promise);
    const poll = test.session.reload(false);
    test.setStored([tucson, decided("stopped")]);
    await act(async () => { write.resolve(); });
    await act(async () => { stale.resolve([tucson, asked]); await poll; });
    await waitFor(() => expect(test.session.getSnapshot().stopDecisions).toEqual({}));
    expect(screen.queryByText(ask)).toBeNull();
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Keep" })).toBeNull();
    expect(test.session.getSnapshot().turns).toEqual([tucson, decided("stopped")]);
  });

  it("is not offered by a connection that cannot answer it", async () => {
    await mount([tucson, asked], { canDecide: false });
    expect(screen.queryByText(ask)).toBeNull();
    expect(within(screen.getByLabelText("Background work")).getByRole("button", { name: /Weather in Tucson/ })).toBeTruthy();
  });

  it("gives work that would wait inline a card while a question asks about it", () => {
    const quick = goal("tucson", "Weather in Tucson", { background: false, acknowledgement: undefined, answer: "", workload: "quick" });
    const view = render(<AskWorkQueue turns={[quick]} onDecideStop={vi.fn()} />);
    expect(screen.queryByLabelText("Background work")).toBeNull();
    view.rerender(<AskWorkQueue turns={[quick, asked]} onDecideStop={vi.fn()} />);
    expect(within(screen.getByLabelText("Background work")).getByText(ask)).toBeTruthy();
  });

  it("never folds a card that asks; the others still fold", () => {
    const onOpen = vi.fn();
    const cards = [goal("a", "Work a"), goal("b", "Work b"), goal("c", "Work c"), tucson];
    const view = render(<AskWorkQueue turns={cards} onOpen={onOpen} onDecideStop={vi.fn()} />);
    expect(screen.getAllByRole("button")).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "4 tasks in progress" }));
    expect(onOpen).toHaveBeenCalledExactlyOnceWith();
    view.rerender(<AskWorkQueue turns={[...cards, asked]} onOpen={onOpen} onDecideStop={vi.fn()} />);
    const card = screen.getByText(ask).closest<HTMLElement>(".os-ask-work-card")!;
    fireEvent.click(within(card).getByRole("button", { name: /Weather in Tucson/ }));
    expect(onOpen).toHaveBeenLastCalledWith("run-tucson");
    expect(screen.getByRole("button", { name: "3 tasks in progress" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Work a/ })).toBeNull();
  });

  it("keeps a refusal said through later reads while its question still waits", async () => {
    const test = setup([tucson, asked]);
    await test.session.select("chat");
    test.decideStop.mockRejectedValueOnce(new Error("This work could not be stopped. Try again."));
    expect(await test.session.decideStop("never", "run-tucson", "stop")).toBe(false);
    await test.session.reload(false);
    expect(test.session.getSnapshot().error).toBe("This work could not be stopped. Try again.");
    test.setStored([tucson, decided("ended")]);
    await test.session.reload(false);
    expect(test.session.getSnapshot().error).toBe("");
  });

  it("answers every question about the goal, not only the one pressed", async () => {
    const again = routedTurn("again", { stops: [question], reply: "Stop the Tucson weather check?" }, { prompt: "Really, drop the Tucson one.", startedAt: "2026-10-19T10:02:00Z" });
    const test = setup([tucson, asked, again]);
    await test.session.select("chat");
    // The read after the answer fails: what shows is what this session knows.
    test.read.mockRejectedValueOnce(new Error("Connection lost."));
    expect(await test.session.decideStop("again", "run-tucson", "keep")).toBe(true);
    expect(test.session.getSnapshot().snapshotError).toBe("Connection lost.");
    expect([...pendingStops(test.session.getSnapshot().turns).keys()]).toEqual([]);
    expect(test.session.getSnapshot().turns.flatMap(turn => turn.routed?.stops ?? []).map(stop => stop.decision)).toEqual(["kept", "kept"]);
  });

  it("records a decision, reloads, and keeps a refusal in the error line", async () => {
    const test = setup([tucson, asked]);
    await test.session.select("chat");
    test.decideStop.mockRejectedValueOnce(new Error("this stop question is already decided"));
    expect(await test.session.decideStop("never", "run-tucson", "keep")).toBe(false);
    expect(test.session.getSnapshot().error).toBe("this stop question is already decided");
    const reads = test.read.mock.calls.length;
    test.setStored([tucson, decided("kept")]);
    expect(await test.session.decideStop("never", "run-tucson", "keep")).toBe(true);
    expect(test.decideStop).toHaveBeenLastCalledWith({ conversationId: "chat", turnId: "never", runId: "run-tucson", decision: "keep" });
    expect(test.read).toHaveBeenCalledTimes(reads + 1);
    expect(test.session.getSnapshot()).toMatchObject({ error: "", turns: [tucson, decided("kept")] });
  });
});

it("a routed turn without a run recovers from the snapshot", async () => {
  vi.useFakeTimers();
  let callbacks!: AskCallbacks;
  let stored: AskTurn[] = [];
  const cancel = vi.fn();
  const ask = vi.fn<AskTransport["ask"]>((_prompt, _context, on) => { callbacks = on; return { cancel }; });
  const session = new ConversationSession({ ask, conversations: { create: async () => ({ id: "chat", title: "Chat" }), list: async () => [], read: async () => stored } });
  sessions.push(session);
  session.send("Actually, I’m in Phoenix.", null); await Promise.resolve();
  callbacks.delta("Switching");
  const streaming = session.getSnapshot().turns[0]!;
  expect(streaming.runId).toBeUndefined();
  stored = [{ ...streaming, state: "done", answer: "Switching the weather check to Phoenix.", endedAt: "2026-10-19T10:01:02Z", routed: { reply: "Switching the weather check to Phoenix.", targets: [target("weather", "Weather in Phoenix", "queued")] } }];
  await vi.advanceTimersByTimeAsync(10000);
  expect(session.getSnapshot().busy).toBe(false);
  expect(session.getSnapshot().turns).toEqual(stored);
  expect(cancel).toHaveBeenCalledOnce();
  callbacks.delta("Late text"); callbacks.done();
  expect(session.getSnapshot().turns).toEqual(stored);
  expect(ask).toHaveBeenCalledOnce();
});

it("shows where a routed message went as soon as its reply ends", async () => {
  vi.useFakeTimers();
  let callbacks!: AskCallbacks;
  const tucson = goal("weather", "Weather in Tucson");
  const hike = goal("hike", "Hike near Tucson");
  let stored: AskTurn[] = [tucson, hike];
  const ask = vi.fn<AskTransport["ask"]>((_prompt, _context, on) => { callbacks = on; return { cancel: vi.fn() }; });
  const read = vi.fn(async (id: string) => id === "chat" ? stored : []);
  const session = new ConversationSession({ ask, conversations: { create: async () => ({ id: "chat", title: "Chat" }), list: async () => [], read } });
  sessions.push(session);
  await session.select("chat");
  session.send("Actually, I’m in Phoenix. And drop the hike?", null);
  callbacks.delta("Switching the weather check to Phoenix. Stop Hike near Tucson?");
  const sent = session.getSnapshot().turns.at(-1)!;
  expect(sent.runId).toBeUndefined();
  // The cluster saved where the message went before it said done.
  stored = [tucson, hike, { ...sent, state: "done", endedAt: "2026-10-19T10:01:02Z", routed: {
    targets: [target("weather", "Weather in Phoenix", "queued")],
    stops: [{ goalId: "goal-hike", runId: "run-hike", title: "Hike near Tucson", decision: "" }],
  } }];
  callbacks.done();
  // No poll has come due: the labels and the stop question are there at once.
  await vi.advanceTimersByTimeAsync(0);
  expect(session.getSnapshot().turns).toEqual(stored);
  expect([...pendingStops(session.getSnapshot().turns).keys()]).toEqual(["run-hike"]);
  expect(read).toHaveBeenCalledTimes(2);
});

it("Activity names the routing call", () => {
  const turn: AskTurn = { id: "phoenix", prompt: "Actually, I’m in Phoenix.", answer: "", state: "done", startedAt, activity: [
    { id: "route", kind: "model", phase: "completed", at: startedAt, call: { promptName: "askMessageRouting", inputTokens: 120, outputTokens: 30, tokensEstimated: false, totalCost: 0, pricingConfigured: false, firstTokenMs: 0 } },
  ] };
  render(<AskActivityLog turns={[turn]} onClose={vi.fn()} />);
  expect(screen.getByText("Routing message")).toBeTruthy();
});

it("Nexus names a run by the title a later message gave it", () => {
  expect(runTitle(runFromRow({ id: "r1", title: "Weather in Phoenix", classification: { workTitle: "Weather in Tucson" } }))).toBe("Weather in Phoenix");
  expect(runTitle(runFromRow({ id: "r2", classification: { workTitle: "Weather in Tucson" } }))).toBe("Weather in Tucson");
});
