const assert = require("node:assert/strict");
const fs = require("node:fs");
const net = require("node:net");
const os = require("node:os");
const path = require("node:path");
const { once } = require("node:events");
const { test } = require("node:test");
const { createHostProfile } = require("./host-profile.cjs");

test("a native checkout with deep scratch supports an actual editor socket", async (t) => {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), "host-"));
  t.after(() => fs.rmSync(base, { recursive: true, force: true }));
  // Match the observed Mac checkout's byte length, then its nested TMPDIR.
  const checkout = path.join(base, "x".repeat(Math.max(1, 68 - Buffer.byteLength(base) - 1)));
  fs.mkdirSync(checkout);
  const scratch = path.join(checkout, ".git", "memql-scratch-86824088");
  fs.mkdirSync(scratch, { recursive: true });
  const oldTmp = process.env.TMPDIR;
  process.env.TMPDIR = scratch;
  t.after(() => { if (oldTmp === undefined) delete process.env.TMPDIR; else process.env.TMPDIR = oldTmp; });
  const profile = createHostProfile(checkout);
  t.after(profile.dispose);
  assert.equal(path.dirname(profile.root), checkout);
  assert.equal(fs.statSync(profile.root).mode & 0o777, 0o700);
  assert.equal(fs.statSync(profile.userData).mode & 0o777, 0o700);
  const socket = path.join(profile.userData, "1.14-main.sock");
  assert.ok(Buffer.byteLength(socket) <= 103, socket);
  const server = net.createServer((connection) => connection.end("ready"));
  server.listen(socket);
  await once(server, "listening");
  try {
    assert.ok(fs.statSync(socket).isSocket());
    const client = net.connect(socket);
    client.setEncoding("utf8");
    const [message] = await once(client, "data");
    assert.equal(message, "ready");
    await once(client, "close");
  } finally {
    await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
  }
  profile.dispose();
  assert.equal(fs.existsSync(profile.root), false);
  assert.deepEqual(fs.readdirSync(scratch), []);
});

test("an overlong checkout refuses before launching and removes its own profile", (t) => {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), "host-"));
  t.after(() => fs.rmSync(base, { recursive: true, force: true }));
  const checkout = path.join(base, "x".repeat(105));
  fs.mkdirSync(checkout);
  fs.writeFileSync(path.join(checkout, "keep"), "source");
  assert.throws(() => createHostProfile(checkout), /checkout path is too long/);
  assert.deepEqual(fs.readdirSync(checkout), ["keep"]);
  assert.equal(fs.readFileSync(path.join(checkout, "keep"), "utf8"), "source");
});
