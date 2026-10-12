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

// The allowlists are those of safeEmailHTML in templatePreview.ts, which this replaces.
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
/** The width of an email, in pixels: a block's width over this is its share of the row. */
const EMAIL_WIDTH = 600;

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null;
/** A number above zero, else nothing: zero is how a block says "the default", and a field may hold anything at all. */
const positive = (value: unknown): number | undefined => typeof value === "number" && Number.isFinite(value) && value > 0 ? value : undefined;
const clamp = (value: number, low: number, high: number) => Math.min(high, Math.max(low, value));
/** A number as short CSS: at most two decimals and no trailing zeros. */
const short = (value: number) => String(Math.round(value * 100) / 100);
/** Pixels of the 600 pixel email as a percentage of the row, to two decimals. */
const share = (pixels: number) => `${(pixels / (EMAIL_WIDTH / 100)).toFixed(2)}%`;

const LINE = '<div class="sk sk-line"></div>';
/** `count` lines, the last one shorter. */
const lines = (count: number) => LINE.repeat(count - 1) + '<div class="sk sk-line" style="width:60%"></div>';

/**
 * The outline of an email: a grey shape for each block, in order, sized from the
 * block and nothing else. No text from the email enters it, only numbers that have
 * been checked and clamped, so it is safe to put in a page as it is.
 */
export function skeletonHTML(blocks: readonly EmailBlock[]): string {
  return `<div class="sk-email" aria-hidden="true">${shapes(blocks, 0)}</div>`;
}

function shapes(list: unknown, depth: number): string {
  if (!Array.isArray(list) || depth > MAX_DEPTH) return "";
  return list.map(block => isRecord(block) ? shape(block as EmailBlock, depth) : "").join("");
}

function shape(block: EmailBlock, depth: number): string {
  switch (block.type) {
    case "image": {
      const width = Math.round(clamp(positive(block.width) ?? EMAIL_WIDTH, 1, EMAIL_WIDTH));
      // Without a height, half the width: the same as the placeholder the renderer draws.
      const height = Math.round(clamp(positive(block.height) ?? width * 0.5, 1, 2000));
      return `<div class="sk sk-picture" style="width:${share(width)};aspect-ratio:${width}/${height}"></div>`;
    }
    case "heading": return `<div class="sk sk-heading" style="height:${short(clamp((positive(block.size) ?? 28) * 0.8, 14, 40))}px"></div>`;
    case "text": return lines(clamp(Math.ceil((typeof block.text === "string" ? block.text.length : 0) / 70), 2, 8));
    case "highlight": return '<div class="sk sk-band"></div>';
    case "button": return `<div class="sk sk-button" style="width:${share(Math.round(clamp(positive(block.width) ?? 300, 1, EMAIL_WIDTH)))}"></div>`;
    case "divider": return '<div class="sk-rule"></div>';
    case "spacer": return `<div class="sk-gap" style="height:${short(clamp((positive(block.height) ?? 0) / 2, 4, 60))}px"></div>`;
    case "section": return `<div class="sk-section">${shapes(block.blocks, depth + 1)}</div>`;
    case "columns": return `<div class="sk-columns">${columns(block.columns, depth)}</div>`;
    case "social": return '<div class="sk-social"><i></i><i></i><i></i></div>';
    case "footer": return `<div class="sk-footer">${LINE.repeat(3)}</div>`;
    case "html": return lines(2);
    default: return "";
  }
}

/** Each column takes its width as its share of the row; one without a usable width takes an equal share. */
function columns(list: unknown, depth: number): string {
  const row = Array.isArray(list) ? list.filter(isRecord) : [];
  return row.map(column => `<div style="flex:${short(clamp(positive(column.width) ?? 100 / row.length, 1, 100))}">${shapes(column.blocks, depth + 1)}</div>`).join("");
}
