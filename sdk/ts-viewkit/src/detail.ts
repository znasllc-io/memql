// Detail rendering -- a recursive walk of a row's full nested shape.
//
// This is the cockpit's "Hybrid C" approach: no concept-specific rendering,
// so a concept works the day it is declared. The walk preserves the wire's
// nesting (payload / provenance / intrinsics stay distinct) rather than
// flattening, because flattening drops the intrinsics an operator came here
// to read.

import { h, text, type VNode } from "./vnode.js";
import type { RowLike } from "./types.js";

// The seen-set guards against a self-referential structure. Wire payloads are
// JSON and cannot be cyclic, but renderDetail is a public export and a caller
// can hand it any object -- unbounded recursion would hang the host process,
// which in a webview means a hung editor.
function renderValue(value: unknown, seen: Set<object>): VNode {
  if (value === null || value === undefined) {
    return h("span", { class: "vk-value vk-null" }, [text("null")]);
  }
  if (typeof value === "string") {
    return h("span", { class: "vk-value" }, [text(value)]);
  }
  if (typeof value === "number" || typeof value === "boolean") {
    return h("span", { class: "vk-value" }, [text(String(value))]);
  }
  if (typeof value === "object") {
    if (seen.has(value as object)) {
      return h("span", { class: "vk-value vk-cycle" }, [text("[circular]")]);
    }
    seen.add(value as object);
    const node = Array.isArray(value)
      ? renderEntries(value.map((v, i) => [`[${i}]`, v] as const), seen, "[]")
      : renderEntries(Object.entries(value as Record<string, unknown>), seen, "{}");
    seen.delete(value as object);
    return node;
  }
  // Functions and symbols cannot appear in wire data; render the type rather
  // than crashing if a caller passes one.
  return h("span", { class: "vk-value" }, [text(typeof value)]);
}

function renderEntries(
  entries: readonly (readonly [string, unknown])[],
  seen: Set<object>,
  emptyMarker: string,
): VNode {
  if (entries.length === 0) {
    return h("span", { class: "vk-value vk-empty-value" }, [text(emptyMarker)]);
  }
  return h(
    "div",
    { class: "vk-nested" },
    entries.map(([key, value]) =>
      h("div", { class: "vk-field" }, [
        h("span", { class: "vk-key" }, [text(key)]),
        renderValue(value, seen),
      ]),
    ),
  );
}

export function renderDetail(node: RowLike): VNode {
  const seen = new Set<object>([node]);
  return h("div", { class: "vk-detail" }, [
    renderEntries(Object.entries(node), seen, "{}"),
  ]);
}
