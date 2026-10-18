import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { AskQuestion } from "../../src/ask/AskQuestion";
afterEach(cleanup);
const question={id:"progress",text:"How should I continue?",kind:"choice" as const,options:[{value:"retry",label:"Keep working"},{value:"abandon",label:"Stop here"}]};
it.each(["choice","guidance"])("accepts %s without asking for resource amounts",async(mode)=>{
 const onAnswer=vi.fn(async()=>{});render(<AskQuestion question={question} onAnswer={onAnswer}/>);
 if(mode==="choice")fireEvent.click(screen.getByRole("button",{name:"Keep working"}));
 else fireEvent.change(screen.getByRole("textbox",{name:"Your answer"}),{target:{value:"Use the saved results and focus on the summary."}});
 fireEvent.click(screen.getByRole("button",{name:"Send answer"}));
 await waitFor(()=>expect(onAnswer).toHaveBeenCalledExactlyOnceWith(mode==="choice"?{value:"retry"}:{text:"Use the saved results and focus on the summary."}));
 expect(screen.queryByText(/New total|Raise limit/)).toBeNull();
});
it("offers only stopping at a firm limit, while preserving the saved-work explanation",async()=>{
 const onAnswer=vi.fn(async()=>{});render(<AskQuestion question={{...question,choiceOnly:true,text:"The results are saved for a narrower request.",options:[{value:"abandon",label:"Stop here"}]}} onAnswer={onAnswer}/>);
 expect(screen.queryByRole("textbox")).toBeNull();expect(screen.queryByRole("button",{name:"Keep working"})).toBeNull();
 fireEvent.click(screen.getByRole("button",{name:"Stop here"}));fireEvent.click(screen.getByRole("button",{name:"Send answer"}));
 await waitFor(()=>expect(onAnswer).toHaveBeenCalledWith({value:"abandon"}));
});
it("retries only the already saved answer after an interrupted continuation",async()=>{
 const onAnswer=vi.fn().mockRejectedValueOnce(new Error("Reconnect")).mockResolvedValueOnce(undefined);
 render(<AskQuestion question={{...question,choiceOnly:true,resumeAnswer:{feedback:"Use the completed lookup."},options:[]}} onAnswer={onAnswer}/>);
 fireEvent.click(screen.getByRole("button",{name:"Continue saved work"}));expect(await screen.findByRole("alert")).toHaveProperty("textContent","Reconnect");
 expect(screen.queryByRole("textbox")).toBeNull();fireEvent.click(screen.getByRole("button",{name:"Continue saved work"}));
 await waitFor(()=>expect(onAnswer).toHaveBeenCalledTimes(2));expect(onAnswer.mock.calls).toEqual([[{feedback:"Use the completed lookup."}],[{feedback:"Use the completed lookup."}]]);
});

it("recovers a saved stop without offering continuation", async () => {
 const onAnswer=vi.fn(async()=>{});
 render(<AskQuestion question={{...question,choiceOnly:true,resumeAnswer:{value:"abandon"},options:[]}} onAnswer={onAnswer}/>);
 expect(screen.queryByRole("button",{name:"Continue saved work"})).toBeNull();
 fireEvent.click(screen.getByRole("button",{name:"Stop saved work"}));
 await waitFor(()=>expect(onAnswer).toHaveBeenCalledExactlyOnceWith({value:"abandon"}));
});
