import { UserInputError } from "./problems.js";
import MarkdownIt from "markdown-it";

export interface MarkdownAnchor {
  kind: "markdown";
  intent?: "extend";
  scope?: "section";
  startLine: number;
  endLine: number;
  sourceQuote: string;
  quote: string;
  prefix?: string;
  suffix?: string;
  sectionPath?: string[];
  startBlock?: number;
  endBlock?: number;
  startTextOffset?: number;
  endTextOffset?: number;
}
export const MAX_MARKDOWN_CHARS = 2 * 1024 * 1024;
export function escapeHTML(value: string): string {
  return value.replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]!));
}
const markdown = new MarkdownIt({ html: false, linkify: false, breaks: false, typographer: false });
// Link navigation is an explicit host action. No command:, javascript:, data:
// or local-file URL is allowed to become a webview navigation capability.
markdown.renderer.rules.link_open = (tokens, i) => {
  const href = tokens[i].attrGet("href") ?? "";
  if (!/^https?:\/\//i.test(href)) return "<span>";
  return `<a href="#" data-external="${escapeHTML(href)}">`;
};
markdown.renderer.rules.link_close = (tokens, i) => {
  let depth = 1;
  for (let j = i - 1; j >= 0; j--) {
    if (tokens[j].type === "link_close") depth++;
    if (tokens[j].type === "link_open" && --depth === 0) return /^https?:\/\//i.test(tokens[j].attrGet("href") ?? "") ? "</a>" : "</span>";
  }
  return "</span>";
};
// Remote images would leak document-open activity. Render the alt text without
// loading external content; local attachment resolution is a separate feature.
markdown.renderer.rules.image = (tokens, i) => `<span class="image-alt">${escapeHTML(tokens[i].content || "Image")}</span>`;
markdown.core.ruler.push("memql_source_ranges", state => {
  let block = 0;
  for (const token of state.tokens) {
    if (token.map && token.nesting !== -1 && token.type !== "inline") {
      token.attrSet("data-block-id", String(block++));
      token.attrSet("data-start-line", String(token.map[0]));
      token.attrSet("data-end-line", String(token.map[1]));
    }
  }
});
for (const name of ["fence", "code_block"] as const) {
  const original = markdown.renderer.rules[name]!;
  markdown.renderer.rules[name] = (tokens, index, options, env, renderer) => {
    const map = tokens[index].map;
    const html = original(tokens, index, options, env, renderer);
    return map ? `<div data-block-id="${tokens[index].attrGet("data-block-id")}" data-start-line="${map[0]}" data-end-line="${map[1]}">${html}</div>` : html;
  };
}
export function renderMarkdown(source: string): string {
  if (source.length > MAX_MARKDOWN_CHARS) throw new UserInputError("This Markdown document exceeds the 2 MiB reading-view limit. Open its source instead.");
  const tokens = markdown.parse(source, {});
  // Generated files carry YAML front matter. Keep it in Source, not as a
  // giant setext heading above the document. Assign ranges/block IDs BEFORE
  // filtering so existing revision-bound comments keep their exact anchors.
  const header = source.match(/^\uFEFF?---\r?\n([\s\S]*?)\r?\n---(?:\r?\n|$)/);
  if (!header || !/^[\w-]+\s*:/m.test(header[1])) return markdown.renderer.render(tokens, markdown.options, {});
  const endLine = header[0].split("\n").length - (header[0].endsWith("\n") ? 1 : 0);
  const hidden: boolean[] = [];
  const body = tokens.filter(token => {
    if (token.nesting === -1) return !hidden.pop();
    const hide = token.map ? token.map[0] < endLine : hidden.at(-1) ?? false;
    if (token.nesting === 1) hidden.push(hide);
    return !hide;
  });
  return markdown.renderer.render(body, markdown.options, {});
}
export function markdownAnchor(source: string, input: unknown): MarkdownAnchor {
  if (!input || typeof input !== "object") throw new UserInputError("Select a passage in the document first.");
  const row = input as Record<string, unknown>;
  const startLine = Number(row.startLine), endLine = Number(row.endLine);
  const quote = typeof row.quote === "string" ? row.quote.trim() : "";
  const lines = source.split(/\r?\n/);
  if (!Number.isSafeInteger(startLine) || !Number.isSafeInteger(endLine) || startLine < 0 || endLine <= startLine || endLine > lines.length || !quote || quote.length > 8000) {
    throw new UserInputError("Select a shorter passage in the rendered document.");
  }
  const sourceQuote = lines.slice(startLine, endLine).join("\n");
  if (sourceQuote.length > 16000) throw new UserInputError("Select a shorter passage for this comment.");
  const position: Partial<MarkdownAnchor> = {};
  for (const key of ["startBlock", "endBlock", "startTextOffset", "endTextOffset"] as const) {
    if (row[key] === undefined) continue;
    const value = Number(row[key]);
    if (!Number.isSafeInteger(value) || value < 0 || value > MAX_MARKDOWN_CHARS) throw new UserInputError("Select the passage again.");
    position[key] = value;
  }
  const sectionPath: string[] = [];
  let inFence = false;
  for (const line of lines.slice(0,startLine+1)) {
    if (/^\s*(`{3,}|~{3,})/.test(line)) inFence = !inFence;
    const heading = !inFence && line.match(/^(#{1,6})\s+(.+?)\s*#*$/);
    if (heading) { sectionPath.length = Math.min(sectionPath.length,heading[1].length-1); sectionPath.push(heading[2]); }
  }
  return { kind: "markdown", startLine, endLine, sourceQuote, quote, ...position,
    ...(row.intent === "extend" ? { intent: "extend" as const, ...(row.scope === "section" ? { scope: "section" as const } : {}) } : {}),
    prefix: typeof row.prefix === "string" ? row.prefix.slice(-80) : "", suffix: typeof row.suffix === "string" ? row.suffix.slice(0,80) : "", sectionPath };

}
export function anchorStillMatches(source: string, anchor: MarkdownAnchor): boolean {
  return source.split(/\r?\n/).slice(anchor.startLine, anchor.endLine).join("\n") === anchor.sourceQuote;
}
