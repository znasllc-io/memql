import { test } from "node:test";
import assert from "node:assert/strict";
import { renderMarkdown, markdownAnchor, anchorStillMatches } from "../src/markdown.js";

test("renders Markdown syntax as content while retaining passage source ranges", () => {
  const html = renderMarkdown("# A title\n\nReview **this section**.\n\n| A | B |\n| - | - |\n| 1 | 2 |\n");
  assert.match(html, /<h1[^>]*>A title<\/h1>/);
  assert.match(html, /<strong>this section<\/strong>/);
  assert.match(html, /data-start-line="2" data-end-line="3"/);
  assert.match(html, /<table/);
});
test("document content cannot execute scripts, navigate commands, or fetch tracking images", () => {
  const html = renderMarkdown('<script>alert(1)</script>\n\n[Run](command:evil) ![Tracking](https://tracker.example/pixel)\n\n[Site](https://example.com)');
  assert.ok(!html.includes("<script>"));
  assert.ok(!html.includes("<img"));
  assert.ok(!html.includes('href="command:'));
  assert.ok(!html.includes('src="https:'));
  assert.match(html, /data-external="https:\/\/example.com"/);
});
test("generated front matter stays in source and preserves existing passage coordinates", () => {
  const source = '---\ntitle: "Draft"\nauthor: "Person"\n---\n\n# My document\n\nReview **this**.\n';
  const html = renderMarkdown(source);
  assert.ok(!html.includes("author:"));
  assert.ok(!html.includes("Draft"));
  assert.match(html, /<h1 data-block-id="2" data-start-line="5" data-end-line="6"[^>]*>My document/);
  assert.match(html, /data-block-id="3" data-start-line="7" data-end-line="8"/);
  const anchor = markdownAnchor(source, {startLine:7,endLine:8,quote:"Review this."});
  assert.equal(anchor.sourceQuote,"Review **this**.");
  assert.match(renderMarkdown("---\nA real heading\n---\n"), /A real heading/);
  assert.match(renderMarkdown("---\ntitle: Unclosed metadata\n"), /Unclosed metadata/);
});
test("passage anchors capture source plus rendered quote and become stale after edits", () => {
  const source = "# Title\n\nReview **this section**.\n";
  const anchor = markdownAnchor(source, { startLine: 2, endLine: 3, quote: "this section" });
  assert.equal(anchor.sourceQuote, "Review **this section**.");
  assert.equal(anchor.quote, "this section");
  assert.ok(anchorStillMatches(source, anchor));
  assert.ok(!anchorStillMatches("New opening\n" + source, anchor));
  assert.throws(() => markdownAnchor(source, { startLine: -1, endLine: 2, quote: "fake" }));
  assert.throws(() => markdownAnchor(source, { startLine: 2, endLine: 999, quote: "fake" }));
});

test("extension metadata retains a verified section or passage anchor", () => {
  const source = "# Guide\n\n## Examples\n\nTry **this**.\n";
  const section = markdownAnchor(source, {startLine:2,endLine:3,quote:"Examples",intent:"extend",scope:"section"});
  assert.equal(section.intent,"extend");assert.equal(section.scope,"section");assert.equal(section.sourceQuote,"## Examples");assert.deepEqual(section.sectionPath,["Guide","Examples"]);
  const passage = markdownAnchor(source, {startLine:4,endLine:5,quote:"this",intent:"extend"});
  assert.equal(passage.intent,"extend");assert.equal(passage.scope,undefined);assert.equal(passage.sourceQuote,"Try **this**.");
});
