import { afterEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { AskSurface } from "../../src/ask/AskSurface";
import { AskWorkLink } from "../../src/ask/AskWorkLink";
import { AskWorkQueue } from "../../src/ask/AskWorkQueue";
import { conversationFeed } from "../../src/ask/conversationFeed";
import { ConversationSession, type AskTurn } from "../../src/ask/conversationSession";
import type { AskCallbacks, AskTransport } from "../../src/ask/askController";

afterEach(() => { cleanup(); vi.useRealTimers(); });
const turn = (id: string, fields: Partial<AskTurn> = {}): AskTurn => ({ id, runId: `run-${id}`, prompt: `Request ${id}`, workTitle: `Work ${id}`, answer: "Accepted", background: true, state: "queued", activity: [], startedAt: "2026-10-05T10:00:00Z", ...fields });
it("shows one to three work cards; four collapses to a scoped list link", () => {
 const onOpen = vi.fn(); const turns = [turn("a"), turn("b", {state:"waiting"}), turn("c")];
 const view = render(<AskWorkQueue turns={turns} onOpen={onOpen} />);
 expect(screen.getAllByRole("button")).toHaveLength(3);
 fireEvent.click(screen.getByRole("button", {name:/Work b/})); expect(onOpen).toHaveBeenLastCalledWith("run-b");
 expect(screen.getByText("Needs your input")).toBeTruthy();
 view.rerender(<AskWorkQueue turns={[...turns, turn("d")]} onOpen={onOpen} />);
 expect(screen.getAllByRole("button")).toHaveLength(1);
 fireEvent.click(screen.getByRole("button", {name:"4 tasks in progress"})); expect(onOpen).toHaveBeenLastCalledWith();
 view.rerender(<AskWorkQueue turns={[turn("a", {state:"done"})]} onOpen={onOpen} />);
 expect(screen.queryByLabelText("Background work")).toBeNull();
});
it("places a late result after intervening replies and retains its original request", () => {
 const task = turn("a", { acknowledgement:"I’m working on this.", answer:"Research result", state:"done", endedAt:"2026-10-05T10:03:00Z" });
 const quick = turn("b", { background: false, state:"done", startedAt:"2026-10-05T10:01:00Z", endedAt:"2026-10-05T10:01:02Z", answer:"Quick answer" });
 const feed = conversationFeed([task,quick]);
 expect(feed.map(item => item.key)).toEqual(["a","b","a:result"]);
 expect(feed[0]?.turn.answer).toBe(task.acknowledgement);
 expect(feed[2]?.completion).toBe(true); expect(feed[2]?.turn.prompt).toBe(task.prompt);
});
it("releases the composer on queue acceptance and merges a completion during the next stream", async () => {
 vi.useFakeTimers(); let callbacks!: AskCallbacks;
 let stored: AskTurn[] = [];
 const ask = vi.fn<AskTransport["ask"]>((_prompt,_context,on) => {callbacks = on;return {cancel:vi.fn()};});
 const session = new ConversationSession({ ask, conversations: {create:async()=>({id:"chat",title:"Chat"}),list:async()=>[],read:async()=>stored} });
 expect(session.send("Research",null)).toBe(true); await Promise.resolve();
 callbacks.activity?.({id:"r",kind:"run",phase:"queued",at:"now",arguments:{runId:"r",goalId:"g",workTitle:"Research",workload:"research"}});
 callbacks.delta("Accepted"); callbacks.done();
 const first = session.getSnapshot().turns[0]!;
 expect(session.getSnapshot().busy).toBe(false); expect(first.endedAt).toBeUndefined();
 expect(session.send("Hello",null)).toBe(true); callbacks.delta("Hi");
 stored = [{...first,state:"done",answer:"Research results",endedAt:new Date().toISOString()}];
 await session.reload(false);
 expect(session.getSnapshot().turns.map(t=>t.answer)).toEqual(["Research results","Hi"]);
 expect(session.getSnapshot().busy).toBe(true);
 expect(ask.mock.calls[1]?.[3]?.routing).toEqual({source:"",level:""});
 callbacks.done(); session.dispose(); expect(vi.getTimerCount()).toBe(0);
});
it("a stale poll cannot replace another conversation", async () => {
 let finish!: (turns: AskTurn[]) => void; let calls = 0;
 const session = new ConversationSession({ask:()=>({cancel(){}}),conversations:{create:async()=>({id:"x",title:"x"}),list:async()=>[],read:async()=> ++calls === 2 ? new Promise(r=>{finish=r;}) : []}});
 await session.select("first"); const pending = session.reload(false);
 await session.select("second"); finish([turn("stale")]); await pending;
 expect(session.getSnapshot().selectedId).toBe("second"); expect(session.getSnapshot().turns).toEqual([]); session.dispose();
});

it.each(["done", "error"] as const)("recovers a durable %s receipt when the foreground stream never closes", async state => {
 vi.useFakeTimers(); let callbacks!: AskCallbacks;
 let stored: AskTurn[] = [];
 const cancel = vi.fn(), cancelGoal = vi.fn();
 const read = vi.fn(async () => stored);
 const ask = vi.fn<AskTransport["ask"]>((_p, _c, on) => { callbacks = on; return { cancel }; });
 const session = new ConversationSession({ ask, cancelGoal, conversations: { create: async () => ({ id:"chat",title:"Chat" }), list: async () => [], read } });
 session.send("Create the file", null); await Promise.resolve();
 callbacks.activity?.({ id:"r",kind:"run",phase:"running",at:"now",arguments:{runId:"r",goalId:"g"} });
 callbacks.delta("Working");
 const current = session.getSnapshot().turns[0]!;
 stored = [{ ...current, runId:"other-run", state, answer:"Unrelated" }];
 await vi.advanceTimersByTimeAsync(10000);
 expect(session.getSnapshot().busy).toBe(true);
 expect(session.getSnapshot().turns[0]?.answer).toBe("Working");
 stored = [{ ...current, state, answer: state === "done" ? "File saved" : "", error: state === "error" ? "Could not save" : undefined }];
 await vi.advanceTimersByTimeAsync(10000);
 expect(read).toHaveBeenCalledTimes(2);
 expect(session.getSnapshot().busy).toBe(false);
 expect(session.getSnapshot().turns[0]).toEqual(stored[0]);
 expect(cancel).toHaveBeenCalledOnce(); expect(cancelGoal).not.toHaveBeenCalled();
 callbacks.delta("Late text"); callbacks.error("Late error"); callbacks.done();
 expect(session.getSnapshot().turns[0]).toEqual(stored[0]);
 expect(ask).toHaveBeenCalledOnce(); expect(vi.getTimerCount()).toBe(0);
 session.dispose();
});

it("resumes observation after a Strict Mode effect cleanup and a lost stream", async () => {
 vi.useFakeTimers(); let callbacks!: AskCallbacks;
 const read=vi.fn(async()=>[turn("pending")]);
 const session=new ConversationSession({ask:(_p,_c,on)=>{callbacks=on;return {cancel(){}};},conversations:{list:async()=>[],create:async()=>({id:"chat",title:"Chat"}),read}});
 session.activate(); session.dispose(); session.activate();
 await session.select("chat");
 expect(session.send("Another request",null)).toBe(true);
 callbacks.activity?.({id:"r",kind:"run",phase:"running",at:"now",arguments:{runId:"run-new"}});
 callbacks.error("Disconnected");
 expect(session.getSnapshot().turns[1]?.state).toBe("interrupted");
 await vi.advanceTimersByTimeAsync(2600);
 expect(read.mock.calls.length).toBeGreaterThan(1);
 session.dispose(); expect(vi.getTimerCount()).toBe(0);
});

it("recovers by the accepted request ID when the initial run event was lost", async () => {
 vi.useFakeTimers(); let callbacks!: AskCallbacks;
 let stored: AskTurn[] = [];
 const cancel = vi.fn(), cancelGoal = vi.fn();
 const ask = vi.fn<AskTransport["ask"]>((_p, _c, on) => { callbacks = on; return { cancel }; });
 const session = new ConversationSession({ ask, cancelGoal, conversations: { create: async () => ({id:"chat",title:"Chat"}), list: async () => [], read: async () => stored } });
 session.send("Read the saved file", null); await Promise.resolve();
 const current = session.getSnapshot().turns[0]!;
 expect(current.runId).toBeUndefined();
 stored = [{...current, id:"unrelated-request", runId:"other-run", state:"done", answer:"Unrelated"}];
 await vi.advanceTimersByTimeAsync(10000);
 expect(session.getSnapshot().busy).toBe(true);
 expect(session.getSnapshot().turns.find(t => t.id === current.id)?.state).toBe("streaming");
 stored = [{...current, runId:"accepted-run", state:"done", answer:"California Condor"}];
 await vi.advanceTimersByTimeAsync(10000);
 expect(session.getSnapshot().busy).toBe(false);
 expect(session.getSnapshot().turns).toEqual(stored);
 callbacks.delta("Late text"); callbacks.done();
 expect(session.getSnapshot().turns).toEqual(stored);
 expect(cancel).toHaveBeenCalledOnce(); expect(cancelGoal).not.toHaveBeenCalled();
 expect(ask).toHaveBeenCalledOnce(); expect(vi.getTimerCount()).toBe(0);
 session.dispose();
});

it("keeps a link and orders the result when classification queued without prose", () => {
 const task = turn("a", {background:true, acknowledgement:"", answer:"Research result", state:"done", endedAt:"2026-10-05T10:03:00Z"});
 const feed = conversationFeed([task]);
 expect(feed).toHaveLength(2); expect(feed[0]?.showAssistant).toBe(false); expect(feed[1]?.showAssistant).toBe(true);
 expect(feed[0]?.workState).toBe("done"); expect(feed[1]?.turn.answer).toBe("Research result");
});

it("opens the acknowledgment receipt from its whole target and reflects real state", () => {
 const onOpen = vi.fn(); const view = render(<AskWorkLink title="Find prior report preferences" state="queued" onOpen={onOpen} />);
 fireEvent.click(screen.getByRole("button", {name:/View work: Find prior report preferences/})); expect(onOpen).toHaveBeenCalledOnce();
 view.rerender(<AskWorkLink title="Find prior report preferences" state="waiting" onOpen={onOpen} />);
 expect(screen.getByRole("button", {name:/Needs your input:/}).title).toBe("Needs your input");
 view.rerender(<AskWorkLink title="Find prior report preferences" state="done" onOpen={onOpen} />);
 const receipt = screen.getByRole("button", {name:/View work:/});
 expect(receipt.title).toBe("View work"); expect(receipt.textContent).toBe("");
 expect(screen.queryByText("Queued work")).toBeNull();
});

it("shows the durable run phase rather than calling every background request working", () => {
 const view = render(<AskWorkQueue turns={[turn("a", {workStatus:"compiling"})]} />);
 expect(screen.getByLabelText("Preparing")).toBeTruthy();
 view.rerender(<AskWorkQueue turns={[turn("a", {workStatus:"running"})]} />);
 expect(screen.getByLabelText("Working")).toBeTruthy(); expect(screen.queryByLabelText("Queued")).toBeNull();
 view.rerender(<AskWorkQueue turns={[turn("a", {workStatus:"waiting"})]} />);
 expect(screen.getByLabelText("Recovering")).toBeTruthy();
});

it("accepts a running background receipt without losing run identity on later events", async () => {
 let callbacks!: AskCallbacks;
 const session = new ConversationSession({ask:(_p,_c,on)=>{callbacks=on;return {cancel(){}};}});
 session.send("Check current records", null);
 callbacks.activity?.({id:"r",kind:"run",phase:"running",at:"2026-10-05T10:00:00Z",arguments:{runId:"r",goalId:"g",background:true,workStatus:"running",acknowledgedAt:"2026-10-05T10:00:01Z"}});
 callbacks.activity?.({id:"r",kind:"run",phase:"running",at:"2026-10-05T10:00:00Z"});
 callbacks.delta("I’ll check those records."); callbacks.done();
 expect(session.getSnapshot().busy).toBe(false);
 expect(session.getSnapshot().turns[0]).toMatchObject({runId:"r",goalId:"g",background:true,workStatus:"running",acknowledgedAt:"2026-10-05T10:00:01Z",acknowledgement:"I’ll check those records.",state:"queued"});
 session.dispose();
});

it.each([undefined, "quick"])("keeps a recovered %s conversation inline without a work card", async workload => {
 const stored = turn("greeting", { background: false, answer: "", workload, workStatus: "compiling" });
 const transport = { ask: vi.fn(() => ({ cancel() {} })), conversations: { list: async () => [], create: async () => ({id:"chat",title:"Chat"}), read: async () => [stored] } };
 const session = new ConversationSession(transport); await session.select("chat");
 render(<AskSurface conversation={session} transport={transport} variant="sheet" availability={{state:"ready",message:"",refresh(){}}} onOpenWork={vi.fn()} />);
 expect(screen.getByLabelText("Waiting for your model")).toBeTruthy();
 expect(screen.queryByLabelText("Background work")).toBeNull();
 expect(screen.queryByRole("button", {name:/View work/})).toBeNull();
 expect(screen.queryByRole("alert")).toBeNull();
 session.dispose();
});
it("places the work shortcut after acknowledgement text and keeps only navigation on the card", async () => {
 const stored = turn("task", { workload: "quick", acknowledgement: "I’ll compare the **reports**.", answer: "I’ll compare the **reports**." });
 const transport = { ask: vi.fn(() => ({cancel(){}})), conversations: { list: async()=>[], create:async()=>({id:"chat",title:"Chat"}), read:async()=>[stored] } };
 const session = new ConversationSession(transport); await session.select("chat"); const onOpen = vi.fn();
 const view = render(<AskSurface conversation={session} transport={transport} variant="sheet" availability={{state:"ready",message:"",refresh(){}}} onOpenWork={onOpen} />);
 const shortcut = screen.getByRole("button", {name:/View work: Work task/});
 expect(shortcut.parentElement?.classList.contains("os-ask-answer-with-link")).toBe(true);
 expect(shortcut.previousElementSibling?.textContent).toBe("I’ll compare the reports.");
 fireEvent.click(shortcut); expect(onOpen).toHaveBeenLastCalledWith("run-task", "chat");
 expect(view.container.querySelectorAll(".os-ask-work-queue button")).toHaveLength(1);
 session.dispose();
});

it("does not repeat an acknowledgement as the result of failed work", () => {
 const feed = conversationFeed([turn("failed", { acknowledgement: "I’ll check the report.", answer: "I’ll check the report.", state: "error", error: "Provider unavailable", endedAt: "2026-10-05T10:03:00Z" })]);
 expect(feed[0]?.turn.answer).toBe("I’ll check the report.");
 expect(feed[1]?.turn.answer).toBe("");
 expect(feed[1]?.turn.error).toBe("Provider unavailable");
 expect(feed[1]?.showAssistant).toBe(true);
});

it.each(["waiting", "queued"] as const)("recovers a durable %s handoff after mobile loses the stream event", async state => {
 vi.useFakeTimers(); let callbacks!: AskCallbacks;
 let stored: AskTurn[] = [];
 const cancel = vi.fn(), cancelGoal = vi.fn(), onOpenWork = vi.fn();
 const ask = vi.fn<AskTransport["ask"]>((_p, _c, on) => { callbacks = on; return { cancel }; });
 const transport = { ask, cancelGoal, conversations: { create: async () => ({id:"chat",title:"Chat"}), list: async () => [], read: async () => stored } };
 const session = new ConversationSession(transport);
 session.send("Create the dog image", null); await Promise.resolve();
 const original = session.getSnapshot().turns[0]!;
 render(<AskSurface conversation={session} transport={transport} variant="sheet" availability={{state:"ready",message:"",refresh(){}}} onOpenWork={onOpenWork} />);
 expect(screen.getByLabelText("Waiting for your model")).toBeTruthy();
 stored = [{...original, runId:"accepted-run", goalId:"goal", state, background:true, workTitle:"Create the dog image", workStatus:state === "waiting" ? "waiting" : "running", answer:"I’ll create the image.", acknowledgement:"I’ll create the image."}];
 await act(async () => { await vi.advanceTimersByTimeAsync(10000); });
 expect(session.getSnapshot().busy).toBe(false);
 expect(session.getSnapshot().turns).toEqual(stored);
 expect(cancel).toHaveBeenCalledOnce(); expect(cancelGoal).not.toHaveBeenCalled();
 if (state === "waiting") {
   expect(screen.getByText("Needs your input")).toBeTruthy();
   expect(document.querySelector(".os-ask-wait")).toBeNull();
   fireEvent.click(screen.getByRole("button", {name:"Create the dog image Needs your input"}));
   expect(onOpenWork).toHaveBeenCalledWith("accepted-run", "chat");
 }
 act(() => { callbacks.delta("Late text"); callbacks.error("Late error"); callbacks.done(); });
 expect(session.getSnapshot().turns).toEqual(stored);
 await act(async () => { await vi.advanceTimersByTimeAsync(20000); });
 expect(ask).toHaveBeenCalledOnce();
 if (state === "waiting") expect(document.querySelector(".os-ask-wait")).toBeNull();
 session.dispose();
});
