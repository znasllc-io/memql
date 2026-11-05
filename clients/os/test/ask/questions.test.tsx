import { afterEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { AskQuestion } from "../../src/ask/AskQuestion";
import { ConversationSession } from "../../src/ask/conversationSession";
import { AskSurface } from "../../src/ask/AskSurface";

afterEach(cleanup);
const question = { id: "q", text: "Which format would you like?", kind: "choice" as const, options: [{value:"pdf",label:"PDF"},{value:"txt",label:"Text"}] };

it("keeps an unfinished answer when optional work continues and sends it once", async () => {
 const answer=vi.fn(async()=>{});
 const {rerender}=render(<AskQuestion question={{...question,optional:true}} onAnswer={answer}/>);
 fireEvent.change(screen.getByRole("textbox",{name:"Your answer"}),{target:{value:"Use the saved preference"}});
 rerender(<AskQuestion question={{...question,optional:true,continued:true}} onAnswer={answer}/>);
 expect(screen.getByText("Work has continued. Your answer can still guide it.")).toBeTruthy();
 expect((screen.getByRole("textbox",{name:"Your answer"}) as HTMLTextAreaElement).value).toBe("Use the saved preference");
 fireEvent.click(screen.getByRole("button",{name:"Send answer"}));
 await waitFor(()=>expect(answer).toHaveBeenCalledExactlyOnceWith({text:"Use the saved preference"}));
});

it("accepts free text instead of a suggested option and prevents duplicate submission", async () => {
  let finish!: () => void;
  const answer = vi.fn(() => new Promise<void>(resolve => { finish = resolve; }));
  render(<AskQuestion question={question} onAnswer={answer} />);
  fireEvent.click(screen.getByRole("button",{name:"PDF"}));
  fireEvent.change(screen.getByRole("textbox",{name:"Your answer"}),{target:{value:"A spreadsheet, please"}});
  expect(screen.getByRole("button",{name:"PDF"}).getAttribute("aria-pressed")).toBe("false");
  fireEvent.click(screen.getByRole("button",{name:"Send answer"}));
  fireEvent.submit(screen.getByRole("form",{name:"Question from MemQL"}));
  expect(answer).toHaveBeenCalledTimes(1);
  expect(answer).toHaveBeenCalledWith({text:"A spreadsheet, please"});
  finish(); await waitFor(()=>expect(screen.getByRole("button",{name:"Send answer"})).toBeTruthy());
});

it("retains a multiple choice answer after failure and permits retry", async () => {
  const answer = vi.fn().mockRejectedValueOnce(new Error("Connection lost")).mockResolvedValue(undefined);
  render(<AskQuestion question={{...question,kind:"multi"}} onAnswer={answer} />);
  fireEvent.click(screen.getByRole("button",{name:"PDF"})); fireEvent.click(screen.getByRole("button",{name:"Text"}));
  fireEvent.click(screen.getByRole("button",{name:"Send answer"}));
  await screen.findByRole("alert");
  expect(screen.getByRole("button",{name:"PDF"}).getAttribute("aria-pressed")).toBe("true");
  fireEvent.click(screen.getByRole("button",{name:"Send answer"}));
  await waitFor(()=>expect(answer).toHaveBeenCalledTimes(2));
  expect(answer).toHaveBeenLastCalledWith({values:["pdf","txt"]});
});

it("opens the server-selected work conversation without submitting another request", async () => {
  const ask = vi.fn(); const openWork = vi.fn(async()=>({id:"original",title:"Work"}));
  const answerQuestion = vi.fn(async()=>{});
  const session = new ConversationSession({ask,openWork,answerQuestion,conversations:{read:async()=>[],list:async()=>[],create:async()=>({id:"unused",title:"Unused"})}});
  await session.openWork("r"); expect(session.getSnapshot().selectedId).toBe("original");
  await session.answerQuestion(question,{text:"PDF"});
  expect(answerQuestion).toHaveBeenCalledWith("q",{text:"PDF"}); expect(ask).not.toHaveBeenCalled(); session.dispose();
});

it.each([null, undefined])("renders a persisted free-text question without options (%s)", async (options) => {
  const answer = vi.fn(async () => {});
  const restored = JSON.parse(JSON.stringify({...question, kind:"text", options}));
  render(<AskQuestion question={restored} onAnswer={answer} />);
  expect(screen.queryByRole("group",{name:"Suggested answers"})).toBeNull();
  fireEvent.change(screen.getByRole("textbox",{name:"Your answer"}),{target:{value:"Example supplier"}});
  fireEvent.click(screen.getByRole("button",{name:"Send answer"}));
  await waitFor(()=>expect(answer).toHaveBeenCalledWith({text:"Example supplier"}));
});

it("focuses an older question when Nexus opens its work, including from Activity", async () => {
  const turns = [{id:"old",runId:"r",prompt:"Prepare a report",answer:"",state:"waiting" as const,startedAt:"2026-10-05T12:00:00Z",activity:[],question},
    {id:"newer",prompt:"Hello",answer:"Hello",state:"done" as const,startedAt:"2026-10-05T12:10:00Z",activity:[]}];
  const transport = {ask:vi.fn(),openWork:async()=>({id:"original",title:"Work"}),conversations:{read:async()=>turns,list:async()=>[],create:async()=>({id:"new",title:"New"})}};
  const session = new ConversationSession(transport);
  render(<AskSurface transport={transport} conversation={session} variant="sheet" autoFocus availability={{state:"ready",message:"",refresh:vi.fn()}} />);
  await act(async () => { await session.select("original"); });
  fireEvent.click(screen.getAllByRole("button", { name: "Message actions" })[0]!);
  fireEvent.click(screen.getByRole("menuitem", { name: "View activity" }));
  await act(async()=>{await session.openWork("r");});
  expect(document.activeElement).toBe(screen.getByRole("textbox",{name:"Your answer"}));
  expect(session.getSnapshot().focusQuestionId).toBeNull();
  // A normal refresh must not steal the user's focus back to the question.
  screen.getByRole("textbox",{name:"Ask"}).focus();
  await act(async()=>{await session.reload();});
  expect(document.activeElement).toBe(screen.getByRole("textbox",{name:"Ask"}));
  session.dispose();
});

it("does not reopen work after the person starts a new conversation during its lookup", async () => {
  let resolve!: (value:{id:string;title:string})=>void;
  const read = vi.fn(async()=>[]);
  const session = new ConversationSession({ask:vi.fn(),openWork:()=>new Promise(r=>{resolve=r;}),conversations:{read,list:async()=>[],create:async()=>({id:"new",title:"New"})}});
  const opening = session.openWork("r"); session.newConversation(); resolve({id:"old",title:"Old"}); await opening;
  expect(session.getSnapshot().selectedId).toBeNull(); expect(read).not.toHaveBeenCalled(); session.dispose();
});

const wire = (fill: string) => `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10" fill="${fill}"/></svg>`;
const layouts = { id: "lq", text: "Which layout?", kind: "choice" as const, presentation: "layouts" as const, options: [
  { value: "builtin:article", label: "Article", wireframe: wire("#aaa") },
  { value: "builtin:steps", label: "Steps", wireframe: wire("#bbb") },
  { value: "builtin:blank", label: "Plain" },
] };
const dataAddress = (svg: string) => `data:image/svg+xml;base64,${btoa(svg)}`;

it("layout choices draw their wireframes", async () => {
  const answer = vi.fn(async () => {});
  const { container } = render(<AskQuestion question={layouts} onAnswer={answer} />);
  const cards = screen.getAllByRole("button", { name: /Article|Steps|Plain/ });
  expect(cards).toHaveLength(3);
  const images = container.querySelectorAll("img");
  expect(images).toHaveLength(2);
  expect(images[0]!.getAttribute("src")).toBe(dataAddress(layouts.options[0]!.wireframe!));
  expect(images[0]!.getAttribute("alt")).toBe("");
  expect(cards[0]!.querySelector("img")).toBe(images[0]!);
  // A layout with no drawing keeps the card's shape: the mat, empty.
  expect(cards[2]!.querySelector(".os-ask-layout-mat")).toBeTruthy();
  expect(cards[2]!.querySelector("img")).toBeNull();
  expect(screen.getByRole("textbox", { name: "Your answer" })).toBeTruthy();
  fireEvent.click(cards[1]!);
  expect(cards[1]!.getAttribute("aria-pressed")).toBe("true");
  fireEvent.click(screen.getByRole("button", { name: "Send answer" }));
  await waitFor(() => expect(answer).toHaveBeenCalledExactlyOnceWith({ value: "builtin:steps" }));
});

it("a plain choice is unchanged", () => {
  const { container } = render(<AskQuestion question={question} onAnswer={vi.fn(async () => {})} />);
  expect(container.querySelectorAll("img")).toHaveLength(0);
  expect(container.querySelector(".os-ask-layout-mat")).toBeNull();
  expect(container.querySelector(".os-ask-question-options")?.className).toBe("os-ask-question-options");
});

it("never draws a wireframe on a question that is not a layouts choice", () => {
  const stray = { ...question, options: [{ value: "a", label: "A", wireframe: wire("#ccc") }] };
  const { container } = render(<AskQuestion question={stray} onAnswer={vi.fn(async () => {})} />);
  expect(container.querySelectorAll("img")).toHaveLength(0);
  expect(container.innerHTML).not.toContain("<rect");
  expect(container.innerHTML).not.toContain("base64");
});

it("keeps a hostile wireframe inside its data address", () => {
  const hostile = `<svg xmlns="http://www.w3.org/2000/svg"><script>window.pwned=1</script><image href="https://evil.test/x"/></svg>"><img src=x onerror=alert(1)>☃`;
  const { container } = render(<AskQuestion question={{ ...layouts, options: [{ value: "builtin:x", label: "X", wireframe: hostile }] }} onAnswer={vi.fn(async () => {})} />);
  expect(container.querySelectorAll("img")).toHaveLength(1);
  expect(container.querySelector("script")).toBeNull();
  expect(container.querySelector("[onerror]")).toBeNull();
  const src = container.querySelector("img")!.getAttribute("src")!;
  expect(src.startsWith("data:image/svg+xml;base64,")).toBe(true);
  expect(decodeURIComponent(escape(atob(src.split(",")[1]!)))).toBe(hostile);
  expect((window as unknown as { pwned?: number }).pwned).toBeUndefined();
});
