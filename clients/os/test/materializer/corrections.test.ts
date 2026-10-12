import { readFileSync, readdirSync } from "node:fs";
import { expect, it } from "vitest";
const root = process.cwd() + "/src/apps/materializer/";
const files = readdirSync(root).filter((name) => /\.tsx?$/.test(name));
const source = files
  .map((name) => readFileSync(root + name, "utf8"))
  .join("\n");
const guards = {
  "has no disabled act": /<(?:Button|ActionBar|RecordRow)\b[^>]*\bdisabled\s*=/,
  "has no raw select": /<select\b/,
  "uses switches for preferences": /import\s*\{[^}]*\bCheck\b/,
  "does not parse entity ids": /\bidTail\b|\.split\(["']:["']\)/,
  "has no old composer": /\bos-mz(?:\b|-)/,
};
const plants = [
  "<Button disabled={true}/>",
  "<select/>",
  'import {Check} from "kit"',
  'id.split(":")',
  "os-mz-columns",
];
Object.entries(guards).forEach(([name, pattern], i) =>
  it(name, () => {
    expect(pattern.test(plants[i]!)).toBe(true);
    expect(source).not.toMatch(pattern);
  }),
);
it("uses every local stylesheet class", () => {
  const css = readFileSync(root + "materializer.css", "utf8");
  const names = [...css.matchAll(/\.(mz-[\w-]+)/g)].map((match) => match[1]);
  for (const name of names) expect(source, `unused ${name}`).toContain(name);
});
it("removes the old composer CSS from the shared stylesheet", () =>
  expect(
    readFileSync(process.cwd() + "/src/styles/index.css", "utf8"),
  ).not.toContain("os-mz"));
