import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AskSurface } from "../../src/ask/AskSurface";
import type { AskCallbacks } from "../../src/ask/askController";

afterEach(cleanup);
function setup() {
 let callbacks: AskCallbacks;
 const cancel = vi.fn();
 const onClose = vi.fn();
 const ask = vi.fn((_p: string, _c: string | null, on: AskCallbacks) => { callbacks = on; return { cancel }; });
 render(<AskSurface transport={{ ask }} variant="sheet" onClose={onClose} availability={{ state:"ready", message:"", refresh:vi.fn() }} />);
 return { ask, cancel, onClose, callbacks:()=>callbacks };
}
function draft(value:string) { fireEvent.change(screen.getByRole("textbox",{name:"Ask"}),{target:{value}}); }
it("groups New and Activity before Close, removes history, and replaces the entire body",()=>{
 setup(); draft("unsent message");
 const activity=screen.getByRole("button",{name:"Activity"});
 expect(activity.parentElement).toBe(screen.getByRole("button",{name:"New conversation"}).parentElement);
 expect(activity.parentElement).not.toBe(screen.getByRole("button",{name:"Close Ask"}).parentElement);
 expect(screen.queryByRole("button",{name:/history|conversations/i})).toBeNull();
 fireEvent.click(activity);
 expect(screen.getByRole("region",{name:"Conversation activity"})).toBeTruthy();
 expect(screen.queryByRole("log",{name:"Conversation"})).toBeNull();
 expect(screen.queryByRole("textbox",{name:"Ask"})).toBeNull();
 expect(document.activeElement).toBe(screen.getByRole("button",{name:"Close activity"}));
 fireEvent.click(screen.getByRole("button",{name:"Close activity"}));
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
 fireEvent.click(screen.getByRole("button",{name:"Close activity"}));
 expect(screen.getByText("Nice to meet you, Jose.")).toBeTruthy();
 expect(w.cancel).not.toHaveBeenCalled();
 expect(w.ask).toHaveBeenCalledOnce();
});
it("Escape returns from Activity without closing Ask, including when focus is in the header",()=>{
 const w=setup(); const activity=screen.getByRole("button",{name:"Activity"});
 fireEvent.click(activity); fireEvent.keyDown(screen.getByRole("button",{name:"Close activity"}),{key:"Escape"});
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
