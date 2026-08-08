// flattenForList projects a wire node into the flat shape a display card
// names its fields on. The wire keeps payload nested; the display card names
// payload fields directly, so the list needs the merge. Detail rendering
// deliberately does NOT flatten -- it shows the nesting.
//
// Row intrinsics always win over a payload field of the same name. memQL
// concepts routinely carry payload fields, so a payload `id` (or
// `createdAt`, `type`, `concept`, ...) colliding with the row intrinsic of
// the same name is reachable, not theoretical -- and `data-row-id`
// (rowList.ts) is sourced from whatever ends up at `out.id`, so a payload
// value winning there means the row's subsequent detail fetch resolves the
// wrong row, or none at all. Intrinsics are copied into the projection
// first; a payload key is only added if that name isn't already taken.
//
// Lives under src/state/ (not src/webview/) because it holds no VS Code types
// at all: src/views/ and src/webview/ are the adapter layers allowed to
// import `vscode`, and everything carrying logic stays out of them so it
// remains unit-testable under bare `node --test`. The rule is enforced
// mechanically -- see cmd/memql-lsp/vscodeimportrule_test.go.
import type { Row } from "@znasllc-io/memql-sdk-core/client";

export function flattenForList(node: Row): Row {
  const payload = node.payload;
  const isFlattenable = payload !== null && typeof payload === "object" && !Array.isArray(payload);

  const out: Row = {};
  for (const [k, v] of Object.entries(node)) {
    // When payload is flattenable its own key is dropped here and merged
    // back in below (with the collision guard); when it isn't, it passes
    // through like any other field so a non-object payload is preserved
    // rather than silently dropped.
    if (k === "payload" && isFlattenable) continue;
    out[k] = v;
  }

  if (isFlattenable) {
    for (const [pk, pv] of Object.entries(payload as Record<string, unknown>)) {
      if (!(pk in out)) out[pk] = pv;
    }
  }

  return out;
}
