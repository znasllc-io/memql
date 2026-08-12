// The one-password agent (memql#3568).
//
// sudo caches an authentication against a timestamp keyed by the terminal --
// with no terminal, by the PARENT PROCESS ID. Every capability script is its own
// process, so no two share the cache and an install that touches the hosts file,
// installs the NSS tools and trusts a CA asked for the password three times.
//
// The agent answers instead: the extension collects it once and serves it over a
// unix socket to an askpass helper every step inherits.
//
// WHAT THESE CASES ARE REALLY ABOUT is where the secret is NOT. Running the
// helper and getting the password back proves the mechanism; the rest prove it
// exists nowhere else -- not in the helper, not in the client, not in the
// directory after disposal -- and that a connection which cannot present the
// nonce gets nothing.

import test from "node:test";
import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import * as fs from "node:fs/promises";
import * as net from "node:net";
import * as path from "node:path";

import { startSudoAgent } from "../src/install/sudoAgent.js";

const SECRET = "correct horse battery staple";

function runHelper(helperPath: string): Promise<string> {
  return new Promise((resolve, reject) => {
    execFile(helperPath, [], (err, stdout) => (err ? reject(err) : resolve(stdout)));
  });
}

test("the helper prints the password sudo asked for", async () => {
  const agent = await startSudoAgent(SECRET, process.execPath);
  try {
    assert.equal((await runHelper(agent.askpassPath)).trim(), SECRET);
  } finally {
    await agent.dispose();
  }
});

test("every step gets the same answer -- that is the whole point", async () => {
  // Three privileged steps, three separate processes, one password.
  const agent = await startSudoAgent(SECRET, process.execPath);
  try {
    for (let i = 0; i < 3; i += 1) {
      assert.equal((await runHelper(agent.askpassPath)).trim(), SECRET);
    }
  } finally {
    await agent.dispose();
  }
});

test("the secret is in no file the agent writes", async () => {
  const agent = await startSudoAgent(SECRET, process.execPath);
  try {
    const dir = path.dirname(agent.askpassPath);
    for (const name of await fs.readdir(dir)) {
      const full = path.join(dir, name);
      if ((await fs.stat(full)).isSocket()) continue;
      const body = await fs.readFile(full, "utf8");
      assert.ok(
        !body.includes(SECRET),
        `${name} contains the password. It is meant to live only in the extension's ` +
          `memory -- a file is something a crash leaves behind.`,
      );
    }
  } finally {
    await agent.dispose();
  }
});

test("the directory is unreadable by anyone else", async () => {
  const agent = await startSudoAgent(SECRET, process.execPath);
  try {
    const dir = path.dirname(agent.askpassPath);
    assert.equal((await fs.stat(dir)).mode & 0o777, 0o700);
  } finally {
    await agent.dispose();
  }
});

test("a connection that cannot present the nonce gets nothing", async () => {
  const agent = await startSudoAgent(SECRET, process.execPath);
  try {
    const socketPath = path.join(path.dirname(agent.askpassPath), "sock");
    const received = await new Promise<string>((resolve) => {
      const conn = net.createConnection(socketPath, () => conn.write("not-the-nonce\n"));
      let out = "";
      conn.on("data", (b) => (out += String(b)));
      conn.on("close", () => resolve(out));
      conn.on("error", () => resolve(out));
    });
    assert.equal(received, "", "the secret was written to a caller that did not prove it belonged");
  } finally {
    await agent.dispose();
  }
});

test("disposing takes the whole thing away", async () => {
  const agent = await startSudoAgent(SECRET, process.execPath);
  const dir = path.dirname(agent.askpassPath);
  await agent.dispose();

  await assert.rejects(() => fs.stat(dir), "the agent's directory outlived it");
  // Idempotent: the panel disposes on run-end AND on close, and the second must
  // not throw into a path that has nothing to do with sudo.
  await agent.dispose();
});
