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

test("a refusal says whether the label or the address is missing", () => {
  assert.equal(markdownLinkProblem("See [ ](https://acme.example)"), "label");
  assert.equal(markdownLinkProblem("See [our site](ftp://acme.example)"), "address");
  assert.equal(markdownLinkProblem("See [our site]()"), "address");
  assert.equal(markdownLinkProblem("See [our site](https://acme.example)"), undefined);
});
