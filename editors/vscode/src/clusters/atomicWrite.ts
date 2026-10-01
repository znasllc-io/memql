import { spawn } from "node:child_process";
import { createHash } from "node:crypto";

export type RegistryWriter = (file: string, before: string | null, after: string) => Promise<boolean>;
let writer: RegistryWriter | undefined;

export function configureRegistryWriter(value: RegistryWriter): void { writer = value; }
export async function writeRegistry(file: string, before: string | null, after: string): Promise<boolean> {
  if (!writer) throw new Error("MemQL's registry helper is unavailable. Reinstall the desktop extension before changing cluster settings.");
  return writer(file, before, after);
}

// Node does not expose flock. Use the bundled native helper to share exactly
// the Cockpit lock/atomic replacement contract. No daemon or Cockpit dependency.
export function nativeRegistryWriter(binary: string): RegistryWriter {
  return (file, before, after) => new Promise((resolve, reject) => {
    const child = spawn(binary, ["registry-update"], { stdio: ["pipe", "pipe", "pipe"], timeout: 15_000 });
    let stdout = "";
    // Never surface stderr/stdin in notifications: this is a credential-bearing
    // file, and even an old helper must not accidentally echo its input.
    child.stderr.resume();
    child.stdout.on("data", (chunk: Buffer) => {
      stdout += chunk.toString();
      if (stdout.length > 4096) child.kill();
    });
    child.on("error", () => reject(new Error("Could not start MemQL's registry helper.")));
    child.stdin.on("error", () => {}); // exit/error below owns the result
    child.on("close", (code) => {
      if (code !== 0) { reject(new Error("Could not safely save cluster settings. Check file permissions or reinstall the desktop extension.")); return; }
      try {
        const result = JSON.parse(stdout) as { written?: boolean; conflict?: boolean };
        if (result.written === true) resolve(true);
        else if (result.conflict === true) resolve(false);
        else throw new Error("invalid result");
      } catch { reject(new Error("MemQL's registry helper returned an invalid result.")); }
    });
    child.stdin.end(JSON.stringify({ path: file, expected: before === null ? null : createHash("sha256").update(before).digest("hex"), data: after }));
  });
}
