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
// IS THE VS CODE PASSWORD BOX SECURE? An operator asked, and the answer is a
// trade rather than a reassurance (memql#3586).
//
//   NEITHER PROMPT IS SYSTEM-TRUSTED. The VS Code input box and the zenity box
//   `scripts/lib/elevate.sh` builds are both drawn by ordinary user processes,
//   and any program running as this user can draw either. Only polkit and the
//   macOS authorization dialog are drawn by the OS -- and pkexec was rejected
//   above for a different reason: it runs the whole command as root, which
//   breaks mkcert's CAROOT resolution.
//
//   THE VS CODE BOX MEANS THE PASSWORD TRANSITS THIS PROCESS. It arrives in an
//   ordinary unlocked JS string, lives in the extension host's heap for the
//   length of the run, may appear in a heap snapshot or a core dump, and cannot
//   be reliably zeroed. The zenity path never hands it to the extension at all:
//   it goes from the dialog's stdout to sudo's stdin.
//
//   SO ASKING ONCE IS THE REASON THE EXTENSION HOLDS IT. That is the whole
//   trade, and it is worth stating rather than implying that the convenient
//   option is also the safest one. What holding it does NOT do is put the
//   password on disk or into any child process's environment; both are argued
//   above and the on-disk half has a test.
//
// A stronger posture is available and deliberately not built: a setting that
// asks per step through the desktop dialog and never holds the password. It
// costs three prompts per install, which is the thing memql#3568 set out to fix,
// so it belongs to an operator who wants it rather than to everyone.
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
  const noncePath = path.join(dir, "nonce");
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

  await fs.writeFile(noncePath, nonce, { mode: 0o600 });

  // A CONSTANT PROGRAM, PARAMETERISED BY ARGV. The first version built this
  // source by interpolating the socket path and the nonce into it, which is
  // constructing code from values -- correct here only because JSON.stringify
  // happens to be the right escaping for a JS string literal, and not a property
  // anyone should have to re-derive when editing it. CodeQL was right to say so.
  //
  // The nonce is READ FROM A FILE rather than passed as an argument, because
  // argv is visible to every process this user owns (`ps`), and the nonce is the
  // gate on the socket. The path is not a secret; what it points at is.
  await fs.writeFile(clientPath, SUDO_ASKPASS_CLIENT, { mode: 0o600 });

  // sudo EXECUTES this, so it needs the bit; it carries no secret of its own.
  await fs.writeFile(
    askpassPath,
    `#!/bin/sh\nexec ${shellQuote(nodePath)} ${shellQuote(clientPath)} ${shellQuote(dir)}\n`,
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

/**
 * The askpass client, verbatim -- no value is ever interpolated into it.
 *
 * It is handed the agent's directory and finds the socket and the nonce inside,
 * so the only thing that varies between runs is an argument, never the program.
 */
const SUDO_ASKPASS_CLIENT = `// Reads the password from the MemQL installer and prints it for sudo.
const fs = require("node:fs");
const net = require("node:net");
const path = require("node:path");

const dir = process.argv[2];
const nonce = fs.readFileSync(path.join(dir, "nonce"), "utf8").trim();
const conn = net.createConnection(path.join(dir, "sock"), () => conn.write(nonce + "\\n"));
let out = "";
conn.on("data", (b) => (out += String(b)));
conn.on("end", () => process.stdout.write(out));
conn.on("error", () => process.exit(1));
`;

/** A path as one inert word for /bin/sh. */
function shellQuote(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`;
}

/**
 * The environment that tells a capability script the wizard owns the asking.
 *
 * WHY THIS IS NOT JUST `SUDO_ASKPASS` (memql#3586). Every capability script can
 * build its own desktop password dialog, which is the right answer for a human
 * running one by hand and the wrong one here: this wizard has an input box, and
 * a script that prompts as well puts a second, differently-shaped question in
 * front of someone who has already answered.
 *
 * With `SUDO_ASKPASS` alone, that consistency rested on the agent ALWAYS being
 * created -- and the run where it was not is exactly the run that asked three
 * times through the desktop. So the marker travels unconditionally and says the
 * true thing on its own: whoever owns the run owns the asking. `elevate_method`
 * then answers `none` when no helper was inherited, and the step refuses with
 * the terminal remedy it already carries.
 *
 * SUDO_ASKPASS is ABSENT rather than empty when there is no agent: sudo reads an
 * empty value as a helper it cannot execute, which is a different and worse
 * failure than having none.
 */
export const ELEVATE_DIALOG_ENV = "MEMQL_ELEVATE_DIALOG";

export function elevationEnv(askpassPath: string | undefined): Record<string, string> {
  return {
    [ELEVATE_DIALOG_ENV]: "never",
    ...(askpassPath !== undefined && askpassPath !== "" ? { SUDO_ASKPASS: askpassPath } : {}),
  };
}

/** How the probe below runs sudo. Injected by tests, which must never reach it. */
export type SudoRunner = (args: string[]) => Promise<number>;

/**
 * Whether sudo will run without asking, because this user is NOPASSWD.
 *
 * `-n` so the probe itself can never block. Asking someone for a password sudo
 * was never going to want is the surest way to teach them to type it at
 * anything.
 *
 * `-k` BECAUSE A CACHED CREDENTIAL IS NOT AN ANSWER TO THIS QUESTION
 * (memql#3586). The sudoers policy keys a cache record to the terminal or, with
 * no terminal, to the PARENT PROCESS ID -- and there is no terminal anywhere in
 * this wizard. So a credential cached for the extension host is one that no
 * capability script can use: each is its own process with its own record. The
 * probe used to find it, conclude no password was needed, skip the agent, and
 * leave every privileged step to discover otherwise and prompt on its own.
 *
 * `-k` WITH A COMMAND IS NOT DESTRUCTIVE, which is the reason it is safe to
 * probe with. From `man sudo`: used with a command it "will cause sudo to ignore
 * the user's cached credentials [...] and will not update" them. Used WITHOUT a
 * command it invalidates them, which is what `sudoAccepts` does deliberately and
 * this must not.
 */
export async function sudoRunsWithoutAsking(run: SudoRunner = defaultSudoRunner): Promise<boolean> {
  return (await run(["-n", "-k", "true"])) === 0;
}

const defaultSudoRunner: SudoRunner = (args) => runSudo(args, {});

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
