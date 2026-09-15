# MemQL for Visual Studio Code and Cursor landing page

A static product page for the existing extension. It uses canonical brand fonts
colours and artwork, shows the actual research-brief source, and links to repository docs.
It has no cluster connection, external font dependency, analytics, or server runtime.

## Build and preview

Requirements: Node.js 20+; Python 3 for the preview server. The build has no npm
dependencies. Playwright is a development dependency for browser verification.
From the repository root:

```bash
npm --prefix editors/vscode/site run build
npm --prefix editors/vscode/site run check
npm --prefix editors/vscode/site run preview
```

Open **http://127.0.0.1:4317/**. The preview binds only to loopback and serves
`editors/vscode/site/dist`. Stop it with Ctrl+C. To use another port:

```bash
python3 -m http.server 4318 --bind 127.0.0.1 --directory editors/vscode/site/dist
```

The build copies static files plus shared `brand/` assets and generates the
code-theme CSS from the committed native editor themes and interactive panels from `examples/research-desk/research/brief.memql`. The source
manifest and complete setup guide are included beside the downloadable example. Run its actual linter:

```bash
go run ./cmd/memqllint examples/research-desk
```

`build.mjs --check` verifies asset paths, internal anchors, source/output parity,
and the absence of inline scripts. The code tabs are extracted from marked
regions in the complete sample rather than maintained as separate snippets.
Core declarations and advanced workflow examples are separate tab groups;
traits/specs are initially visible in the core group. It does not check live external URLs or run
the example against a cluster.

## Browser verification

```bash
npm --prefix editors/vscode/site ci
cd editors/vscode/site
npx playwright install chromium
npm run test:browser
```

The test starts its own ephemeral loopback server and fresh headless browser.
To use an existing browser binary without downloading Chromium, set
`SITE_BROWSER_EXECUTABLE` to its absolute executable path. It still launches a
new headless process and never attaches to an existing browser. Screenshots go
to the ignored `artifacts/` directory, outside the deployable bundle.

The [CI workflow](../../../.github/workflows/vscode-site.yml) builds and tests
the page and retains the static bundle and screenshots. It publishes nothing.

## Deployable artifact

`dist/` is the complete artifact: static files with `index.html` at the root.
It follows the repository's [site hosting contract](../../../docs/public/operate/site-hosting.md).
Serve it at a hostname's root, or at a static host subdirectory with a trailing
slash. Relative asset paths support either. No SPA fallback is required.

For MemQL hosting, publish the built directory through the existing Deployables /
site bundle workflow, then associate the resulting bundle with the intended site
and hostname. The source tree needs the repository root as build context because
it reads `brand/`, `editors/vscode/themes/`, and `examples/research-desk/`; the output directory is
`editors/vscode/site/dist`. Do not use `editors/vscode/site` alone as the build
context. The edge must be able to read the published bundle; a developer's local
filesystem path is not a path inside the edge pod.

Use external scripts and same-origin assets; the page is compatible with the
edge's script/style/font policy without adding inline-script permission. The
preview server is only a local inspection tool, not the deployment server.
Public publishing and cluster/site-row changes are separate actions and have
not been performed by this build.

## Content and verification

- Extension features: `editors/vscode/package.json`, `src/constructs/runnable.ts`,
  `src/training/actions.ts`, the language-server contract, and extension tests.
- Installation: `scripts/vscode/package.sh`, `scripts/vscode/install.sh`,
  `scripts/lib/platform.sh`, and the existing release workflow. No marketplace
  URL or hosted VSIX is invented.
- Code illustration: a labeled illustration, not a screenshot or live run.
  Tabs and downloads use the canonical research-brief example. A snippet may
  depend on another construct; the download contains all definitions and imports.
- Shared visual identity: fonts, colour tokens, mark, and favicon are copied
  from `brand/` at build time. Marks resolve the chosen page accent; code colours
  come from the generated native editor themes.
- Appearance: System is the default and follows OS changes live. Light and Dark
  persist in local storage. Invalid or unavailable storage falls back to System;
  selection still works for the visit. Without JavaScript, CSS follows the OS.
  The page never changes the editor or OS appearance.

Browser checks cover both appearances at 320–1440px, live System changes,
persisted choices, unavailable storage, rendered text contrast, and keyboard
focus. Visual QA covers desktop and narrow screens, all code tabs and keyboard
navigation, visible focus, downloads, clipboard success/failure, reduced motion,
and JavaScript-disabled fallback. See the task report for observed results.
