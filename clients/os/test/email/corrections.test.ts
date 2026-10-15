import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
const root = join(__dirname, "../../src/apps/email");
function walk(dir: string): string[] { return readdirSync(dir, { withFileTypes: true }).flatMap(f => f.isDirectory() ? walk(join(dir, f.name)) : /\.tsx?$/.test(f.name) ? [join(dir, f.name)] : []); }
const sources = walk(root).map(p => readFileSync(p, "utf8"));
const text = sources.join("\n");
const withoutComments = text.replace(/\/\*[\s\S]*?\*\/|^\s*\/\/.*$/gm, "");
describe("Email corrections", () => {
  it.each([
    [/JourneyTrail|os-journey-trail/, "JourneyTrail"],
    [/<(?:Button|button)\b[^>]*\bdisabled[=\s>]|disabled:\s*true/, '<Button disabled={true} />'],
    [/Updates automatically|Reading the|Checking template/, 'Updates automatically'],
    [/3am|silence, not|Productivity Tools/, 'Productivity Tools'],
    [/os-campaign-actions/, 'os-campaign-actions'],
    [/InfoDetail/, 'InfoDetail'],
    [/BASE_MERGE_TAGS|mergeTagsFor|export function insertAt/, 'mergeTagsFor'],
  ])("does not carry forbidden presentation %s", (pattern, planted) => { expect((pattern as RegExp).test(String(planted))).toBe(true); expect(withoutComments.match(pattern as RegExp)).toBeNull(); });
  it("keeps only used Email-owned styles", () => { const css = readFileSync(join(root, "email.css"), "utf8"); const names = [...new Set(css.match(/\.(?:email|os-send|os-campaign)-[a-z-]+/g) ?? [])]; expect(names.filter(n => !text.includes(n.slice(1)))).toEqual([]); });
  it("uses the documented unresolved tag answer", () => { const actions = readFileSync(join(root, "actions.ts"), "utf8"); expect(actions).toContain('unresolvedTagsFrom(result.rows()[0]'); expect(actions).not.toContain('["unresolved", "unresolvedTags"]'); });
});
