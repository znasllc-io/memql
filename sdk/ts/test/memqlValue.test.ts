// renderMemQLValue's object-key quoting (epic memql#4349).
//
// A label key is user-chosen text, not a schema field name, so it routinely
// carries a hyphen -- the workers runbook's own example is `has-blender=true`.
// Rendered bare, that key lexes as `has`, `-`, `blender` and the call fails to
// parse in a caller that did nothing wrong. The parser's parseObject takes a
// quoted key as its FIRST branch, so quoting is the grammar's answer for the
// case rather than a way around it.
//
// This mirrors sdk/go/client/object_key_render_test.go. The two renderers are
// meant to produce identical strings, and a key that quotes on one side and
// not the other is the drift that would make them disagree.

import test from "node:test";
import assert from "node:assert/strict";

import { renderMemQLValue } from "../src/client/memqlValue.js";

test("identifier keys stay bare and everything else is quoted", () => {
  assert.equal(
    renderMemQLValue({ "has-blender": "true", os: "darwin" }),
    '{"has-blender": "true", os: "darwin"}',
  );
  assert.equal(renderMemQLValue({ _private: 1, a1: 2 }), "{_private: 1, a1: 2}");
  assert.equal(renderMemQLValue({ "os.name": "x" }), '{"os.name": "x"}');
  assert.equal(renderMemQLValue({ "2fast": true }), '{"2fast": true}');
});

test("keys sort on their RAW name, before quoting", () => {
  // Quoting is a rendering decision and must not move a key: sorting the
  // quoted forms would put every hyphenated key first, and the Go renderer
  // (which sorts raw and then quotes) would disagree with this one on the same
  // input. The two are meant to produce identical strings.
  assert.equal(
    renderMemQLValue({ zeta: 1, alpha: 2, "m-key": 3 }),
    '{alpha: 2, "m-key": 3, zeta: 1}',
  );
});
