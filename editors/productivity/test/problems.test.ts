import { test } from "node:test";
import assert from "node:assert/strict";
import * as vscode from "vscode";
import { configureProblems, ProblemReporter, ReportedError, UserInputError, userMessage } from "../src/problems.js";
import { setProblemOutput, showProblem } from "../src/problemUI.js";
import type { EditorConnectionAPI } from "../../vscode/src/connection/api.js";

test("unexpected failures give safe messages and searchable, redacted diagnostics", async () => {
  const calls: any[] = [], output: string[] = [];
  const lease = {domain:"example.test",name:"test",generation:1};
  const api = {current:()=>lease,execute:async (...args:unknown[])=>{calls.push(args);return[{accepted:1}];}} as unknown as EditorConnectionAPI;
  const reporter = new ProblemReporter(api,line=>output.push(line));
  const failure = new Error('function "runAgentTurn" execution failed: idle ceiling Bearer private-token password=secret-value');
  const problem=reporter.report(failure,"prepare changes",lease,"review-run-test");
  assert.equal(problem.reference,"review-run-test");assert.match(problem.message,/AI stopped responding/);
  assert.doesNotMatch(problem.message,/runAgentTurn|idle ceiling|private-token/);
  assert.deepEqual(reporter.report(failure,"prepare changes",lease,"review-run-test"),problem);
  assert.deepEqual(reporter.report(new ReportedError(problem),"save"),problem);
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(calls.length,1);assert.equal(calls[0][0],lease);assert.equal(calls[0][1],"logsRecordClient");
  assert.match(calls[0][2],/\[review-run-test\]/);assert.match(calls[0][2],/runAgentTurn/);
  assert.doesNotMatch(calls[0][2],/private-token|secret-value/);assert.doesNotMatch(output.join(""),/private-token|secret-value/);
});

test("polling one durable failure keeps its reference without logging it every minute",()=>{
  const output:string[]=[];
  const api={current:()=>undefined} as unknown as EditorConnectionAPI;
  const reporter=new ProblemReporter(api,line=>output.push(line));
  const failure=new Error("idle ceiling"), now=Date.now;
  try {
    const first=reporter.report(failure,"prepare changes",undefined,"review-run-repeated");
    Date.now=()=>now()+120_000;
    assert.deepEqual(reporter.report(failure,"prepare changes",undefined,"review-run-repeated"),first);
    assert.equal(output.length,1);
  } finally { Date.now=now; reporter.dispose(); }
});

test("input validation stays actionable; arbitrary server strings never pass through",()=>{
  assert.equal(userMessage(new UserInputError("Select a passage first."),"save"),"Select a passage first.");
  assert.equal(userMessage(new Error("Select a passage first. INTERNAL password=abc"),"save"),"Couldn’t save. Try again. If this continues, choose Show details.");
  assert.match(userMessage(new Error("expected version mismatch"),"save"),/Compare with the latest/);
  const denied=new Error("Permission denied");denied.name="NotAllowedError";
  assert.match(userMessage(denied,"transcribe your feedback"),/Microphone access/);
  assert.doesNotMatch(userMessage(denied,"copy the reference"),/Microphone/);
  assert.match(userMessage(new Error("context checkpoint cannot fit the active request, tool contracts and pending exchange; history retained"),"prepare changes"),/Your work is saved\. If retrying doesn’t help, choose Show details\.$/);
  assert.match(userMessage(new Error("model_call_timeout: deadline exceeded"),"prepare changes"),/AI couldn’t finish/);
});

test("an AI work limit keeps its cause when wrapped by a context checkpoint failure",()=>{
  const direct="run review-run-test reached its modelCalls ceiling at step research (limit 16 calls, 16 made): the run reached its model-call cap";
  const wrapped=`function "runAgentTurn" execution failed: context checkpoint failed; history retained: ${direct}`;
  const message=userMessage(new Error(wrapped),"prepare changes");
  assert.equal(message,userMessage(new Error(direct),"prepare changes"));
  assert.match(message,/AI work limit/);
  assert.doesNotMatch(message,/room|context|16|runAgentTurn/);
  assert.match(userMessage(new Error("context checkpoint cannot fit the active request; history retained"),"prepare changes"),/room/);
});

test("offline and refused diagnostics preserve a local reference without retries or recursion",async()=>{
  const output:string[]=[];let sends=0;
  const api={current:()=>undefined,execute:async()=>{sends++;throw new Error("log transport failed");}} as unknown as EditorConnectionAPI;
  const reporter=new ProblemReporter(api,line=>output.push(line));
  const offline=reporter.report(new Error("PDF parser failed"),"open the PDF");
  assert.match(offline.reference!,/^tools-/);assert.equal(sends,0);assert.match(output[0],/PDF parser failed/);
  reporter.report(new Error("failed"),"save",{domain:"a.test",name:"a",generation:1});
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(sends,1);assert.match(output.at(-1)!,/Cluster logging unavailable/);
});

test("diagnostic concurrency remains bounded when the transport stalls",async()=>{
  let sends=0;
  const api={current:()=>({domain:"a.test",name:"a",generation:1}),execute:()=>{sends++;return new Promise(()=>{});}} as unknown as EditorConnectionAPI;
  const reporter=new ProblemReporter(api,()=>{});
  for(let n=0;n<250;n++)reporter.report(new Error(`failure ${n}`),"save");
  await new Promise(resolve=>setImmediate(resolve));assert.equal(sends,8);
});

test("Show details reveals the output instead of a modal", async () => {
  const toasts: { message: string; buttons: string[] }[] = [], dialogs: unknown[][] = [], shown: unknown[][] = [], lines: string[] = [];
  const channel = { appendLine: (line: string) => { lines.push(line); }, show: (...args: unknown[]) => { shown.push(args); } };
  const window = vscode.window as unknown as Record<string, (...args: any[]) => unknown>;
  const original = { showErrorMessage: window.showErrorMessage, showInformationMessage: window.showInformationMessage, showWarningMessage: window.showWarningMessage };
  // The person's answer: a toast can only return a button it offered.
  let answer: string | undefined = "Show details";
  window.showErrorMessage = async (message: string, ...buttons: string[]) => { toasts.push({ message, buttons }); return answer !== undefined && buttons.includes(answer) ? answer : undefined; };
  window.showInformationMessage = async (...args: unknown[]) => { dialogs.push(args); };
  window.showWarningMessage = async (...args: unknown[]) => { dialogs.push(args); };
  const reporter = new ProblemReporter({ current: () => undefined } as unknown as EditorConnectionAPI, line => channel.appendLine(line));
  configureProblems(reporter);
  setProblemOutput(channel as unknown as vscode.OutputChannel);
  try {
    await showProblem(new Error("PDF parser failed"), "open the PDF");
    assert.equal(toasts.length, 1);
    assert.equal(toasts[0].message, "Couldn’t open the PDF. Try again. If this continues, choose Show details.", "the message sends the person to the button it carries");
    assert.deepEqual(toasts[0].buttons, ["Show details"], "one toast, one button");
    assert.equal(shown.length, 1, "Show details reveals the output channel");
    const reference = /\[(tools-[^\]]+)\]/.exec(lines[0] ?? "")?.[1];
    assert.ok(reference, "the output channel holds a line with the reference");
    assert.match(lines[0], new RegExp(`\\[${reference}\\] open the PDF: .*PDF parser failed`));
    assert.deepEqual(dialogs, [], "no second dialog opens, modal or not");

    // Dismissing the toast reveals nothing.
    answer = undefined;
    shown.length = 0;
    await showProblem(new Error("PDF parser failed"), "open the PDF");
    assert.equal(toasts.length, 2);
    assert.equal(shown.length, 0);

    // A problem with no reference, an input the person can fix, has nothing to show: the toast carries no button.
    answer = "Show details";
    await showProblem(new UserInputError("Select a passage first."), "save");
    assert.deepEqual(toasts[2], { message: "Select a passage first.", buttons: [] });
    assert.equal(shown.length, 0);
    assert.deepEqual(dialogs, []);
  } finally {
    for (const [name, replaced] of Object.entries(original)) { if (replaced === undefined) delete window[name]; else window[name] = replaced; }
    reporter.dispose();
  }
});

test("a notification VS Code draws itself carries the reference, since it has no Show details button", () => {
  const generic = userMessage(new Error("PDF parser failed"), "save the PDF");
  assert.equal(generic, "Couldn’t save the PDF. Try again. If this continues, choose Show details.");
  assert.equal(new ReportedError({ message: generic, reference: "tools-4f7c2a" }).message,
    "Couldn’t save the PDF. Try again. Reference: tools-4f7c2a. Search it in MemQL OS Logs.");
  const room = userMessage(new Error("context checkpoint cannot fit the active request; history retained"), "prepare changes");
  assert.equal(new ReportedError({ message: room, reference: "tools-1" }).message,
    "MemQL couldn’t make enough room to continue this request. Your work is saved. Reference: tools-1. Search it in MemQL OS Logs.");
  // A message that sends the person nowhere is carried whole, and a problem with no reference has nothing to add.
  assert.equal(new ReportedError({ message: "The AI stopped responding. Try again.", reference: "tools-2" }).message,
    "The AI stopped responding. Try again. Reference: tools-2. Search it in MemQL OS Logs.");
  assert.equal(new ReportedError({ message: "Select a passage first." }).message, "Select a passage first.");
  // The problem itself keeps the whole message: the toast and the panels show it with their own control.
  assert.equal(new ReportedError({ message: generic, reference: "tools-3" }).problem.message, generic);
});
