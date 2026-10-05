import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { AskQuestion } from "../../src/ask/AskQuestion";
import { ConversationSession } from "../../src/ask/conversationSession";

afterEach(cleanup);
const question = { id: "q", text: "Which format would you like?", kind: "choice" as const, options: [{value:"pdf",label:"PDF"},{value:"txt",label:"Text"}] };

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
