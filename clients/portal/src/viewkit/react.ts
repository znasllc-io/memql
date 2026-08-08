// The React adapter for view-kit.
//
// view-kit (sdk/ts-viewkit) is DOM-free by design: it emits a VNode tree and
// can serialize that tree to an HTML string, and it takes no framework
// dependency so a VS Code webview and this portal can share one renderer.
// That leaves exactly one job for a React host -- walk the VNode tree into
// React elements.
//
// TWO THINGS THIS DELIBERATELY DOES NOT DO:
//
//   - dangerouslySetInnerHTML over renderToHtml(). It would work, and it
//     would throw away everything React is for: the rows would be outside the
//     reconciler, so selection state, click handling and incremental updates
//     would all have to be re-implemented against raw DOM. It also re-parses
//     HTML that was just serialized, which is a strictly worse way to build a
//     tree the walk below builds directly.
//   - fork view-kit into a React renderer. The whole point of the package is
//     that the VS Code panel and the portal render rows IDENTICALLY; a second
//     copy diverges on its first bug fix.
//
// The only real friction is attribute naming, handled by ATTRIBUTE_ALIASES.
// Everything else view-kit emits -- `data-*`, `class`, plain text -- maps
// across unchanged.

import { createElement, type ReactNode } from "react";
import type { VNode } from "@znasllc-io/memql-view-kit";

// HTML attribute -> React prop, for the handful where React does not accept
// the HTML spelling. `class` is the one view-kit actually emits today; `for`
// is here because it is the other member of the same tiny closed set and
// discovering it later would mean discovering it as a silent React warning in
// a console nobody is watching.
//
// `data-*` and `aria-*` are NOT in this map on purpose: React passes both
// through verbatim, and view-kit's interactivity contract is entirely
// data-attribute based (see sdk/ts-viewkit/src/vnode.ts).
const ATTRIBUTE_ALIASES: Readonly<Record<string, string>> = {
  class: "className",
  for: "htmlFor",
};

// vnodeToReact converts one node (and its subtree) to a ReactNode.
//
// `key` is the node's index among its siblings. view-kit lists are positional
// -- a row's identity lives in its `data-row-id` attribute, not in the tree
// shape -- and a rerender replaces the whole list, so an index key is honest
// here rather than the usual anti-pattern. Text nodes are returned as bare
// strings, which React accepts inside an array without a key.
export function vnodeToReact(node: VNode, key?: number): ReactNode {
  if ("text" in node) {
    return node.text;
  }

  const props: Record<string, unknown> = {};
  for (const [name, value] of Object.entries(node.attrs)) {
    props[ATTRIBUTE_ALIASES[name] ?? name] = value;
  }
  if (key !== undefined) {
    props["key"] = key;
  }

  // A void element (br/img/input/...) must be created with NO children
  // argument at all -- React throws on `<br>{[]}</br>`. view-kit already
  // gives void tags an empty children array, so the length check is what
  // keeps that true here.
  if (node.children.length === 0) {
    return createElement(node.tag, props);
  }
  return createElement(
    node.tag,
    props,
    node.children.map((child, index) => vnodeToReact(child, index)),
  );
}
