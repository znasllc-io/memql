const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { downloadVSCode, exitCodeFor } = require("./download.cjs");

function fixture(t, body) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "memql-download-test-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const worker = path.join(root, "worker.cjs");
  const counter = path.join(root, "attempts");
  fs.writeFileSync(worker, `const fs = require('node:fs');
    const counter = ${JSON.stringify(counter)};
    const n = fs.existsSync(counter) ? Number(fs.readFileSync(counter, 'utf8')) + 1 : 1;
    fs.writeFileSync(counter, String(n));
    ${body}`);
  return { worker, retryDelayMs: 0, count: () => Number(fs.readFileSync(counter, "utf8")) };
}

test("retries a download process killed by the observed unhandled ECONNRESET", async t => {
  const f = fixture(t, `if (n === 1) Promise.reject(Object.assign(new Error('aborted'), {code:'ECONNRESET'}));
    else process.send({executable:'/download/Code'}, () => process.disconnect());`);
  assert.equal(await downloadVSCode({ version: "1.91.0" }, f), "/download/Code");
  assert.equal(f.count(), 2);
});

test("retries the upstream request timeout whether caught or unhandled", async t => {
  const message = "@vscode/test-electron request timeout out after 15000ms";
  for (const failure of [
    `Promise.reject(Object.assign(new Error(${JSON.stringify(message)}), {name:'TimeoutError'}));`,
    `console.error(${JSON.stringify(message)}); process.exit(1);`,
  ]) {
    const f = fixture(t, `if (n === 1) { ${failure} }
      else process.send({executable:'/download/Code'}, () => process.disconnect());`);
    assert.equal(await downloadVSCode({ version: "stable" }, f), "/download/Code");
    assert.equal(f.count(), 2);
  }
});

test("persistent transport failure stops after three attempts", async t => {
  const f = fixture(t, `Promise.reject(Object.assign(new Error('aborted'), {code:'ECONNRESET'}));`);
  await assert.rejects(downloadVSCode({}, f), /ECONNRESET/);
  assert.equal(f.count(), 3);
});

test("a stalled downloader is killed and stops after three bounded attempts", { timeout: 10000 }, async t => {
  const f = fixture(t, "setInterval(() => {}, 1000);");
  await assert.rejects(downloadVSCode({}, { ...f, timeoutMs: 500 }), /download stalled/);
  assert.equal(f.count(), 3);
});

test("invalid version and missing executable receipts fail without retries", async t => {
  for (const body of ["console.error('Version not found: HTTP 404'); process.exit(1);", "process.disconnect();"]) {
    const f = fixture(t, body);
    await assert.rejects(downloadVSCode({}, f), /download failed/);
    assert.equal(f.count(), 1);
  }
});

test("a receipt cannot hide a failed downloader process", async t => {
  const f = fixture(t, "process.send({executable:'/download/Code'}, () => process.exit(1));");
  await assert.rejects(downloadVSCode({}, f), /download failed/);
  assert.equal(f.count(), 1);
});


test("a progressing transfer outlives the idle watchdog without restarting", {timeout:10000}, async t => {
  const f = fixture(t, `let bytes=0; const timer=setInterval(()=>{
    process.send({progress:{stage:'downloading',bytes:++bytes}});
    if (bytes===12) {clearInterval(timer);process.send({executable:'/download/Code'},()=>process.disconnect());}
  },100);`);
  assert.equal(await downloadVSCode({}, {...f, timeoutMs:500,totalTimeoutMs:5000}), '/download/Code');
  assert.equal(f.count(),1);
});

test("repeated progress cannot keep a stalled downloader alive", {timeout:10000}, async t => {
  const f = fixture(t, `setInterval(()=>process.send({progress:{stage:'downloading',bytes:1}}),20);`);
  await assert.rejects(downloadVSCode({}, {...f, timeoutMs:500,totalTimeoutMs:5000}), /download stalled/);
  assert.equal(f.count(),3);
});

test("continuous progress still stops at the shared overall deadline", {timeout:10000}, async t => {
  const f = fixture(t, `let bytes=0;setInterval(()=>process.send({progress:{stage:'downloading',bytes:++bytes}}),20);`);
  await assert.rejects(downloadVSCode({}, {...f, timeoutMs:500,totalTimeoutMs:1000}), /total time limit/);
  assert.equal(f.count(),1);
});

test("transport retries share one overall time budget", {timeout:10000}, async t => {
  const f = fixture(t, `setTimeout(()=>{console.error('ECONNRESET');process.exit(1)},400);`);
  const started=performance.now();
  await assert.rejects(downloadVSCode({}, {...f, timeoutMs:2000,totalTimeoutMs:1000}), /total time limit|ECONNRESET/);
  assert.ok(performance.now()-started<2000,'each retry must not restart the total budget');
});


test("an upstream transport retry resets only its byte counter", {timeout:10000}, async t => {
  const f = fixture(t, `let ticks=0;const timer=setInterval(()=>{
    ticks++;
    if(ticks===2) process.send({progress:{stage:'retrying'}});
    else process.send({progress:{stage:'downloading',bytes:ticks===1?100000:ticks}});
    if(ticks===12){clearInterval(timer);process.send({executable:'/download/Code'},()=>process.disconnect());}
  },100);`);
  assert.equal(await downloadVSCode({}, {...f,timeoutMs:500,totalTimeoutMs:5000}),'/download/Code');
  assert.equal(f.count(),1);
});

test("a download that failed for a passing reason asks the pipeline to retry the step", {timeout:10000}, async t => {
  const stalled = fixture(t, "setInterval(() => {}, 1000);");
  const stall = await downloadVSCode({}, {...stalled, timeoutMs:300}).catch(error => error);
  assert.match(stall.message, /download stalled/);
  assert.equal(exitCodeFor(stall), 75);

  const slow = fixture(t, `let bytes=0;setInterval(()=>process.send({progress:{stage:'downloading',bytes:++bytes}}),20);`);
  const limit = await downloadVSCode({}, {...slow, timeoutMs:500, totalTimeoutMs:600}).catch(error => error);
  assert.match(limit.message, /total time limit/);
  assert.equal(exitCodeFor(limit), 75);
  // The same words thrown outside the download are final.
  assert.equal(exitCodeFor(new Error(limit.message)), 1);

  const transport = fixture(t, `Promise.reject(Object.assign(new Error('aborted'), {code:'ECONNRESET'}));`);
  const reset = await downloadVSCode({}, transport).catch(error => error);
  assert.match(reset.message, /ECONNRESET/);
  assert.equal(exitCodeFor(reset), 75);
});

test("a download that cannot succeed by retrying fails the step", async t => {
  for (const body of ["console.error('Version not found: HTTP 404'); process.exit(1);", "process.disconnect();"]) {
    const f = fixture(t, body);
    const error = await downloadVSCode({}, f).catch(error => error);
    assert.match(error.message, /download failed/);
    assert.equal(exitCodeFor(error), 1);
  }
});

test("only a download failure can ask for a retry", () => {
  // A failed host test, or anything else thrown outside the download, is final.
  assert.equal(exitCodeFor(Object.assign(new Error("3 host tests failed"), {retryable:true})), 1);
  assert.equal(exitCodeFor("ECONNRESET"), 1);
  assert.equal(exitCodeFor(undefined), 1);
});
