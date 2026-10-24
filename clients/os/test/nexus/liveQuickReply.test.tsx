import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { flatten } from "../../src/kit/rows";
import capture from "./fixtures/liveQuickReply.json";
import { fakeConnection, withSession } from "./harness";
import { modelUsageFromRows, summarizeUsage, usageLabel } from "../../src/apps/nexus/modelUsage";
import { stepFromRow } from "../../src/apps/nexus/rows";
import { stepOutcome, stepTitle } from "../../src/nexus/presentation";
const h=vi.hoisted(()=>({connection:null as unknown}));
vi.mock("../../src/live/connection",()=>({useOsConnection:()=>h.connection, bridgePathFor:(base:string)=>base+"_memql/ws",osBridgePath:"/_memql/ws"}));
const {NexusApp}=await import("../../src/apps/nexus/NexusApp");
const answer="A software release is a specific version of your application that has been tested and officially made available to users.";

// Captured from a real local Ask run on 2026-10-22. Only identities/profile
// names are anonymized; result.value and node-envelope shapes are preserved.
describe("the actual quick-answer journal",()=>{
  it("reads recorded values and presents the action, including the reply, without inventing evidence review",()=>{
    const steps=capture.steps.map(stepFromRow);
    expect(steps.map(stepTitle)).toEqual(["Read the request details","Check your profile context","Write the answer","Prepare the final answer"]);
    expect(stepOutcome(steps[0]!)).toBe("No additional request details were supplied.");
    expect(stepOutcome(steps[1]!)).toBe("Your available profile and account context was checked.");
    expect(stepOutcome(steps[2]!)).toBe(answer);expect(stepOutcome(steps[3]!)).toBe(answer);
    const envelopeOnly={...steps[2]!,result:{...steps[2]!.result}};delete envelopeOnly.result.value;
    expect(stepOutcome(envelopeOnly)).toBe(answer);
  });
  it("sums recorded usage once, preserves estimates, and never adds placeholder model-call zeros",()=>{
    const rows=[...capture.modelUsage,...capture.modelUsage];
    const calls=modelUsageFromRows(rows,capture.run.id);
    expect(calls).toHaveLength(2);
    expect(summarizeUsage(calls,true)).toEqual({calls:2,input:7768,output:160,total:7928,estimated:true,incomplete:false});
    expect(usageLabel(summarizeUsage(calls,true))).toBe("7.9k estimated tokens");
    expect(modelUsageFromRows(capture.modelCalls,capture.run.id)).toHaveLength(0);
    // The modelCall records are not the usage query. Their absence of the
    // receipt's estimate/pricing metadata must never become an alternate feed.
    expect(capture.modelCalls.every(row=>row.payload.inputTokens===0&&row.payload.outputTokens===0)).toBe(true);
  });
  it("distinguishes actual zero, missing measurements, foreign runs and incomplete pages",()=>{
    const first=capture.modelUsage[0]!;
    const zero=modelUsageFromRows([{...first,inputTokens:0,outputTokens:0,tokensEstimated:false}],capture.run.id);
    expect(usageLabel(summarizeUsage(zero,true))).toBe("0 reported tokens");
    expect(usageLabel(summarizeUsage([],true))).toBe("Usage not reported");
    expect(modelUsageFromRows([{...first,runId:"different-run"}],capture.run.id)).toHaveLength(0);
    const missing=modelUsageFromRows([{...first,outputTokens:undefined}],capture.run.id);
    expect(summarizeUsage(missing,true).total).toBeNull();
    expect(summarizeUsage(missing,true).incomplete).toBe(true);
    expect(usageLabel(summarizeUsage(zero,false))).toBe("0 reported tokens shown");
  });
  it("renders the real goal with authorized usage, readable results and no false zero in the cost details",async()=>{
    const conn=fakeConnection({goals:[flatten(capture.goal)],runs:[flatten(capture.run)],steps:capture.steps.map(flatten),modelCalls:capture.modelCalls.map(flatten),modelUsage:capture.modelUsage});h.connection=conn;
    render(withSession(<NexusApp sectionId="goals" navigate={()=>{}} askContext={()=>{}} intent={{id:"open-capture",payload:{goalId:capture.goal.id}}} consumeIntent={()=>{}} />));
    const journey=await screen.findByRole("region",{name:"Goal journey"});
    await within(screen.getByRole("region",{name:"Current progress"})).findByText("7.9k estimated tokens");
    expect(within(journey).getAllByText(answer)).toHaveLength(2);
    expect(within(journey).queryByText(/No written result/)).toBeNull();
    fireEvent.click(within(journey).getByRole("button",{name:"Write the answer, Done"}));
    const selected=screen.getByRole("region",{name:"Selected action"});
    expect(within(selected).getByText(answer)).toBeTruthy();
    expect(within(selected).getByText("475 estimated tokens")).toBeTruthy();
    expect(within(selected).queryByText("Why")).toBeNull();
    const details=screen.getByText("Usage and execution details").closest("details")!;fireEvent.click(within(details).getByText("Usage and execution details"));
    await waitFor(()=>expect(within(details).getByText("7.8k")).toBeTruthy());
    expect(within(details).getByText("160")).toBeTruthy();
    expect(within(details).queryByText(/unmeasured rather than zero/)).toBeNull();
    expect(conn.query.workModelUsageForOwnerRun).toHaveBeenCalledWith({runId:capture.run.id},expect.objectContaining({signal:expect.any(AbortSignal)}));
  });
});
