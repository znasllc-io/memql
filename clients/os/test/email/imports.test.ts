import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

// NO SECTION IMPORTS THE APP THAT IMPORTS IT.
//
// `EmailApp.tsx` imports every section, so a section that imports a VALUE from
// `EmailApp.tsx` (`useOpenRecord`, `useOpenSettings`, `useOpenRequest`) closes a
// runtime import cycle: whichever module the bundler reaches first sees the
// other half-initialised, and the failure is a hook that is `undefined` on one
// load order and fine on another. The request model lives in `openRequest.ts`
// for exactly that reason (its header says so), and every file under
// `apps/email/` takes the openers from there.
//
// The REACHABLE POSITIVE: the file that does re-export them is found, so an
// empty offender list is evidence about the tree rather than about the pattern.

const EMAIL = join(__dirname, "..", "..", "src", "apps", "email");

function walk(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) out.push(...walk(path));
    else if (/\.(ts|tsx)$/.test(name)) out.push(path);
  }
  return out;
}

/** An import of the app root, from any depth: static, re-exported, or dynamic. */
const IMPORTS_THE_APP_ROOT = /(?:\bfrom\s*|\bimport\s*\(\s*|\bimport\s+)["'][^"']*\/EmailApp(?:\.tsx?)?["']/;

describe("the Email app's files", () => {
  it("never import the app root", () => {
    const offenders = walk(EMAIL)
      .filter((path) => !path.endsWith("EmailApp.tsx"))
      .filter((path) => IMPORTS_THE_APP_ROOT.test(readFileSync(path, "utf8")))
      .map((path) => path.slice(EMAIL.length + 1));
    expect(offenders).toEqual([]);
  });

  it("can tell an import of the app root from one of its neighbours", () => {
    for (const text of [
      'import { useOpenRecord } from "../EmailApp";',
      "import { useOpenSettings } from './EmailApp';",
      'export { useOpenRecord } from "../../email/EmailApp";',
      'const root = await import("./EmailApp.tsx");',
      'import "./EmailApp";',
    ]) {
      expect(IMPORTS_THE_APP_ROOT.test(text)).toBe(true);
    }
    for (const text of [
      'import { useOpenRecord } from "./openRequest";',
      'import { useOpenRecord } from "../openRequest";',
      'import { EmailAppProps } from "./EmailAppProps";',
      "// the app root, EmailApp, imports every section",
    ]) {
      expect(IMPORTS_THE_APP_ROOT.test(text)).toBe(false);
    }
  });

  it("find the openers in the module they live in", () => {
    const holders = walk(EMAIL).filter((path) => /export function useOpenRecord\b/.test(readFileSync(path, "utf8")));
    expect(holders.map((path) => path.slice(EMAIL.length + 1))).toEqual(["openRequest.ts"]);
  });
});
