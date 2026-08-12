// Asking for the password once.
//
// THE PROBLEM. Three install steps need root -- the hosts block, the NSS tools,
// trusting the local CA -- and each is a separate capability script, so each is
// a separate process. sudo caches an authentication against a timestamp keyed by
// the terminal, or, when there is no terminal, by the PARENT PROCESS ID. Every
// script is its own parent, so no two share the cache and the operator is asked
// three times for one install.
//
// Nothing about that is fixable from inside a script. The cache is sudo's and
// its key is a fact about process lineage.
//
// WHAT THIS DOES INSTEAD. The extension asks ONCE, through VS Code's own masked
// input, and holds the answer in memory. It then starts a tiny server on a unix
// socket and writes an askpass helper that reads from it. Every step gets
// `SUDO_ASKPASS` pointing at that helper, so each sudo asks the extension rather
// than the operator, and the operator types their password one time.
//
// WHERE THE SECRET LIVES, AND WHERE IT DOES NOT.
//
//   IN MEMORY, in the extension host, for the length of the run.
//   NEVER ON DISK -- not in the helper, not in a cache file, not in the receipt.
//   NEVER IN AN ENVIRONMENT VARIABLE. This is the design's whole reason for
//   being a socket rather than the twenty-line version that exports the password
//   and has the helper echo it. Environments are INHERITED: a password exported
//   for the scripts is also in the environment of mkcert, apt-get, tee and
//   everything else they spawn, any of which may log its own environment. Over a
//   socket the secret exists only in the helper's stdout pipe, which is sudo's.
//
// WHAT IT DOES NOT PROTECT AGAINST, stated plainly: another process running as
// THE SAME USER. Such a process can already ptrace the extension host, read its
// memory, or replace the helper. The socket lives in a 0700 directory and is
// gated on a nonce, which raises the bar over a guessable path; it does not
// change the trust boundary, and nothing at this layer could.
//
// Free of `vscode` imports so it is unit-testable under plain `node --test`;
// the panel supplies the secret it collected.
//
// Refs: #3568 #3562

import { spawn } from "node:child_process";
import * as crypto from "node:crypto";
import * as fs from "node:fs/promises";
import * as net from "node:net";
import * as os from "node:os";
import * as path from "node:path";

export interface SudoAgent {
  /** Point `SUDO_ASKPASS` at this. */
  askpassPath: string;
  /** Stops the server and removes the directory. Safe to call twice. */
  dispose: () => Promise<void>;
}

/**
 * Starts the agent.
 *
 * `nodePath` is the interpreter the helper runs -- `process.execPath` in the
 * extension host. Passed in rather than read here so a test can point it
 * somewhere else, and so this module makes no assumption about how it was
 * launched.
 */
export async function startSudoAgent(secret: string, nodePath: string): Promise<SudoAgent> {
  // 0700 BEFORE anything is written into it. mkdtemp is already 0700 on the
  // platforms this runs on; setting it explicitly means the guarantee does not
  // depend on that staying true.
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-sudo-"));
  await fs.chmod(dir, 0o700);

  const socketPath = path.join(dir, "sock");
  const clientPath = path.join(dir, "client.js");
  const askpassPath = path.join(dir, "askpass");
  const nonce = crypto.randomBytes(32).toString("hex");

  const server = net.createServer((conn) => {
    // The client speaks first. A connection that does not present the nonce
    // gets nothing and is closed -- the secret is never written speculatively.
    let seen = "";
    const timer = setTimeout(() => conn.destroy(), 5_000);
    conn.on("data", (chunk) => {
      seen += String(chunk);
      if (seen.length > nonce.length + 1) {
        clearTimeout(timer);
        conn.destroy();
        return;
      }
      if (!seen.startsWith(nonce)) {
        // Still could be a prefix of it; wait for more unless it cannot be.
        if (!nonce.startsWith(seen.replace(/\n$/, ""))) {
          clearTimeout(timer);
          conn.destroy();
        }
        return;
      }
      if (seen.includes("\n")) {
        clearTimeout(timer);
        conn.end(`${secret}\n`);
      }
    });
    conn.on("error", () => clearTimeout(timer));
  });

  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(socketPath, () => resolve());
  });
  await fs.chmod(socketPath, 0o600);

  await fs.writeFile(
    clientPath,
    `// Reads the password from the extension and prints it for sudo.
const net = require("node:net");
const c = net.createConnection(${JSON.stringify(socketPath)}, () => c.write(${JSON.stringify(nonce)} + "\\n"));
let out = "";
c.on("data", (b) => (out += String(b)));
c.on("end", () => process.stdout.write(out));
c.on("error", () => process.exit(1));
`,
    { mode: 0o600 },
  );

  // sudo EXECUTES this, so it needs the bit; it carries no secret of its own.
  await fs.writeFile(
    askpassPath,
    `#!/bin/sh\nexec ${shellQuote(nodePath)} ${shellQuote(clientPath)}\n`,
    { mode: 0o700 },
  );

  let disposed = false;
  return {
    askpassPath,
    dispose: async () => {
      if (disposed) return;
      disposed = true;
      await new Promise<void>((resolve) => server.close(() => resolve()));
      await fs.rm(dir, { recursive: true, force: true });
    },
  };
}

/** A path as one inert word for /bin/sh. */
function shellQuote(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`;
}

/**
 * Whether sudo will run without asking: this user is NOPASSWD, or a recent
 * authentication is still cached.
 *
 * `-n` so the probe itself can never block. Asking someone for a password sudo
 * was never going to want is the surest way to teach them to type it at
 * anything.
 */
export async function sudoRunsWithoutAsking(): Promise<boolean> {
  return (await runSudo(["-n", "true"], {})) === 0;
}

/**
 * Whether the password behind this helper is the right one.
 *
 * `-v` validates and refreshes the timestamp WITHOUT running a command, so a
 * typo is caught before the graph starts rather than nine minutes into it --
 * and `-k` first, so a cached authentication cannot make a wrong password look
 * right.
 */
export async function sudoAccepts(askpassPath: string): Promise<boolean> {
  await runSudo(["-k"], {});
  return (await runSudo(["-A", "-v"], { SUDO_ASKPASS: askpassPath })) === 0;
}

function runSudo(args: string[], extraEnv: Record<string, string>): Promise<number> {
  return new Promise((resolve) => {
    // No shell: each argument is its own argv element, as everywhere else in
    // this substrate.
    const child = spawn("sudo", args, {
      stdio: ["ignore", "ignore", "ignore"],
      env: { ...process.env, ...extraEnv },
    });
    child.on("error", () => resolve(127));
    child.on("close", (code) => resolve(code ?? 1));
  });
}
