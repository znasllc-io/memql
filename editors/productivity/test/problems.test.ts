import { test } from "node:test";
import assert from "node:assert/strict";
import { ProblemReporter, ReportedError, UserInputError, userMessage } from "../src/problems.js";
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
