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
