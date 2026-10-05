import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AskSurface } from "../../src/ask/AskSurface";
import { AskActivityLog } from "../../src/ask/AskActivityLog";
import type { AskCallbacks } from "../../src/ask/askController";
import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG } from "../../src/cluster/config";

afterEach(() => { cleanup(); vi.useRealTimers(); });
it("updates a historical queue receipt when the durable request finishes", () => {
 const turn = {id:"turn",prompt:"Recall a preference",answer:"",state:"queued" as const,runId:"run",startedAt:"2026-10-05T10:00:00Z",activity:[{id:"run",kind:"run" as const,phase:"queued" as const,at:"2026-10-05T10:00:00Z"}]};
 const view=render(<AskActivityLog turns={[turn]} onClose={vi.fn()} />);
 expect(screen.getByText("Working in background")).toBeTruthy();
 view.rerender(<AskActivityLog turns={[{...turn,state:"waiting"}]} onClose={vi.fn()} />);
 expect(screen.getByText("Needs input")).toBeTruthy();
 view.rerender(<AskActivityLog turns={[{...turn,state:"done",answer:"Done"}]} onClose={vi.fn()} />);
 expect(screen.getByText("Completed")).toBeTruthy();
 expect(screen.queryByText("Working in background")).toBeNull();
});
function setup() {
 let callbacks: AskCallbacks;
 const cancel = vi.fn();
 const onClose = vi.fn();
 const ask = vi.fn((_p: string, _c: string | null, on: AskCallbacks) => { callbacks = on; return { cancel }; });
 const session = { config: UNKNOWN_RUNTIME_CONFIG, access: { userId: "user", primaryEmail: "znas@example.test", role: "owner", roleName: "Owner", rank: 400 } };
 render(<SessionProvider value={session}><AskSurface transport={{ ask }} variant="sheet" onClose={onClose} availability={{ state:"ready", message:"", refresh:vi.fn() }} /></SessionProvider>);
 return { ask, cancel, onClose, callbacks:()=>callbacks };
}
function draft(value:string) { fireEvent.change(screen.getByRole("textbox",{name:"Ask"}),{target:{value}}); }
it("separates all header actions and toggles the entire body without a second back button",()=>{
 setup(); draft("unsent message");
 const activity=screen.getByRole("button",{name:"Activity"});
 expect(activity.parentElement).not.toBe(screen.getByRole("button",{name:"New conversation"}).parentElement);
 expect(activity.parentElement).not.toBe(screen.getByRole("button",{name:"Close Ask"}).parentElement);
 expect(activity.parentElement).toBe(screen.getByRole("button", { name: "Conversations" }).parentElement);
 activity.focus(); fireEvent.click(activity);
 expect(screen.getByRole("region",{name:"Conversation activity"})).toBeTruthy();
 expect(screen.queryByRole("log",{name:"Conversation"})).toBeNull();
 expect(screen.queryByRole("textbox",{name:"Ask"})).toBeNull();
 const conversation = screen.getByRole("button", { name: "Conversation" });
 expect(conversation).toBe(activity);
 expect(document.activeElement).toBe(conversation);
 expect(screen.queryByRole("button", { name: /back|close activity/i })).toBeNull();
 expect(screen.queryByRole("button", { name: "Activity" })).toBeNull();
 fireEvent.click(conversation);
 expect(screen.getByRole("button", { name: "Activity" })).toBe(activity);
 expect((screen.getByRole("textbox",{name:"Ask"}) as HTMLTextAreaElement).value).toBe("unsent message");
 expect(document.activeElement).toBe(activity);
});
it("keeps streaming while activity is open, shows terminal evidence, and returns to the same answer",()=>{
 const w=setup();draft("Jose");fireEvent.click(screen.getByRole("button",{name:"Send"}));
 const event={id:"run",kind:"run" as const,phase:"running" as const,at:new Date().toISOString()};
 act(()=>w.callbacks().activity?.(event));
 fireEvent.click(screen.getByRole("button",{name:"Activity"}));
 expect(screen.getByText("Working")).toBeTruthy();
 act(()=>{ w.callbacks().delta("Nice to meet you, Jose."); w.callbacks().activity?.({...event,phase:"completed"});w.callbacks().done(); });
 expect(screen.queryByText("Working")).toBeNull();
 expect(screen.getByText("Completed")).toBeTruthy();
 fireEvent.click(screen.getByRole("button",{name:"Conversation"}));
 expect(screen.getByText("Nice to meet you, Jose.")).toBeTruthy();
 expect(w.cancel).not.toHaveBeenCalled();
 expect(w.ask).toHaveBeenCalledOnce();
});
it("Escape returns from Activity without closing Ask, including when focus is in the header",()=>{
 const w=setup(); const activity=screen.getByRole("button",{name:"Activity"});
 fireEvent.click(activity); fireEvent.keyDown(screen.getByRole("region",{name:"Conversation activity"}),{key:"Escape"});
 expect(screen.queryByRole("region",{name:"Conversation activity"})).toBeNull();
 fireEvent.click(activity); activity.focus(); fireEvent.keyDown(activity,{key:"Escape"});
 expect(screen.queryByRole("region",{name:"Conversation activity"})).toBeNull();
 expect(w.onClose).not.toHaveBeenCalled();
});
it("shows failed run details without an indefinite Working state",()=>{
 const w=setup();draft("Jose");fireEvent.click(screen.getByRole("button",{name:"Send"}));
 act(()=>{w.callbacks().activity?.({id:"run",kind:"run",phase:"failed",at:new Date().toISOString(),error:"Intent classification timed out"});w.callbacks().error("Intent classification timed out");});
 fireEvent.click(screen.getByRole("button",{name:"Activity"}));
 const region=screen.getByRole("region",{name:"Conversation activity"});
 expect(within(region).getByText("Failed")).toBeTruthy();
 expect(within(region).getByText("Intent classification timed out")).toBeTruthy();
 expect(within(region).queryByText("Working")).toBeNull();
 fireEvent.click(screen.getByRole("button",{name:"New conversation"}));
 expect(screen.queryByRole("region",{name:"Conversation activity"})).toBeNull();
 expect(document.activeElement).toBe(screen.getByRole("textbox",{name:"Ask"}));
});

it("uses the account avatar and timestamps a reply only at successful completion", () => {
 vi.useFakeTimers(); vi.setSystemTime(new Date("2026-10-05T14:00:00Z"));
 const w = setup(); draft("Jose"); fireEvent.click(screen.getByRole("button", { name: "Send" }));
 const log = screen.getByRole("log", { name: "Conversation" });
 expect(within(log).getByText("Z")).toBeTruthy();
 expect(within(log).getAllByText("You")).toHaveLength(1);
 expect([...log.querySelectorAll("time")].map(time => time.dateTime)).toEqual(["2026-10-05T14:00:00.000Z"]);
 act(() => { vi.setSystemTime(new Date("2026-10-05T14:01:00Z")); w.callbacks().delta("Nice to meet you, Jose."); });
 expect(log.querySelectorAll("time")).toHaveLength(1);
 act(() => { vi.setSystemTime(new Date("2026-10-05T14:02:00Z")); w.callbacks().done(); });
 expect([...log.querySelectorAll("time")].map(time => time.dateTime)).toEqual(["2026-10-05T14:00:00.000Z", "2026-10-05T14:02:00.000Z"]);
});

it("does not stamp a failed partial reply as a completed response", () => {
 const w = setup(); draft("Jose"); fireEvent.click(screen.getByRole("button", { name: "Send" }));
 act(() => { w.callbacks().delta("Nice to meet"); w.callbacks().error("Connection lost"); });
 expect(screen.getByRole("log", { name: "Conversation" }).querySelectorAll("time")).toHaveLength(1);
});

it("keeps active dictation working until its matching completion arrives", () => {
 const event = { id: "transcribe", kind: "model" as const, phase: "running" as const, at: new Date().toISOString() };
 const view = render(<AskActivityLog turns={[]} dictation={[event]} onClose={vi.fn()} />);
 expect(screen.getByText("Working")).toBeTruthy();
 view.rerender(<AskActivityLog turns={[]} dictation={[event, { ...event, phase: "completed" }]} onClose={vi.fn()} />);
 expect(screen.queryByText("Working")).toBeNull();
 expect(screen.getByText("Completed")).toBeTruthy();
});
