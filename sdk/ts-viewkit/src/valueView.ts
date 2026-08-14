// The value viewer: one renderer for every surface that shows a value.
//
// A row's detail, a run's result and a construct's arguments are the same
// problem -- an arbitrary JSON value an operator has to read -- and until now
// they shared a renderer that could do none of the things that make one
// readable. This replaces it. Because it lives in view-kit rather than in the
// extension, the portal gets the same viewer, which is the reason view-kit
// exists.
//
// WHAT IT KEEPS, because all three are load-bearing:
//
//  1. THE WIRE'S NESTING IS PRESERVED, NEVER FLATTENED. Payload, provenance
//     and intrinsics stay distinct. Flatten them and a payload field named
//     `type` silently replaces the row's type, and a reader has no way to tell
//     which layer a value came from -- which is precisely what an operator
//     opened the detail pane to find out.
//  2. THE CYCLE GUARD, with its exact ANCESTOR-CHAIN semantics. This is a
//     public export and a caller can hand it any object; unbounded recursion
//     in a webview is a hung editor. The guard tracks the ancestor chain, not
//     "every object ever seen", so the same record referenced from two
//     siblings renders in full both times.
//  3. NO CONCEPT-SPECIFIC RENDERING, ANYWHERE. A value is rendered by its
//     shape and nothing else. That is what lets a concept declared five
//     minutes ago -- or promoted a minute ago -- render with no client change.
//
// WHAT IT ADDS, and how each survives a webview's Content-Security-Policy:
//
//  - COLLAPSE is native `<details>` / `<summary>`. No script at all, in any
//    host: the browser does it, it is keyboard-operable for free, and there is
//    no listener to forget to attach. Everything else here would have needed
//    one, so this is the single decision that makes the viewer work the moment
//    it is rendered.
//  - FILTER is an INPUT to the render, not a client-side behaviour. A host
//    with a search box re-renders with `filter` set; the matching is done here
//    where it is testable under `node --test`.
//  - TYPE BADGES, COUNTS, BREADCRUMB, TRUNCATION AND PAGING are all static
//    markup.
//  - COPY is the one affordance that needs a host. It is emitted only when
//    `copy` is set, because a button that does nothing is worse than no
//    button; a host turns it on in the same change that attaches the delegated
//    listener. The attribute contract is VALUE_VIEW_ATTRS.
//
// THE OUTPUT IS BOUNDED, whatever the input. `nodeBudget` is a hard depth-first
// ceiling on rendered nodes, and it is what makes "a 4MB payload must not hang
// the webview" a property rather than a hope: paging and string truncation
// bound the common shapes, and the budget bounds every other one, including
// the ones nobody has thought of. Truncation is always SAID, never silent.
//
// Refs: #3751 #3747

import { formatNumber } from "./format.js";
import { h, text, type VNode } from "./vnode.js";

/**
 * The data attributes a host reads back.
 *
 * view-kit never emits an inline handler -- a webview CSP forbids them, and the
 * data being rendered is untrusted. A host attaches ONE delegated listener and
 * reads these off the target.
 */
export const VALUE_VIEW_ATTRS = {
  /** The dotted path of the node an action targets. */
  path: "data-vk-path",
  /** What to copy: "value", "json" or "path". */
  copy: "data-vk-copy",
  /** On a "more entries" node: the path of the collection it belongs to. */
  more: "data-vk-more",
  /** On a "more entries" node: the index the next page starts at. */
  moreFrom: "data-vk-more-from",
  /** On a truncated string: the path of the string to show in full. */
  expandString: "data-vk-expand-string",
} as const;

/** Depth at which subtrees start collapsed. */
export const DEFAULT_EXPAND_DEPTH = 2;
/** Entries rendered per object or array before the rest become a "more" node. */
export const DEFAULT_PAGE_SIZE = 100;
/** Characters of a string rendered before it truncates. */
export const DEFAULT_MAX_STRING_LENGTH = 512;
/** The hard ceiling on rendered nodes. See the header. */
export const DEFAULT_NODE_BUDGET = 5_000;

export type ValueTypeName =
  | "string"
  | "number"
  | "boolean"
  | "null"
  | "object"
  | "array"
  | "unknown";

/**
 * A value's type, as the badge names it.
 *
 * The badge is not decoration: without it `"42"` and `42` render identically,
 * and an operator debugging a query that returns one when it should return the
 * other has no way to see the difference. `undefined` is named `null` because
 * it is rendered as `null` -- a badge disagreeing with the text beside it would
 * be worse than no badge.
 *
 * `unknown` covers functions and symbols. They cannot appear in wire data, but
 * this is a public export and a caller can pass anything; naming the type beats
 * throwing.
 */
export function valueTypeName(value: unknown): ValueTypeName {
  if (value === null || value === undefined) return "null";
  if (Array.isArray(value)) return "array";
  switch (typeof value) {
    case "string":
      return "string";
    case "number":
    case "bigint":
      return "number";
    case "boolean":
      return "boolean";
    case "object":
      return "object";
    default:
      return "unknown";
  }
}

export interface ValueViewOptions {
  /**
   * Where this value sits in a larger document, as path segments. Renders as
   * the breadcrumb and prefixes every node's copyable path. Array indices are
   * their own segment, spelled `[0]`.
   */
  path?: readonly string[];
  /** Depth at which subtrees start collapsed. Default DEFAULT_EXPAND_DEPTH. */
  expandDepth?: number;
  /** Case-insensitive substring matched against keys and scalar values. */
  filter?: string;
  /** Default DEFAULT_MAX_STRING_LENGTH. */
  maxStringLength?: number;
  /** Default DEFAULT_PAGE_SIZE. */
  pageSize?: number;
  /** Default DEFAULT_NODE_BUDGET. */
  nodeBudget?: number;
  /**
   * Emit the copy affordances. OFF by default: they do nothing until a host
   * attaches the delegated listener described by VALUE_VIEW_ATTRS, and a
   * button that does nothing is worse than no button.
   */
  copy?: boolean;
}

interface Walk {
  /** The ANCESTOR chain, not every object ever seen. See the header. */
  seen: Set<object>;
  /** Mutable, shared across the whole render: the hard node ceiling. */
  budget: { left: number };
  expandDepth: number;
  maxStringLength: number;
  pageSize: number;
  copy: boolean;
  /** Lowercased; "" when no filter is active. */
  needle: string;
}

/**
 * Render a value.
 *
 * The root's entries render directly rather than inside a collapsible node of
 * their own: the root IS the thing being looked at, and putting it behind a
 * disclosure that starts closed would hide the whole view.
 */
export function renderValueView(value: unknown, options: ValueViewOptions = {}): VNode {
  const path = [...(options.path ?? [])];
  const walk: Walk = {
    seen: new Set<object>(),
    budget: { left: Math.max(0, options.nodeBudget ?? DEFAULT_NODE_BUDGET) },
    expandDepth: options.expandDepth ?? DEFAULT_EXPAND_DEPTH,
    maxStringLength: options.maxStringLength ?? DEFAULT_MAX_STRING_LENGTH,
    pageSize: Math.max(1, options.pageSize ?? DEFAULT_PAGE_SIZE),
    copy: options.copy === true,
    needle: (options.filter ?? "").trim().toLowerCase(),
  };

  const children: VNode[] = [];
  if (path.length > 0) {
    children.push(h("div", { class: "vk-vv-crumb" }, [text(joinPath(path))]));
  }

  if (isBranch(value)) {
    walk.seen.add(value as object);
    const body = renderEntries(
      entriesOf(value),
      0,
      path,
      walk,
      false,
      Array.isArray(value) ? "item" : "key",
    );
    walk.seen.delete(value as object);
    children.push(
      body.length === 0
        ? h("div", { class: "vk-vv-nothing" }, [text(nothingText(walk.needle, value))])
        : h("div", { class: "vk-vv-tree" }, body),
    );
  } else {
    children.push(h("div", { class: "vk-vv-tree" }, [renderScalarRow("", value, path, walk)]));
  }

  return h("div", { class: "vk-vv" }, children);
}

/** What an empty tree says, which differs by why it is empty. */
function nothingText(needle: string, value: unknown): string {
  if (needle !== "") return "No key or value matches the filter.";
  return Array.isArray(value) ? "[] (empty)" : "{} (empty)";
}

// ---------------------------------------------------------------------------
// the walk
// ---------------------------------------------------------------------------

function isBranch(value: unknown): boolean {
  return value !== null && typeof value === "object";
}

function entriesOf(value: unknown): (readonly [string, unknown])[] {
  return Array.isArray(value)
    ? value.map((v, i) => [`[${i}]`, v] as const)
    : Object.entries(value as Record<string, unknown>);
}

/**
 * Render a collection's entries, honouring the filter, the page size and the
 * budget -- in that order, because each only matters for what the previous one
 * let through.
 *
 * `forced` means an ancestor's KEY matched the filter, so everything beneath it
 * renders whole. Searching for `lineage` and getting the key with none of its
 * contents would be a worse answer than no filter at all.
 */
function renderEntries(
  entries: readonly (readonly [string, unknown])[],
  depth: number,
  path: readonly string[],
  walk: Walk,
  forced: boolean,
  noun: "item" | "key",
): VNode[] {
  const out: VNode[] = [];
  let shown = 0;
  let index = 0;

  for (const [key, value] of entries) {
    if (walk.budget.left <= 0) {
      out.push(budgetNode(entries.length - index));
      return out;
    }
    if (shown >= walk.pageSize) {
      // The remainder is NAMED, with the index it resumes at, so a host can
      // ask for the next page and a reader who has no host still knows exactly
      // how much they are not being shown.
      out.push(moreNode(entries.length - index, index, path, noun));
      return out;
    }
    const node = renderNode(key, value, depth, path, walk, forced);
    if (node !== null) {
      out.push(node);
      shown += 1;
    }
    index += 1;
  }
  return out;
}

/**
 * One entry, or null when the filter excludes it.
 *
 * A branch survives the filter if its own key matches OR any descendant does --
 * which falls out of rendering the children first and keeping the branch only
 * when something came back. That is also what reveals a match "with its
 * ancestors" without a second pass that could disagree with this one.
 */
function renderNode(
  key: string,
  value: unknown,
  depth: number,
  parentPath: readonly string[],
  walk: Walk,
  forced: boolean,
): VNode | null {
  const path = [...parentPath, key];
  const keyMatches = walk.needle !== "" && key.toLowerCase().includes(walk.needle);
  const active = forced || keyMatches;

  if (!isBranch(value)) {
    if (walk.needle !== "" && !active && !scalarMatches(value, walk.needle)) return null;
    walk.budget.left -= 1;
    return renderScalarRow(key, value, path, walk);
  }

  const object = value as object;
  if (walk.seen.has(object)) {
    walk.budget.left -= 1;
    return h("div", { class: "vk-vv-node" }, [
      keyNode(key),
      typeBadge(valueTypeName(value)),
      h("span", { class: "vk-vv-value vk-vv-cycle" }, [text("[circular]")]),
    ]);
  }

  const entries = entriesOf(value);
  if (entries.length === 0) {
    if (walk.needle !== "" && !active) return null;
    walk.budget.left -= 1;
    return h("div", { class: "vk-vv-node" }, [
      keyNode(key),
      typeBadge(valueTypeName(value)),
      h("span", { class: "vk-vv-value vk-vv-empty" }, [
        text(Array.isArray(value) ? "[]" : "{}"),
      ]),
      ...actions(path, walk, "branch"),
    ]);
  }

  walk.budget.left -= 1;
  walk.seen.add(object);
  const children = renderEntries(
    entries,
    depth + 1,
    path,
    walk,
    active,
    Array.isArray(value) ? "item" : "key",
  );
  walk.seen.delete(object);

  if (children.length === 0 && walk.needle !== "" && !active) return null;

  // A filter that matched deeper down forces the branch OPEN whatever the
  // depth threshold says: revealing a match inside a collapsed node is not
  // revealing it.
  const open = active || walk.needle !== "" || depth < walk.expandDepth;
  const attrs: Record<string, string> = {
    class: keyMatches ? "vk-vv-branch vk-vv-match" : "vk-vv-branch",
    [VALUE_VIEW_ATTRS.path]: joinPath(path),
  };
  // Emitted ONLY when open, and spelled "open" rather than "" or "true". An
  // empty string is falsy to React and would render every node collapsed;
  // "false" is a non-empty string and truthy to React, so it would render
  // every node OPEN. Omitting the attribute is the only spelling of "closed"
  // that means the same thing in a serialized string and in React.
  if (open) attrs["open"] = "open";

  return h("details", attrs, [
    h("summary", { class: "vk-vv-summary" }, [
      keyNode(key),
      typeBadge(valueTypeName(value)),
      h("span", { class: "vk-vv-count" }, [text(collectionSummary(value, entries.length))]),
      ...actions(path, walk, "branch"),
    ]),
    h("div", { class: "vk-vv-children" }, children),
  ]);
}

/** `{...} 14 keys` / `[...] 1,284 items` -- the marker plus what is inside it. */
function collectionSummary(value: unknown, count: number): string {
  return Array.isArray(value)
    ? `[...] ${formatNumber(count)} ${count === 1 ? "item" : "items"}`
    : `{...} ${formatNumber(count)} ${count === 1 ? "key" : "keys"}`;
}

function renderScalarRow(
  key: string,
  value: unknown,
  path: readonly string[],
  walk: Walk,
): VNode {
  const type = valueTypeName(value);
  const children: VNode[] = [];
  if (key !== "") children.push(keyNode(key));
  children.push(typeBadge(type));

  if (type === "null") {
    children.push(h("span", { class: "vk-vv-value vk-vv-null" }, [text("null")]));
  } else if (typeof value === "string") {
    children.push(...stringValue(value, path, walk));
  } else if (type === "unknown") {
    children.push(h("span", { class: "vk-vv-value" }, [text(typeof value)]));
  } else {
    children.push(h("span", { class: "vk-vv-value" }, [text(String(value))]));
  }

  children.push(...actions(path, walk, "scalar"));
  return h("div", { class: "vk-vv-node", [VALUE_VIEW_ATTRS.path]: joinPath(path) }, children);
}

/**
 * A string, truncated at the limit with the remainder counted.
 *
 * The count is the point. "..." alone tells a reader something was cut and
 * nothing about whether it was three characters or three megabytes, which is
 * the difference between shrugging and going to look.
 */
function stringValue(value: string, path: readonly string[], walk: Walk): VNode[] {
  if (value.length <= walk.maxStringLength) {
    return [h("span", { class: "vk-vv-value" }, [text(value)])];
  }
  const rest = value.length - walk.maxStringLength;
  return [
    h("span", { class: "vk-vv-value" }, [text(value.slice(0, walk.maxStringLength))]),
    h(
      "span",
      {
        class: "vk-vv-truncated",
        [VALUE_VIEW_ATTRS.expandString]: joinPath(path),
      },
      [text(`... ${formatNumber(rest)} more characters`)],
    ),
  ];
}

function keyNode(key: string): VNode {
  return h("span", { class: "vk-vv-key" }, [text(key)]);
}

/**
 * Spelled out per type rather than composed as `vk-vv-type-${type}`.
 *
 * styles.test.ts scans this source for class literals and requires a rule for
 * each; an interpolated name reads as `vk-vv-type-` with nothing after it, so
 * the guard could neither find the rule nor tell anyone which one was missing.
 * Six literals cost a table and keep the stylesheet contract checkable.
 */
const TYPE_CLASS: Readonly<Record<ValueTypeName, string>> = {
  string: "vk-vv-type vk-vv-type-string",
  number: "vk-vv-type vk-vv-type-number",
  boolean: "vk-vv-type vk-vv-type-boolean",
  null: "vk-vv-type vk-vv-type-null",
  object: "vk-vv-type vk-vv-type-object",
  array: "vk-vv-type vk-vv-type-array",
  unknown: "vk-vv-type vk-vv-type-unknown",
};

function typeBadge(type: ValueTypeName): VNode {
  return h("span", { class: TYPE_CLASS[type] }, [text(type)]);
}

function moreNode(
  remaining: number,
  from: number,
  path: readonly string[],
  noun: "item" | "key",
): VNode {
  return h(
    "div",
    {
      class: "vk-vv-more",
      [VALUE_VIEW_ATTRS.more]: joinPath(path),
      [VALUE_VIEW_ATTRS.moreFrom]: String(from),
    },
    [text(`... ${formatNumber(remaining)} more ${remaining === 1 ? noun : `${noun}s`}`)],
  );
}

/**
 * The node that says the render hit its ceiling.
 *
 * SAID, never silent. A viewer that quietly stopped would be indistinguishable
 * from a value that genuinely ended there, and the reader would draw a
 * conclusion from data they were not shown.
 */
function budgetNode(remaining: number): VNode {
  return h("div", { class: "vk-vv-budget" }, [
    text(
      `... ${formatNumber(remaining)} more not shown: this value is too large to render in full.`,
    ),
  ]);
}

function actions(path: readonly string[], walk: Walk, kind: "scalar" | "branch"): VNode[] {
  if (!walk.copy) return [];
  const dotted = joinPath(path);
  const buttons: VNode[] =
    kind === "scalar"
      ? [copyButton("value", "value", dotted), copyButton("path", "path", dotted)]
      : [copyButton("json", "JSON", dotted), copyButton("path", "path", dotted)];
  return [h("span", { class: "vk-vv-actions" }, buttons)];
}

function copyButton(what: string, label: string, dotted: string): VNode {
  return h(
    "button",
    {
      type: "button",
      class: "vk-vv-copy",
      title: `Copy ${what}`,
      [VALUE_VIEW_ATTRS.copy]: what,
      [VALUE_VIEW_ATTRS.path]: dotted,
    },
    [text(label)],
  );
}

/**
 * Path segments as one dotted string, with array indices attached rather than
 * dotted: `payload.phases[0].name`. That is the spelling a reader can paste
 * into a query, which is the only reason to offer a copyable path at all.
 */
export function joinPath(segments: readonly string[]): string {
  let out = "";
  for (const segment of segments) {
    if (out === "" || segment.startsWith("[")) out += segment;
    else out += `.${segment}`;
  }
  return out;
}

/** Whether a scalar's rendered text contains the (lowercased) needle. */
function scalarMatches(value: unknown, needle: string): boolean {
  if (value === null || value === undefined) return "null".includes(needle);
  return String(value).toLowerCase().includes(needle);
}
