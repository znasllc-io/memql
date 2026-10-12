import { test } from "node:test";
import assert from "node:assert/strict";
import { startMaterialize } from "@znasllc-io/memql-sdk-core/client";
import { diagnosticText, ProblemReporter, ReportedError, UserInputError, userMessage } from "../src/problems.js";
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
  assert.equal(userMessage(new Error("Select a passage first. INTERNAL password=abc"),"save"),"Couldn’t save. Try again. If this continues, use the troubleshooting reference.");
  assert.match(userMessage(new Error("expected version mismatch"),"save"),/Compare with the latest/);
  const denied=new Error("Permission denied");denied.name="NotAllowedError";
  assert.match(userMessage(denied,"transcribe your feedback"),/Microphone access/);
  assert.doesNotMatch(userMessage(denied,"copy the reference"),/Microphone/);
  assert.match(userMessage(new Error("context checkpoint cannot fit the active request, tool contracts and pending exchange; history retained"),"prepare changes"),/Your work is saved/);
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
