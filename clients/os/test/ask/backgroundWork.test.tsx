import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { AskWorkLink } from "../../src/ask/AskWorkLink";
import { AskWorkQueue } from "../../src/ask/AskWorkQueue";
import { conversationFeed } from "../../src/ask/conversationFeed";
import { ConversationSession, type AskTurn } from "../../src/ask/conversationSession";
import type { AskCallbacks, AskTransport } from "../../src/ask/askController";

afterEach(() => { cleanup(); vi.useRealTimers(); });
const turn = (id: string, fields: Partial<AskTurn> = {}): AskTurn => ({ id, runId: `run-${id}`, prompt: `Request ${id}`, workTitle: `Work ${id}`, answer: "Accepted", state: "queued", activity: [], startedAt: "2026-10-05T10:00:00Z", ...fields });
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
 const quick = turn("b", { state:"done", startedAt:"2026-10-05T10:01:00Z", endedAt:"2026-10-05T10:01:02Z", answer:"Quick answer" });
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

it("keeps a link and orders the result when classification queued without prose", () => {
 const task = turn("a", {background:true, acknowledgement:"", answer:"Research result", state:"done", endedAt:"2026-10-05T10:03:00Z"});
 const feed = conversationFeed([task]);
 expect(feed).toHaveLength(2); expect(feed[0]?.turn.answer).toBe("");
 expect(feed[0]?.workState).toBe("done"); expect(feed[1]?.turn.answer).toBe("Research result");
});

it("opens the acknowledgment receipt from its whole target and reflects real state", () => {
 const onOpen = vi.fn(); const view = render(<AskWorkLink title="Find prior report preferences" state="queued" onOpen={onOpen} />);
 fireEvent.click(screen.getByRole("button", {name:/Queued work: Find prior report preferences/})); expect(onOpen).toHaveBeenCalledOnce();
 view.rerender(<AskWorkLink title="Find prior report preferences" state="waiting" onOpen={onOpen} />);
 expect(screen.getByText("Needs your input")).toBeTruthy();
 view.rerender(<AskWorkLink title="Find prior report preferences" state="done" onOpen={onOpen} />);
 expect(screen.getByText("View work")).toBeTruthy(); expect(screen.queryByText("Queued work")).toBeNull();
});
