// A concept's rows as a list a keyboard can use.
//
// view-kit renders a row list as `<li data-row-id>`: the concept's own display
// card, no concept-specific code, and the same markup MemQL OS draws. What it
// does not render is a way to reach a row without a mouse -- an `<li>` takes no
// focus and answers no key. So each row is re-wrapped here in a real
// `<button>`: Tab reaches it, Enter and Space press it, and the page runtime
// posts the button's `data-act` like any other act. The display card, the
// selection marker and every class view-kit's stylesheet dresses are kept, so
// the list looks the same and is shared markup underneath.
//
// ONE WRAPPER FOR THREE LISTS: the Concept page, a run Result's rows and the
// automation form's row picker all pick a row, and all three were mouse-only.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import {
  renderRowList,
  renderToHtml,
  h,
  type ConceptLike,
  type VNode,
} from "@znasllc-io/memql-view-kit";

export interface RowListInput {
  /** Rows, already projected for display (state/rowProjection.ts). */
  rows: readonly Record<string, unknown>[];
  concept: ConceptLike;
  selectedRowId?: string;
  /** The act a row posts, with the row's id as its `value`. */
  act: string;
  /** Extra `data-*` attributes on every row, keys without the prefix. */
  data?: Readonly<Record<string, string>>;
  /** The list's accessible name ("Rows of space"). */
  label: string;
}

function isElement(node: VNode): node is { tag: string; attrs: Record<string, string>; children: VNode[] } {
  return "tag" in node;
}

/** The rows as buttons. Undefined for an empty list: an empty state is the caller's to say. */
export function rowListHtml(input: RowListInput): string | undefined {
  if (input.rows.length === 0) return undefined;
  const list = renderRowList([...input.rows], input.concept, input.selectedRowId);
  if (!isElement(list)) return undefined;
  const items = list.children.filter(isElement).map((li) => {
    const id = li.attrs["data-row-id"] ?? "";
    const attrs: Record<string, string> = {
      type: "button",
      class: li.attrs.class ?? "vk-row",
      "data-act": input.act,
      "data-value": id,
      "data-row-id": id,
    };
    for (const [key, value] of Object.entries(input.data ?? {})) attrs[`data-${key}`] = value;
    if (li.attrs["data-selected"] === "true") {
      attrs["data-selected"] = "true";
      attrs["aria-current"] = "true";
    }
    return h("li", { class: "rowlist-item" }, [h("button", attrs, li.children)]);
  });
  return renderToHtml(h("ul", { class: `${list.attrs.class ?? "vk-rows"} rowlist`, "aria-label": input.label }, items));
}

/**
 * The button reset that makes a row button look like view-kit's row, plus the
 * selection marker view-kit leaves to its host. Layout and tokens only.
 */
export const ROW_LIST_STYLES = `
  .rowlist { list-style: none; margin: 0; padding: 0; }
  .rowlist-item { margin: 0; padding: 0; }
  .rowlist button.vk-row { box-sizing: border-box; width: 100%; margin: 0; font: inherit;
    text-align: left; background: transparent; border: 0; }
  .rowlist button.vk-row[data-selected="true"] { box-shadow: inset 2px 0 0 var(--memql-accent); }
  .rowlist button.vk-row:focus-visible { outline: 1px solid var(--memql-focus); outline-offset: -1px; }
`;
