// The links of a text block's Markdown, read as the engine's parser reads them
// (component/compose/emaildoc/markdown.go): paragraphs are their lines joined,
// a list item is one line, "[label](address)" with balanced parentheses, and a
// backslash escapes \ * [ ] ( ). A link needs a label and an address the link
// rule allows. Anything this does not call a link, the parser does not either.
//
// The editor uses it to refuse such a link before an edit is sent. A corpus the
// engine's own test writes (component/compose/emaildoc/testdata/markdown-links.json)
// holds the two to the same verdicts (test/email-markdown.test.ts).
//
// Pure: no vscode and no DOM.

import { validLink } from "./emailDocument.js";

const ESCAPABLE = "\\*[]()";
/** What Go's strings.TrimSpace trims (unicode.IsSpace). */
const GO_SPACE = "\\t\\n\\v\\f\\r \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000";
const EDGE_SPACE = new RegExp(`^[${GO_SPACE}]+|[${GO_SPACE}]+$`, "g");
/** A line that starts a list item: "- ", or one to nine digits and ". ". */
const LIST_ITEM = /^(?:- |\d{1,9}\. )/;

const goTrim = (text: string) => text.replace(EDGE_SPACE, "");
/** A backslash with one of \ * [ ] ( ) after it. */
const escapes = (text: string, at: number) => text[at] === "\\" && at + 1 < text.length && ESCAPABLE.includes(text[at + 1]!);
const unescape = (text: string) => text.replace(/\\([\\*[\]()])/g, "$1");

/** What is wrong with the first link the parser would refuse: its label is missing, or its address. Nothing when every link is fine. */
export function markdownLinkProblem(source: string): "label" | "address" | undefined {
  const runs: string[] = [];
  let paragraph: string[] = [];
  const endParagraph = () => { if (paragraph.length) runs.push(paragraph.join("\n")); paragraph = []; };
  for (const raw of source.replace(/\r\n?/g, "\n").split("\n")) {
    const line = goTrim(raw);
    if (line === "") { endParagraph(); continue; }
    const item = LIST_ITEM.exec(line);
    if (!item) { paragraph.push(line); continue; }
    endParagraph();
    runs.push(goTrim(line.slice(item[0].length)));
  }
  endParagraph();
  for (const run of runs) {
    const problem = linkProblem(run);
    if (problem) return problem;
  }
  return undefined;
}

function linkProblem(text: string): "label" | "address" | undefined {
  if (!text.includes("](")) return undefined;
  const parens = matchParens(text);
  for (let at = 0; at < text.length;) {
    if (escapes(text, at)) { at += 2; continue; }
    if (text[at] !== "[") { at++; continue; }
    const close = labelEnd(text, at);
    const closeParen = close < 0 || text[close + 1] !== "(" ? 0 : parens[close + 1] ?? 0;
    if (closeParen === 0) { at++; continue; }
    if (goTrim(unescape(text.slice(at + 1, close)).replace(/\n/g, " ")) === "") return "label";
    const address = unescape(text.slice(close + 2, closeParen));
    if (address === "" || !validLink(address)) return "address";
    at = closeParen + 1;
  }
  return undefined;
}

/** For each "(", the index of the ")" that closes it, or 0 (a ")" is never at 0). */
function matchParens(text: string): Int32Array {
  const parens = new Int32Array(text.length), open: number[] = [];
  for (let at = 0; at < text.length; at++) {
    if (escapes(text, at)) at++;
    else if (text[at] === "(") open.push(at);
    else if (text[at] === ")" && open.length) parens[open.pop()!] = at;
  }
  return parens;
}

/** The "]" that closes the "[" at `start`, or -1; a label holds no "[". */
function labelEnd(text: string, start: number): number {
  for (let at = start + 1; at < text.length; at++) {
    if (escapes(text, at)) at++;
    else if (text[at] === "[") return -1;
    else if (text[at] === "]") return at;
  }
  return -1;
}
