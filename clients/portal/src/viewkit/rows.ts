// Projecting a wire node into the flat shape view-kit's row list reads.
//
// The wire keeps a node's payload NESTED under `payload`, alongside the row
// intrinsics (id / concept / type / createdAt / createdBy / schema). A
// @displayCard hint names payload fields DIRECTLY ("name", "status"), so the
// list needs the merge. Detail rendering deliberately does not flatten --
// renderDetail shows the nesting, which is what an operator came to read.
//
// ROW INTRINSICS WIN over a payload field of the same name. memQL concepts
// routinely carry a payload `id`, `createdAt` or `type`, and view-kit sources
// `data-row-id` from whatever lands at `id` -- so a payload value winning
// there means a subsequent row fetch resolves the wrong row, or none at all.
// Intrinsics are copied first; a payload key is added only if the name is
// still free.
//
// This mirrors editors/vscode/src/webview/rowProjection.ts. Duplicated rather
// than shared because the two live in different packages and the shared home
// would be view-kit -- which is deliberately structural about its input
// (ConceptLike / RowLike) and takes no dependency on the SDK's wire types.

import type { Row } from "@znasllc-io/memql-sdk-core/client";

export function flattenForList(node: Row): Row {
  const payload = node["payload"];
  const flattenable =
    payload !== null && typeof payload === "object" && !Array.isArray(payload);

  const out: Row = {};
  for (const [key, value] of Object.entries(node)) {
    // When payload is flattenable its own key is dropped here and merged back
    // in below. When it is NOT (a scalar payload, or null) it passes through
    // like any other field, so it is preserved rather than silently dropped.
    if (key === "payload" && flattenable) continue;
    out[key] = value;
  }

  if (flattenable) {
    for (const [key, value] of Object.entries(payload as Record<string, unknown>)) {
      if (!(key in out)) out[key] = value;
    }
  }

  return out;
}
