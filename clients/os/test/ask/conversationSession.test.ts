import { expect, it, vi } from "vitest";
import { ConversationSession, type AskTurn } from "../../src/ask/conversationSession";
import type { AskTransport } from "../../src/ask/askController";
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(r => { resolve = r; }); return { promise, resolve }; }
function setup(overrides: Partial<NonNullable<AskTransport["conversations"]>> = {}) {
 const store = { list: vi.fn(async () => []), create: vi.fn(async () => ({ id: "new", title: "New conversation" })), read: vi.fn(async () => []), ...overrides };
 return new ConversationSession({ ask: vi.fn(() => ({ cancel: vi.fn() })), conversations: store });
}
it("ignores a slow selection after New conversation", async () => {
 const read = deferred<AskTurn[]>(); const session = setup({ read: () => read.promise });
 const pending = session.select("old"); session.newConversation(); read.resolve([]); await pending;
 expect(session.getSnapshot().selectedId).toBeNull(); expect(session.getSnapshot().loading).toBe(false);
});
it("does not revive a stopped voice start after conversation creation", async () => {
 const create = deferred<{id:string;title:string}>(); const session = setup({ create: () => create.promise });
 const pending = session.beginVoice(); session.endVoice(); create.resolve({id:"late",title:"Late"});
 await expect(pending).rejects.toThrow("cancelled"); expect(session.getSnapshot().voiceActive).toBe(false); expect(session.getSnapshot().selectedId).toBeNull();
});
it("buffers ASR evidence before the transcript and ignores duplicate transcripts", async () => {
 const session = setup(); await session.beginVoice();
 const activity = {id:"asr",kind:"model" as const,phase:"completed" as const,at:new Date().toISOString(),model:"whisper",elapsedMs:500};
 session.voiceEvent({type:"activity",turnId:"one",activity}); session.voiceEvent({type:"transcript",turnId:"one",text:"Hello"}); session.voiceEvent({type:"transcript",turnId:"one",text:"Hello"});
 expect(session.getSnapshot().turns).toHaveLength(1); expect(session.getSnapshot().turns[0]?.activity).toEqual([activity]);
 session.voiceEvent({type:"transcript",turnId:"two",text:"Again"}); session.voiceEvent({type:"done",turnId:"one"}); expect(session.getSnapshot().busy).toBe(true);
});
it("keeps the latest conversation list when refreshes finish out of order", async () => {
 const first = deferred<{id:string;title:string}[]>(); let calls=0;
 const session = setup({list: () => ++calls === 1 ? first.promise : Promise.resolve([{id:"new",title:"New"}])});
 const pending = session.refresh(); await session.refresh(); first.resolve([]); await pending; expect(session.getSnapshot().conversations[0]?.id).toBe("new");
});
it("learns dictation estimates only from successful calls on the same route", () => {
 const session = setup(); const base = {kind:"model" as const,at:new Date().toISOString(),provider:"fleet:local",model:"whisper"};
 session.recordDictation({...base,id:"1",phase:"completed",elapsedMs:8200}); session.recordDictation({...base,id:"2",phase:"failed",elapsedMs:99999}); session.recordDictation({...base,id:"3",phase:"running"});
 expect(session.getSnapshot().dictationActivity.at(-1)?.expectedMs).toBe(8200);
 session.recordDictation({...base,id:"4",model:"remote",phase:"running"}); expect(session.getSnapshot().dictationActivity.at(-1)?.expectedMs).toBe(60000);
});
