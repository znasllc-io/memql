import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

// ONE UPLOAD PATH (design D3): the provider is the only place in clients/os
// that speaks the artifact upload wire. Every surface -- desk drop, app
// upload button, drop-onto-window, drop-onto-folder -- imports the provider,
// so retry, resume, progress and refusal behavior cannot fork per surface.
//
// The REACHABLE POSITIVE: the provider file itself must match, so an empty
// offender list is evidence about the tree rather than about the regex.

const SRC = join(__dirname, "..", "..", "src");

function walk(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) out.push(...walk(path));
    else if (/\.(ts|tsx)$/.test(name)) out.push(path);
  }
  return out;
}

describe("one upload path", () => {
  it("only items/edgeUpload.ts names the artifact upload routes", () => {
    const offenders = new Map<string, string[]>();
    for (const path of walk(SRC)) {
      const text = readFileSync(path, "utf8");
      const hits: string[] = [];
      // The one-shot root, as the exact quoted path segment the provider
      // composes -- the content route (`/artifacts/<id>/content`, a GET) does
      // not match either pattern.
      if (text.includes('"_memql/artifacts"')) hits.push("one-shot root");
      if (text.includes("/uploads")) hits.push("session routes");
      if (hits.length > 0) offenders.set(path.slice(SRC.length + 1), hits);
    }
    expect([...offenders.keys()]).toEqual(["items/edgeUpload.ts"]);
    expect(offenders.get("items/edgeUpload.ts")).toEqual(["one-shot root", "session routes"]);
  });
});
