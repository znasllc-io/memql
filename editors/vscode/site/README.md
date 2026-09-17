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
the page and keeps the built page and screenshots for review. It is a check,
not a publish.

## Hosting: a built-in platform site

The page is a built-in platform site, hosted the same way MemQL OS is
(memql#5518). The edge image builds it in the Dockerfile's `spa-build` stage --
the stage that also builds the OS shell -- and copies the output to
`/app/vscode-site`. A seeded, system-owned site row
([`dsl/platform/seeds.memql`](../../../dsl/platform/seeds.memql), bundleRef
`file:///app/vscode-site`) names that directory, and the edge serves it at
`vscode.<domain>` -- locally `https://vscode.memql.localhost/` after `make up`.
Nothing is published by hand: a change to the page ships with the next edge
image, and the site row is seeded on boot like the OS shell's.

`make vscode-site-build` is the local build: the same `npm run build && npm run
check` the image stage runs, so a locally built `dist/` and the image bundle
cannot differ in how they were produced. `dist/` has `index.html` at the root
and follows the repository's
[site hosting contract](../../../docs/public/operate/site-hosting.md); relative
asset paths mean it also serves from a static host subdirectory with a trailing
slash, and no SPA fallback is required.

The build needs the repository root as its context because it reads `brand/`,
`editors/vscode/themes/`, and `examples/research-desk/`; the `spa-build` stage
copies exactly those trees, and `scripts/ci/spa_image_wiring_test.go` derives
the list from `build.mjs` so a new read outside `editors/vscode/site` fails a
test before it fails a release cut.

Use external scripts and same-origin assets; the page is compatible with the
edge's script/style/font policy without adding inline-script permission. The
preview server is only a local inspection tool, not the deployment server.

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
