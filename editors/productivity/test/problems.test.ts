import { test } from "node:test";
import assert from "node:assert/strict";
import * as vscode from "vscode";
import { startMaterialize } from "@znasllc-io/memql-sdk-core/client";
import { configureProblems, diagnosticText, ProblemReporter, ReportedError, UserInputError, userMessage } from "../src/problems.js";
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

// The SDK hands a person a refusal as one sentence and keeps the refusal as it arrived -- the
// call's name and the engine's frame -- as the error's cause. The diagnostic is where those
// frames belong, so it carries the cause, straight after the first line where the length cap
// cannot cut it off, and redacted like the rest.
const AS_IT_ARRIVED = "composeMaterialize: MemQL engine failed to execute query. Details: Enter a name without slashes.";

test("a start refusal keeps the call's name and the engine's frame in its diagnostic, through its cause", async () => {
  const refused = await startMaterialize(async () => { throw new Error(AS_IT_ARRIVED); }, { name: "Notes 10/11", format: "markdown", requestId: "req-1" })
    .then(() => null, (error: unknown) => error);
  assert.ok(refused instanceof Error);
  assert.equal(refused.message, "Enter a name without slashes.");
  assert.ok(diagnosticText(refused).startsWith(`Error: Enter a name without slashes.\nCaused by: ${AS_IT_ARRIVED}\n`));
  const output: string[] = [];
  const reporter = new ProblemReporter({ current: () => undefined } as unknown as EditorConnectionAPI, line => output.push(line));
  reporter.report(refused, "create the Markdown document");
  assert.ok(output[0]?.includes(AS_IT_ARRIVED), output[0]);
});

test("every cause is kept, ahead of a long stack, and redacted like the rest", () => {
  const error = new Error("outer", { cause: new Error("middle", { cause: "innermost Bearer private-token" }) });
  error.stack = `Error: outer\n${"    at frame (file.js:1:1)\n".repeat(200)}`;
  const text = diagnosticText(error);
  assert.ok(text.startsWith("Error: outer\nCaused by: middle\nCaused by: innermost Bearer [redacted]\n"), text.slice(0, 120));
  assert.doesNotMatch(text, /private-token/);
  assert.equal(diagnosticText(new Error("alone")).split("\n")[0], "Error: alone");
  assert.doesNotMatch(diagnosticText(new Error("alone")), /Caused by/);
});

// A refusal reaches a person in the cluster's words (the SDK's refusalSentence takes the call's name, the
// engine's frame and a Go package name off), never as "Couldn’t <operation>. Try again".
test("a refusal from the cluster reads in the cluster's words", () => {
  const refused = (sentence: string, call = "campaignSaveTemplate") => new Error(`${call}: MemQL engine failed to execute query. Details: ${sentence}`);
  for (const sentence of [
    "Someone else is saving this template. Try again in a moment.",
    "This file is already linked to a template you can't open. Save it with a different template.",
    "Previews are busy right now. Try again in a moment.",
  ]) assert.equal(userMessage(refused(sentence), "publish the email"), sentence);
  // A sentence a Go error opens in lower case is shown as a sentence.
  assert.equal(userMessage(refused("fix these before publishing: Add a link to “Start”. Choose a picture for “Banner”."), "publish the email"),
    "Fix these before publishing: Add a link to “Start”. Choose a picture for “Banner”.");
  assert.equal(userMessage(refused("compose: the sources could not be read", "composeMaterialize"), "create the email"), "The sources could not be read");
  // The frame without a call's name in front of it is the same refusal.
  assert.equal(userMessage(new Error("MemQL engine failed to execute query. Details: That folder isn't available. Choose another folder."), "save"), "That folder isn't available. Choose another folder.");
  // A refusal the SDK has already put in words is found through its cause, and keeps them.
  const worded = new Error("Enter a name without slashes.", { cause: refused("Enter a name without slashes.", "composeMaterialize") });
  assert.equal(userMessage(worded, "create the Markdown document"), "Enter a name without slashes.");
  // What an AI run says about itself is still put in plain words first.
  assert.match(userMessage(refused('function "runAgentTurn" execution failed: idle ceiling', "libraryRequestDocumentRevision"), "prepare changes"), /^The AI stopped responding\./);
  // Credential material in a refusal never reaches the person.
  const secret = userMessage(refused("token=abc123 and Bearer xyz789 were refused"), "publish the email");
  assert.doesNotMatch(secret, /abc123|xyz789/);
  assert.match(secret, /\[redacted\]/);
  // A frame with nothing in it, and a message that is not the cluster's refusal, keep the safe message.
  assert.equal(userMessage(new Error("campaignSaveTemplate: MemQL engine failed to execute query."), "publish the email"), "Couldn’t publish the email. Try again. If this continues, choose Show details.");
  assert.equal(userMessage(new Error("campaignSaveTemplate: MemQL engine failed to execute query. Details:   "), "publish the email"), "Couldn’t publish the email. Try again. If this continues, choose Show details.");
  assert.equal(userMessage(new Error("campaignSaveTemplate: invalid query syntax"), "publish the email"), "Couldn’t publish the email. Try again. If this continues, choose Show details.");
  assert.equal(userMessage(new Error("The MemQL connection changed. Keep your edits, reconnect to the original cluster, and compare with its latest version before saving."), "publish the email"),
    "The MemQL connection changed. Reconnect to the file’s cluster and compare with its latest version before saving.");
});

test("a reported refusal keeps a reference, since the cluster's trouble can be its own", () => {
  const output: string[] = [];
  const reporter = new ProblemReporter({ current: () => undefined } as unknown as EditorConnectionAPI, line => output.push(line));
  const problem = reporter.report(new Error("campaignSaveTemplate: MemQL engine failed to execute query. Details: Someone else is saving this template. Try again in a moment."), "publish the email");
  assert.equal(problem.message, "Someone else is saving this template. Try again in a moment.");
  assert.match(problem.reference ?? "", /^tools-/);
  assert.match(output[0] ?? "", /campaignSaveTemplate: MemQL engine failed to execute query\. Details: Someone else/);
  reporter.dispose();
});
