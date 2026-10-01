// The gallery's stand-in for the VS Code webview host.
//
// WHAT IT IS FOR. A panel's document is only ever seen inside the editor, where
// nobody reviewing a change can look at it at real size, in both themes, wide
// and narrow, side by side. The gallery renders every scenario to a static
// HTML file that a browser opens directly (`npm run gallery`) and headless
// Chrome photographs (`npm run gallery:shoot`).
//
// WHAT VS CODE ADDS, ADDED HERE. The editor injects three things into every
// webview before the page's own markup, and a page that looks right without
// them can look wrong inside the editor:
//   1. the theme's colours and fonts as `--vscode-*` custom properties on
//      <html> (the values below are the Dark Modern and Light Modern themes,
//      for every variable the extension's CSS reads);
//   2. a default stylesheet (link and code colours, the focus outline,
//      scrollbars, a 20px body gutter) that follows the EDITOR's theme;
//   3. a `vscode-light` / `vscode-dark` class on <body> and the
//      `acquireVsCodeApi()` global.
// `wrapDocument` puts all three into a panel's document, under the document's
// own nonce, and KEEPS the page's CSP: a gallery page that needed the CSP
// loosened would be proving nothing about the real one.
//
// The acquireVsCodeApi shim logs every postMessage to the console, so a click
// can be checked in a browser's devtools, and it pins Date.now to GALLERY_NOW
// (plus the page's own running time) so an elapsed clock reads the same on
// every capture.

/** A theme the gallery renders every scenario in. */
export type GalleryTheme = "light" | "dark";

export const GALLERY_THEMES: readonly GalleryTheme[] = ["light", "dark"];

/** One page of the gallery. */
export interface Scenario {
  /** Unique, file-name safe: the page is written as `<id>.<theme>.html`. */
  id: string;
  /** The index heading it is listed under. */
  group: string;
  title: string;
  /** The full HTML document, exactly as the panel would assign it to `webview.html`. */
  render(theme: GalleryTheme): string;
}

/** The fixed "now" the gallery's clock starts from (2026-09-28 10:00:00 UTC). */
export const GALLERY_NOW = Date.UTC(2026, 8, 28, 10, 0, 0);

/** The nonce gallery-built documents use; wrapDocument reads whatever the document carries. */
export const GALLERY_NONCE = "gallery";

const FONTS: Readonly<Record<string, string>> = {
  "--vscode-font-family": "-apple-system, BlinkMacSystemFont, sans-serif",
  "--vscode-font-size": "13px",
  "--vscode-font-weight": "normal",
  "--vscode-editor-font-family": "Menlo, Monaco, 'Courier New', monospace",
  "--vscode-editor-font-size": "12px",
  "--vscode-editor-font-weight": "normal",
  "--monaco-monospace-font":
    "\"SF Mono\", Monaco, Menlo, Consolas, \"Ubuntu Mono\", \"Liberation Mono\", \"DejaVu Sans Mono\", \"Courier New\", monospace",
};

/** Dark Modern and Light Modern, for every `--vscode-*` colour the extension or VS Code's defaults read. */
export const VSCODE_THEME_VARS: Readonly<Record<GalleryTheme, Readonly<Record<string, string>>>> = {
  dark: {
    ...FONTS,
    "--vscode-editor-background": "#1f1f1f",
    "--vscode-editor-foreground": "#cccccc",
    "--vscode-foreground": "#cccccc",
    "--vscode-descriptionForeground": "#9d9d9d",
    "--vscode-errorForeground": "#f85149",
    "--vscode-focusBorder": "#0078d4",
    "--vscode-button-background": "#0078d4",
    "--vscode-button-foreground": "#ffffff",
    "--vscode-button-hoverBackground": "#026ec1",
    "--vscode-button-secondaryBackground": "#313131",
    "--vscode-button-secondaryForeground": "#cccccc",
    "--vscode-input-background": "#313131",
    "--vscode-input-border": "#3c3c3c",
    "--vscode-input-foreground": "#cccccc",
    "--vscode-input-placeholderForeground": "#989898",
    "--vscode-panel-border": "#2b2b2b",
    "--vscode-textLink-foreground": "#4daafc",
    "--vscode-textLink-activeForeground": "#4daafc",
    "--vscode-textPreformat-foreground": "#d0d0d0",
    "--vscode-textPreformat-background": "#3c3c3c",
    "--vscode-textBlockQuote-background": "#2b2b2b",
    "--vscode-textBlockQuote-border": "#616161",
    "--vscode-scrollbarSlider-background": "rgba(121, 121, 121, 0.4)",
    "--vscode-scrollbarSlider-hoverBackground": "rgba(100, 100, 100, 0.7)",
    "--vscode-scrollbarSlider-activeBackground": "rgba(191, 191, 191, 0.4)",
    "--vscode-editorError-foreground": "#f14c4c",
    "--vscode-editorWarning-foreground": "#cca700",
    "--vscode-charts-green": "#89d185",
    "--vscode-widget-shadow": "rgba(0, 0, 0, 0.36)",
  },
  light: {
    ...FONTS,
    "--vscode-editor-background": "#ffffff",
    "--vscode-editor-foreground": "#3b3b3b",
    "--vscode-foreground": "#3b3b3b",
    "--vscode-descriptionForeground": "#3b3b3b",
    "--vscode-errorForeground": "#f85149",
    "--vscode-focusBorder": "#005fb8",
    "--vscode-button-background": "#005fb8",
    "--vscode-button-foreground": "#ffffff",
    "--vscode-button-hoverBackground": "#0258a8",
    "--vscode-button-secondaryBackground": "#e5e5e5",
    "--vscode-button-secondaryForeground": "#3b3b3b",
    "--vscode-input-background": "#ffffff",
    "--vscode-input-border": "#cecece",
    "--vscode-input-foreground": "#3b3b3b",
    "--vscode-input-placeholderForeground": "#767676",
    "--vscode-panel-border": "#e5e5e5",
    "--vscode-textLink-foreground": "#005fb8",
    "--vscode-textLink-activeForeground": "#005fb8",
    "--vscode-textPreformat-foreground": "#3b3b3b",
    "--vscode-textPreformat-background": "rgba(0, 0, 0, 0.12)",
    "--vscode-textBlockQuote-background": "#f8f8f8",
    "--vscode-textBlockQuote-border": "#e5e5e5",
    "--vscode-scrollbarSlider-background": "rgba(100, 100, 100, 0.4)",
    "--vscode-scrollbarSlider-hoverBackground": "rgba(100, 100, 100, 0.7)",
    "--vscode-scrollbarSlider-activeBackground": "rgba(0, 0, 0, 0.6)",
    "--vscode-editorError-foreground": "#e51400",
    "--vscode-editorWarning-foreground": "#bf8803",
    "--vscode-charts-green": "#388a34",
    "--vscode-widget-shadow": "rgba(0, 0, 0, 0.16)",
  },
};

/** VS Code's default webview stylesheet (src/vs/workbench/contrib/webview/browser/pre/index.html). */
const VSCODE_DEFAULT_STYLES = `
html { scrollbar-color: var(--vscode-scrollbarSlider-background) var(--vscode-editor-background); }
body { overscroll-behavior-x: none; background-color: transparent; color: var(--vscode-editor-foreground);
       font-family: var(--vscode-font-family); font-weight: var(--vscode-font-weight);
       font-size: var(--vscode-font-size); margin: 0; padding: 0 20px; }
img, video { max-width: 100%; max-height: 100%; }
a, a code { color: var(--vscode-textLink-foreground); }
a:hover { color: var(--vscode-textLink-activeForeground); }
a:focus, input:focus, select:focus, textarea:focus { outline: 1px solid -webkit-focus-ring-color; outline-offset: -1px; }
code { font-family: var(--monaco-monospace-font); color: var(--vscode-textPreformat-foreground);
       background-color: var(--vscode-textPreformat-background); padding: 1px 3px; border-radius: 4px; }
pre code { padding: 0; }
blockquote { background: var(--vscode-textBlockQuote-background); border-color: var(--vscode-textBlockQuote-border); }
::-webkit-scrollbar { width: 10px; height: 10px; }
::-webkit-scrollbar-corner { background-color: var(--vscode-editor-background); }
::-webkit-scrollbar-thumb { background-color: var(--vscode-scrollbarSlider-background); }
::-webkit-scrollbar-thumb:hover { background-color: var(--vscode-scrollbarSlider-hoverBackground); }
::-webkit-scrollbar-thumb:active { background-color: var(--vscode-scrollbarSlider-activeBackground); }
`;

/** The acquireVsCodeApi shim, plus the pinned clock. No backticks: it is itself a script. */
function shimScript(): string {
  return `
(function () {
  var base = ${GALLERY_NOW};
  var start = performance.now();
  Date.now = function () { return base + Math.floor(performance.now() - start); };
  var acquired = false;
  var state;
  window.acquireVsCodeApi = function () {
    if (acquired) throw new Error('An instance of the VS Code API has already been acquired');
    acquired = true;
    return {
      postMessage: function (msg) { console.log('postMessage ' + JSON.stringify(msg)); },
      getState: function () { return state; },
      setState: function (next) { state = next; return next; }
    };
  };
})();
`;
}

/**
 * A panel document, as it would stand inside VS Code under `theme`.
 *
 * Throws on a document with no nonce: every panel document carries one, and a
 * gallery page that had to add its own CSP exception would be showing a page
 * the editor would never render.
 */
export function wrapDocument(html: string, theme: GalleryTheme): string {
  const nonce = /<style nonce="([^"]+)"/.exec(html)?.[1] ?? /<script nonce="([^"]+)"/.exec(html)?.[1];
  if (nonce === undefined) throw new Error("wrapDocument: the document carries no nonce");
  if (!html.includes("<head>")) throw new Error("wrapDocument: the document has no <head>");
  const vars = Object.entries(VSCODE_THEME_VARS[theme])
    .map(([name, value]) => `  ${name}: ${value};`)
    .join("\n");
  const injected =
    `<style nonce="${nonce}" id="_defaultStyles">\nhtml {\n${vars}\n}\n${VSCODE_DEFAULT_STYLES}</style>\n` +
    `<script nonce="${nonce}">${shimScript()}</script>`;
  return html
    .replace("<head>", () => `<head>\n${injected}`)
    .replace(/<body([^>]*)>/, (_tag, attrs: string) => {
      const kind = `vscode-${theme}`;
      if (/\sclass="/.test(attrs)) {
        return `<body${attrs.replace(/\sclass="/, ` class="${kind} `)} data-vscode-theme-kind="${kind}">`;
      }
      return `<body class="${kind}" data-vscode-theme-kind="${kind}"${attrs}>`;
    });
}

/**
 * Append a script that plays the HOST's side after the page has loaded --
 * posting the `patch` / `progress` / `log` messages a LiveView would -- so a
 * capture shows the runtime having applied them.
 */
export function withHostScript(html: string, script: string): string {
  const nonce = /<script nonce="([^"]+)"/.exec(html)?.[1];
  if (nonce === undefined) throw new Error("withHostScript: the document has no script nonce");
  // A function replacement, so a `$` in the script is never read as a pattern.
  return html.replace("</body>", () => `<script nonce="${nonce}">\n${script}\n</script>\n</body>`);
}
