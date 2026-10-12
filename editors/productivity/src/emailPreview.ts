// What the email pages show of an email: the engine's render, made safe for a
// sandboxed frame (previewHTML), and a grey outline of its blocks for the moments
// before a render exists (skeletonHTML).
//
// Pure: no vscode and no SDK. Both view bundles import this module, so it takes
// nothing else with it. previewHTML needs a DOMParser: the view's own, or jsdom's
// in a test.
import type { EmailBlock } from "./emailDocument.js";

// -- The preview --------------------------------------------------------------

/** What a preview frame may load: its own inline styles and embedded pictures, nothing else. */
export const PREVIEW_CSP = "default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'none'; base-uri 'none'";

/** The doctype the renderer writes: the limited-quirks mode email clients use. Keeping it keeps the table layout the same in the preview. */
const XHTML_DOCTYPE = '<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd">';

// The email presentation elements and attributes a preview keeps; everything else goes (previewHTML).
const TAGS = new Set("a b blockquote br caption center code col colgroup dd div dl dt em font h1 h2 h3 h4 h5 h6 hr i img li ol p pre s small span strong sub sup table tbody td th thead tfoot tr u ul style".split(" "));
const ATTRIBUTES = new Set("align alt bgcolor border cellpadding cellspacing class color colspan dir face height lang role rowspan size style title valign width".split(" "));
const DATA_PICTURE = /^data:image\/(png|jpeg|gif);base64,[A-Za-z0-9+/=]+$/;
/** A block id, as the renderer marks it in a preview (the id grammar of the email document). */
const BLOCK_ID = /^[a-z0-9][a-z0-9-]{0,63}$/;

function keeps(element: Element, attribute: Attr, blocks: boolean): boolean {
  switch (attribute.name) {
    case "src": return element.localName === "img" && DATA_PICTURE.test(attribute.value);
    case "data-block": return blocks && BLOCK_ID.test(attribute.value);
    default: return ATTRIBUTES.has(attribute.name);
  }
}

/**
 * An email's HTML made safe to show in a sandboxed frame. It is read in an inert
 * document and rebuilt from the email presentation elements alone, so nothing in it
 * can run, navigate, submit or fetch; the policy meta says the same to the frame.
 * Layout survives: the doctype, the body's attributes, the head's styles and every
 * inline style. Links lose their address and a picture that is not embedded is named
 * instead of loaded. With `blocks`, the renderer's `data-block` marks survive, so the
 * page can tell which block a click landed in.
 */
export function previewHTML(html: string, options: { blocks: boolean }): string {
  const doc = new DOMParser().parseFromString(html, "text/html");
  for (const node of Array.from(doc.querySelectorAll("*"))) {
    const name = node.localName;
    if (name === "html" || name === "head") continue;
    if (name !== "body" && !TAGS.has(name)) { node.remove(); continue; }
    for (const attribute of Array.from(node.attributes)) {
      if (!keeps(node, attribute, options.blocks)) node.removeAttribute(attribute.name);
    }
    if (name === "img" && !node.hasAttribute("src")) {
      const label = doc.createElement("span");
      label.textContent = `[Image: ${node.getAttribute("alt") || "external asset"}]`;
      node.replaceWith(label);
    }
  }
  const styles = Array.from(doc.head.querySelectorAll("style")).map(style => style.outerHTML).join("");
  // A document that is only a frameset has no body left once the frameset is gone.
  const body = doc.body ? doc.body.outerHTML : "<body></body>";
  const doctype = html.startsWith(XHTML_DOCTYPE) ? XHTML_DOCTYPE : "<!doctype html>";
  return `${doctype}<html><head><meta http-equiv="Content-Security-Policy" content="${PREVIEW_CSP}">${styles}</head>${body}</html>`;
}

// -- The skeleton -------------------------------------------------------------

/** The deepest nesting the email model accepts; anything deeper is not drawn. */
const MAX_DEPTH = 16;
/** The width of an email, in pixels. A block takes its share of the row it sits in: the email, or a section or column of it. */
const EMAIL_WIDTH = 600;
/** About how wide a heading's character is, as a share of its size: enough to give a heading its length, never its words. */
const GLYPH = 0.5;

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null;
/** A number above zero, else nothing: zero is how a block says "the default", and a field may hold anything at all. */
const positive = (value: unknown): number | undefined => typeof value === "number" && Number.isFinite(value) && value > 0 ? value : undefined;
const clamp = (value: number, low: number, high: number) => Math.min(high, Math.max(low, value));
/** A number as short CSS: at most two decimals and no trailing zeros. */
const short = (value: number) => String(Math.round(value * 100) / 100);
/** Pixels of the row (600 for the email itself) as a percentage of it, to two decimals; never more than the row. */
const share = (pixels: number, row: number) => `${(Math.min(pixels, row) / (row / 100)).toFixed(2)}%`;
/** The class that places a shape as its block is aligned: none for the left. */
const aligned = (align: unknown, fallback: "left" | "center") => {
  const value = align === "left" || align === "center" || align === "right" ? align : fallback;
  return value === "left" ? "" : ` sk-${value}`;
};

const LINE = '<div class="sk sk-line"></div>';
/** `count` lines, the last one shorter and placed as the text is aligned. */
const lines = (count: number, align = "") => LINE.repeat(count - 1) + `<div class="sk sk-line${align}" style="width:60%"></div>`;

/**
 * The outline of an email: a grey shape for each block, in order, sized from the
 * block and nothing else. No text from the email enters it, only numbers that have
 * been checked and clamped (a text's length becomes its number of lines, a
 * heading's its width), so it is safe to put in a page as it is.
 */
export function skeletonHTML(blocks: readonly EmailBlock[]): string {
  return `<div class="sk-email" aria-hidden="true">${shapes(blocks, 0, EMAIL_WIDTH)}</div>`;
}

function shapes(list: unknown, depth: number, row: number): string {
  if (!Array.isArray(list) || depth > MAX_DEPTH) return "";
  return list.map(block => isRecord(block) ? shape(block as EmailBlock, depth, row) : "").join("");
}

/** One block's shape, in a row `row` pixels of the email wide. */
function shape(block: EmailBlock, depth: number, row: number): string {
  switch (block.type) {
    case "image": {
      const width = Math.round(clamp(positive(block.width) ?? row, 1, EMAIL_WIDTH));
      // Without a height, half the width: the same as the placeholder the renderer draws.
      const height = Math.round(clamp(positive(block.height) ?? width * 0.5, 1, 2000));
      return `<div class="sk sk-picture" style="width:${share(width, row)};aspect-ratio:${width}/${height}"></div>`;
    }
    case "heading": {
      // Centred unless it says otherwise, as the renderer draws it, and as long as its longest line.
      const size = positive(block.size) ?? 28;
      const text = typeof block.text === "string" ? block.text.slice(0, 2000) : "";
      const longest = text.split("\n").reduce((most, line) => Math.max(most, line.length), 0);
      const width = longest > 0 ? `width:${share(Math.max(longest * size * GLYPH, row * 0.15), row)};` : "";
      return `<div class="sk sk-heading${aligned(block.align, "center")}" style="${width}height:${short(clamp(size * 0.8, 14, 40))}px"></div>`;
    }
    case "text": {
      // A line per 70 characters of a 600 pixel row, fewer to a line in a narrower one.
      const length = typeof block.text === "string" ? block.text.length : 0;
      return lines(clamp(Math.ceil(length / Math.max(10, 70 * row / EMAIL_WIDTH)), 2, 8), aligned(block.align, "left"));
    }
    case "highlight": return '<div class="sk sk-band"></div>';
    case "button": return `<div class="sk sk-button" style="width:${share(Math.round(clamp(positive(block.width) ?? 300, 1, EMAIL_WIDTH)), row)}"></div>`;
    case "divider": return '<div class="sk-rule"></div>';
    case "spacer": return `<div class="sk-gap" style="height:${short(clamp((positive(block.height) ?? 0) / 2, 4, 60))}px"></div>`;
    case "section": return `<div class="sk-section">${shapes(block.blocks, depth + 1, Math.min(row, positive(block.maxWidth) ?? row))}</div>`;
    case "columns": {
      // Columns narrower than their row stand centred at their own width, as the renderer draws them.
      const width = Math.min(row, positive(block.maxWidth) ?? row);
      const cells = columns(block.columns, depth, width);
      return width < row ? `<div class="sk-columns sk-center" style="width:${share(width, row)}">${cells}</div>` : `<div class="sk-columns">${cells}</div>`;
    }
    case "social": return '<div class="sk-social"><i></i><i></i><i></i></div>';
    case "footer": return `<div class="sk-footer">${LINE.repeat(3)}</div>`;
    case "html": return lines(2);
    default: return "";
  }
}

/** Each column takes its width as its share of the row (one without a usable width an equal share), and its blocks theirs of it. */
function columns(list: unknown, depth: number, row: number): string {
  const cells = Array.isArray(list) ? list.filter(isRecord) : [];
  return cells.map(column => {
    const width = clamp(positive(column.width) ?? 100 / cells.length, 1, 100);
    return `<div style="flex:${short(width)}">${shapes(column.blocks, depth + 1, row * width / 100)}</div>`;
  }).join("");
}
