import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { markdownLinkProblem } from "../src/emailMarkdown.js";

// The engine's parser decides which Markdown links a text block may hold
// (component/compose/emaildoc/markdown.go); the editor refuses the same ones
// before an edit is sent. The corpus is shared: a Go test writes and checks
// the engine's verdicts, and this test holds the editor to them.
const CORPUS = "../../component/compose/emaildoc/testdata/markdown-links.json";

test("the editor refuses exactly the Markdown links the engine refuses", () => {
  const cases = JSON.parse(readFileSync(CORPUS, "utf8")) as { text: string; refused: boolean }[];
  assert.ok(cases.length >= 200, `${cases.length} cases`);
  assert.ok(cases.some(item => item.refused) && cases.some(item => !item.refused), "both verdicts are in the corpus");
  const disagreements = cases.filter(item => (markdownLinkProblem(item.text) !== undefined) !== item.refused).map(item => JSON.stringify(item));
  assert.deepEqual(disagreements, []);
});

test("the corpus is plain ASCII: every invisible character is written as an escape a reviewer can see", () => {
  const raw = readFileSync(CORPUS, "utf8");
  const hidden = [...raw].filter(character => character !== "\n" && (character < " " || character > "~"));
  assert.deepEqual(hidden.map(character => `U+${character.codePointAt(0)!.toString(16).toUpperCase().padStart(4, "0")}`), []);
  // The escapes stand for the characters the cases are about.
  const texts = (JSON.parse(raw) as { text: string }[]).map(item => item.text);
  for (const character of ["\u00a0", "\u0085", "\ufeff", "\u200b", "\u3000"]) assert.ok(texts.some(text => text.includes(character)), JSON.stringify(character));
});

test("a refusal says whether the label or the address is missing", () => {
  assert.equal(markdownLinkProblem("See [ ](https://acme.example)"), "label");
  assert.equal(markdownLinkProblem("See [our site](ftp://acme.example)"), "address");
  assert.equal(markdownLinkProblem("See [our site]()"), "address");
  assert.equal(markdownLinkProblem("See [our site](https://acme.example)"), undefined);
});
