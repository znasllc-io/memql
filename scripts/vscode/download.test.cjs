const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { downloadVSCode } = require("./download.cjs");

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

test("persistent transport failure stops after three attempts", async t => {
  const f = fixture(t, `Promise.reject(Object.assign(new Error('aborted'), {code:'ECONNRESET'}));`);
  await assert.rejects(downloadVSCode({}, f), /ECONNRESET/);
  assert.equal(f.count(), 3);
});

test("a stalled downloader is killed and stops after three bounded attempts", { timeout: 10000 }, async t => {
  const f = fixture(t, "setInterval(() => {}, 1000);");
  await assert.rejects(downloadVSCode({}, { ...f, timeoutMs: 500 }), /download timed out/);
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
