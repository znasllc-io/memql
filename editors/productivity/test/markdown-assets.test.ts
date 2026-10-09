import { test } from "node:test";
import assert from "node:assert/strict";
import { imageMime, imageMarkdown, markdownImageURI, MAX_IMAGE_BYTES } from "../src/markdownAssets.js";
import { renderMarkdown } from "../src/markdown.js";
import { markdownPage } from "../src/markdownPage.js";

test("only same-cluster or document-directory raster images resolve", () => {
  const base = "memql-file://cluster.example/artifacts/document/paper.md";
  const image = "memql-file://cluster.example/artifacts/picture/Figure%201.png";
  assert.equal(markdownImageURI(image, base), image);
  for (const source of [image.replace("cluster.example", "other.example"), "https://tracker.example/pixel.png", "data:image/png;base64,x", "javascript:alert(1)", image + "?token=x", image.replace(".png", ".svg"), "memql-file://cluster.example/templates/picture/figure.png"]) assert.equal(markdownImageURI(source, base), undefined, source);
  assert.equal(markdownImageURI("figures/a.png", "file:///docs/paper.md"), "file:///docs/figures/a.png");
  for (const source of ["../private/a.png", "file:///private/a.png", "figures/%2e%2e/%2e%2e/private/a.png", "figures/%2f..%2f..%2fprivate/a.png"]) assert.equal(markdownImageURI(source, "file:///docs/paper.md"), undefined, source);
});

test("image uploads reject active content, mismatched formats, and oversized bytes", () => {
  const png = new Uint8Array(Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg==", "base64"));
  assert.equal(imageMime("figure.PNG", png), "image/png");
  assert.throws(() => imageMime("figure.jpg", png));
  assert.throws(() => imageMime("figure.png", new TextEncoder().encode('<svg onload="evil()"/>')));
  assert.throws(() => imageMime("figure.png", new Uint8Array(MAX_IMAGE_BYTES + 1)));
  const huge = new Uint8Array(png); new DataView(huge.buffer).setUint32(16, 100000);
  new DataView(huge.buffer).setUint32(20, 100000);
  assert.throws(() => imageMime("figure.png", huge), /24 million/);
});

test("renderer uses the authorized image resolver and escapes attributes", () => {
  const uri = "memql-file://cluster.example/artifacts/figure/a.png";
  const html = renderMarkdown(imageMarkdown('A [figure] "with" detail', uri), {image: source => source === uri ? 'https://resource.example/image?a="quoted"' : undefined});
  assert.match(html, /<img src="https:\/\/resource.example\/image\?a=&quot;quoted&quot;"/);
  assert.match(html, /loading="lazy" decoding="async"/);
  assert.match(html, /alt="A \[figure\] &quot;with&quot; detail"/);
  assert.doesNotMatch(renderMarkdown(`![Remote](https://tracker.example/image.png)`), /<img/);
  const page = markdownPage("Paper", "bundle", "nonce", "https://resource.example");
  assert.match(page, /img-src https:\/\/resource.example;/);
  assert.doesNotMatch(page, /img-src https: |img-src \*/);
});

test("research formatting includes footnotes, inert task lists, nested blocks and stable section links", () => {
  const source = "# Findings\n\n[Methods](#methods) and a citation.[^source]\n\n## Methods\n\n- [x] Sourced\n- [ ] Replicate\n  - Nested **item**\n\n> Evidence with `code` and ~~superseded~~ estimates.\n\n```python\nprint(1 < 2)\n```\n\n| Approach | Size |\n| :-- | --: |\n| A | 20 |\n\n## Methods\n\n[^source]: Primary [study](https://example.org/paper).\n";
  const html = renderMarkdown(source);
  assert.match(html, /data-heading-anchor="methods"/); assert.match(html, /data-heading-anchor="methods-1"/);
  assert.match(html, /data-internal="methods"/); assert.match(html, /footnote-ref/); assert.match(html, /id="fn1"/);
  assert.match(html, /type="checkbox"/); assert.match(html, /disabled=""/); assert.doesNotMatch(html, /contenteditable/);
  assert.match(html, /<blockquote/); assert.match(html, /language-python/); assert.match(html, /<s>superseded<\/s>/); assert.match(html, /<table/);
});
