// AN EMAIL, SHOWN AS IT IS SENT, THAT CAN NEITHER RUN NOR FETCH ANYTHING
// (memql#6033, Ruling 21).
//
// The template page, a layout's page and the capture inbox draw an email's HTML
// in a frame. The email is shown with its own look -- an email is laid out with
// inline styles, style blocks and table attributes, and a preview without them
// is not the email that is sent -- and nothing in it may run or reach the
// network. Three layers say so, each enough on its own for what it covers:
//
//   - this sanitizer keeps only elements and attributes that draw, strips
//     scripts, handlers, forms, `meta`, `base` and `link` (and SVG and MathML
//     whole), keeps a picture only when it is a raster carried in the email
//     itself, and drops every CSS declaration and rule that would load
//     something (`url(`, `@import`, the image functions that take a bare
//     string, `expression(`, and the `behavior` and `-moz-binding` properties);
//   - the document it writes opens with a Content-Security-Policy that allows
//     inline styles and `data:` pictures and nothing else;
//   - the frame is sandboxed without scripts and without its origin
//     (`templates/Preview.tsx`).
//
// The sanitizer is a function of a string, so all of it is asserted without a
// frame (test/email/preview.test.ts).

/** Elements that draw an email and do nothing else. */
const KEEP = new Set([
  "A", "ABBR", "ADDRESS", "ARTICLE", "ASIDE", "B", "BDI", "BDO", "BIG", "BLOCKQUOTE", "BR", "CAPTION", "CENTER", "CITE",
  "CODE", "COL", "COLGROUP", "DD", "DEL", "DFN", "DIV", "DL", "DT", "EM", "FIGCAPTION", "FIGURE", "FONT", "FOOTER", "H1",
  "H2", "H3", "H4", "H5", "H6", "HEADER", "HR", "I", "IMG", "INS", "KBD", "LI", "MAIN", "MARK", "NAV", "OL", "P", "PRE",
  "Q", "RP", "RT", "RUBY", "S", "SAMP", "SECTION", "SMALL", "SPAN", "STRIKE", "STRONG", "SUB", "SUP", "TABLE", "TBODY",
  "TD", "TFOOT", "TH", "THEAD", "TIME", "TR", "TT", "U", "UL", "VAR", "WBR",
]);

/**
 * Elements that go WITH what they hold: they run, load, take input or stand for
 * another document. Anything neither kept nor here (`o:p`, `label`, `summary`)
 * gives way to its content, so an unknown wrapper never costs the words in it.
 *
 * Names are compared in upper case: an SVG or MathML element keeps its own
 * lower-case name in an HTML document (`svg`, and a `script` inside it), and
 * one that slipped past as unknown would be unwrapped, leaving its script as
 * visible text.
 */
const DROP = new Set([
  "APPLET", "AREA", "AUDIO", "BASE", "BGSOUND", "BUTTON", "CANVAS", "DATALIST", "DIALOG", "EMBED", "FENCEDFRAME", "FORM",
  "FRAME", "FRAMESET", "HEAD", "IFRAME", "INPUT", "KEYGEN", "LINK", "MAP", "MATH", "META", "NOSCRIPT", "OBJECT",
  "OPTGROUP", "OPTION", "OUTPUT", "PARAM", "PORTAL", "SCRIPT", "SELECT", "SLOT", "SOURCE", "STYLE", "SVG", "TEMPLATE",
  "TEXTAREA", "TITLE", "TRACK", "VIDEO",
]);

/** Attributes an email is laid out with. `href` and `src` are judged on their own. */
const ATTRIBUTES = new Set([
  "align", "alt", "bgcolor", "border", "cellpadding", "cellspacing", "class", "color", "colspan", "dir", "face", "height",
  "hspace", "id", "lang", "nowrap", "role", "rowspan", "size", "span", "start", "style", "title", "type", "valign",
  "vspace", "width",
]);

/** A picture carried in the email itself, as a raster: the only one the policy lets load. */
const RASTER = /^data:image\/(png|jpeg|gif);base64,[A-Za-z0-9+/=]+$/;
const MAX_RASTER = 2 * 1024 * 1024;

/**
 * What the frame may do: inline styles and pictures carried in the email, and
 * nothing fetched from anywhere, nothing run, no base and no form target.
 */
export const PREVIEW_POLICY = "default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'";

/**
 * The frame's own defaults, BEFORE the email's styles so the email's win where
 * it speaks: a plain-text message still reads as text, and a rendered email
 * keeps its own margins, fonts and width.
 */
const DEFAULTS = "body{margin:0;padding:16px;font:15px/1.6 system-ui,sans-serif;overflow-wrap:anywhere}img{max-width:100%}pre{white-space:pre-wrap}";

/** Keep an email readable as it is sent, without running it or loading anything it names. */
export function emailPreview(html: string): string {
  const doc = new DOMParser().parseFromString(html, "text/html");

  // The email's style blocks, wherever they stood (an email's are in its head),
  // each kept for what it says that loads nothing.
  const styles: string[] = [];
  for (const style of [...doc.querySelectorAll("style")]) {
    const css = sanitizeStylesheet(style.textContent ?? "");
    // A block that would end early once written out is not the block it was.
    if (css.trim() !== "" && !/<\/style/i.test(css)) styles.push(css);
    style.remove();
  }

  for (const comment of commentsIn(doc.body)) comment.remove();
  for (const element of [...doc.body.querySelectorAll("*")]) {
    const tag = element.tagName.toUpperCase();
    if (DROP.has(tag)) {
      element.remove();
      continue;
    }
    if (!KEEP.has(tag)) {
      element.replaceWith(...element.childNodes);
      continue;
    }
    keepAttributes(element);
  }
  keepAttributes(doc.body);

  return (
    '<!doctype html><html><head>' +
    `<meta http-equiv="Content-Security-Policy" content="${PREVIEW_POLICY}">` +
    '<meta name="referrer" content="no-referrer">' +
    `<style>${DEFAULTS}</style>` +
    styles.map((css) => `<style>${css}</style>`).join("") +
    "</head>" +
    doc.body.outerHTML +
    "</html>"
  );
}

/** Every comment under a node: invisible to the reader, and nothing to keep. */
function commentsIn(root: Node): Comment[] {
  const out: Comment[] = [];
  const walker = root.ownerDocument!.createTreeWalker(root, NodeFilter.SHOW_COMMENT);
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) out.push(node as Comment);
  return out;
}

/** An element keeps the attributes that lay it out; a link keeps an address that goes somewhere; a picture keeps a raster it carries. */
function keepAttributes(element: Element): void {
  const href = (element.getAttribute("href") ?? "").trim();
  const src = element.getAttribute("src") ?? "";
  for (const attribute of [...element.attributes]) {
    const name = attribute.name.toLowerCase();
    if (!ATTRIBUTES.has(name)) {
      element.removeAttribute(attribute.name);
      continue;
    }
    if (name === "style") {
      const style = sanitizeDeclarations(attribute.value);
      if (style === "") element.removeAttribute(attribute.name);
      else element.setAttribute(attribute.name, style);
    }
  }
  const tag = element.tagName.toUpperCase();
  if (tag === "IMG") {
    if (src.length <= MAX_RASTER && RASTER.test(src)) element.setAttribute("src", src);
    else element.remove();
  }
  if (tag === "A" && /^https?:\/\//i.test(href)) {
    element.setAttribute("href", href);
    element.setAttribute("target", "_blank");
    element.setAttribute("rel", "noopener noreferrer");
  }
}

// ---------------------------------------------------------------------------
// CSS: what an email's styles may say
// ---------------------------------------------------------------------------
//
// Not a CSS parser, and it does not need to be one: it reads a stylesheet as
// items -- a declaration or a statement, ended by `;`, or a block, a prelude and
// what its braces hold -- minding strings, escapes, comments and parentheses
// (a `;` inside `url(data:...;base64,...)` ends nothing). An item that would
// load something is dropped whole; everything else is kept as it was written.
// Whether an item loads something is asked of its text with comments taken out
// and escapes read, so neither `u\72 l(` nor `ur/**/l(` is a way past it.

/**
 * What makes a declaration or a rule fetch or run: a URL, an import, an image
 * function that takes a bare string or another element (`image-set(`,
 * `image(`, `cross-fade(` and their prefixed forms, `element(`, `paint(`,
 * `src(`), script.
 */
const LOADS = /url\(|src\(|image-set\(|image\(|cross-fade\(|element\(|paint\(|@import|expression\(|javascript:/;

/**
 * The properties that load script whatever their value says: IE's `behavior`
 * (and its `-ms-` and `*` hack spellings) and Firefox's `-moz-binding`. Asked
 * of a declaration's property name, so a selector or a value that only says the
 * word (`.behavior-note`, a font named Behavior) keeps its rule.
 */
const LOADING_PROPERTY = /^[*_]?(?:(?:-ms-)?behavior|-moz-binding):/;

/** A style attribute's declarations, without any that would load something. "" when none is left. */
export function sanitizeDeclarations(css: string): string {
  return sanitizeStylesheet(css);
}

/** A stylesheet (or a block's body), without any declaration or rule that would load something. */
export function sanitizeStylesheet(css: string): string {
  const kept: string[] = [];
  let at = 0;
  while (at < css.length) {
    const { text, end, stop } = readItem(css, at);
    const prelude = text.trim();
    if (stop === "{") {
      const close = closingBrace(css, end + 1);
      const body = css.slice(end + 1, close);
      if (prelude !== "" && !loads(prelude)) {
        const inner = sanitizeStylesheet(body);
        // A rule left with nothing in it says nothing; a block of rules keeps its shape.
        kept.push(`${prelude}{${inner}}`);
      }
      at = close + 1;
      continue;
    }
    if (prelude !== "" && !loads(prelude)) kept.push(prelude);
    // A stray `}` ends nothing here: it is skipped with the item before it.
    at = end + 1;
  }
  // Blocks stand on their own; declarations and statements are separated by `;`.
  let out = "";
  for (const item of kept) {
    if (out !== "" && !out.endsWith("}")) out += ";";
    out += item;
  }
  return out;
}

/** Whether an item's text, comments out and escapes read, names anything that loads. */
function loads(text: string): boolean {
  const read = unescapeCss(withoutComments(text)).toLowerCase().replace(/\s+/g, "");
  return LOADS.test(read) || LOADING_PROPERTY.test(read);
}

/**
 * One item from `from`: its text, the index of what ended it, and what that was
 * (`;`, `{`, `}` or the end). Strings, escapes, comments and parentheses are
 * stepped over, so none of them ends an item.
 */
function readItem(css: string, from: number): { text: string; end: number; stop: ";" | "{" | "}" | "" } {
  let depth = 0;
  let at = from;
  while (at < css.length) {
    const char = css[at]!;
    if (char === "\\") {
      at += 2;
      continue;
    }
    if (char === '"' || char === "'") {
      at = endOfString(css, at);
      continue;
    }
    if (char === "/" && css[at + 1] === "*") {
      const close = css.indexOf("*/", at + 2);
      at = close < 0 ? css.length : close + 2;
      continue;
    }
    if (char === "(") depth++;
    else if (char === ")" && depth > 0) depth--;
    else if (depth === 0 && (char === ";" || char === "{" || char === "}")) {
      return { text: css.slice(from, at), end: at, stop: char };
    }
    at++;
  }
  return { text: css.slice(from), end: css.length, stop: "" };
}

/** The index of the brace that closes the block opened just before `from`, or the end. */
function closingBrace(css: string, from: number): number {
  let depth = 1;
  let at = from;
  while (at < css.length) {
    const char = css[at]!;
    if (char === "\\") {
      at += 2;
      continue;
    }
    if (char === '"' || char === "'") {
      at = endOfString(css, at);
      continue;
    }
    if (char === "/" && css[at + 1] === "*") {
      const close = css.indexOf("*/", at + 2);
      at = close < 0 ? css.length : close + 2;
      continue;
    }
    if (char === "{") depth++;
    else if (char === "}" && --depth === 0) return at;
    at++;
  }
  return css.length;
}

/** The index just past the string that opens at `from`. An unclosed string runs to the end of its line. */
function endOfString(css: string, from: number): number {
  const quote = css[from];
  let at = from + 1;
  while (at < css.length) {
    const char = css[at]!;
    if (char === "\\") {
      at += 2;
      continue;
    }
    if (char === quote) return at + 1;
    if (char === "\n") return at;
    at++;
  }
  return css.length;
}

function withoutComments(css: string): string {
  return css.replace(/\/\*[\s\S]*?(?:\*\/|$)/g, "");
}

/** CSS escapes read as the characters they stand for: `\75 ` is `u`, `\(` is `(`. */
function unescapeCss(css: string): string {
  return css.replace(/\\(?:([0-9a-fA-F]{1,6})\s?|([\s\S]))/g, (_, hex: string | undefined, char: string | undefined) => {
    if (hex !== undefined) {
      const point = Number.parseInt(hex, 16);
      return point > 0 && point <= 0x10ffff ? String.fromCodePoint(point) : "�";
    }
    return char === "\n" ? "" : (char ?? "");
  });
}
