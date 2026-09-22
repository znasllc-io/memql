import {
  cp,
  rm,
  mkdir,
  readFile,
  writeFile,
  readdir,
  stat,
} from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";

const source = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(source, "../../..");
const output = path.join(source, "dist");
const assets = ["index.html", "style.css", "site.js", "appearance.js"];

async function check() {
  const html = await readFile(path.join(output, "index.html"), "utf8");
  const ids = [...html.matchAll(/\bid="([^"]+)"/g)].map((match) => match[1]);
  if (new Set(ids).size !== ids.length) throw new Error("Duplicate HTML id");
  for (const [, url] of html.matchAll(/(?:href|src)="([^"]+)"/g)) {
    if (/^https:\/\//.test(url)) continue;
    if (url.startsWith("#")) {
      if (!ids.includes(url.slice(1)))
        throw new Error(`Missing anchor: ${url}`);
      continue;
    }
    const file = path.resolve(output, url);
    if (!file.startsWith(output + path.sep))
      throw new Error(`Asset escapes bundle: ${url}`);
    await stat(file);
  }
  const example = await readFile(
    path.join(root, "examples/research-desk/research/brief.memql"),
    "utf8",
  );
  if ((await readFile(path.join(output, "brief.memql"), "utf8")) !== example) {
    throw new Error("Download differs from canonical example; rebuild");
  }
  const expectedData = showcaseModule(example);
  if (
    (await readFile(path.join(output, "example-data.js"), "utf8")) !==
    expectedData
  )
    throw new Error("Showcase excerpts differ from canonical source; rebuild");
  if (
    (await readFile(path.join(output, "memql.toml"), "utf8")) !==
    (await readFile(
      path.join(root, "examples/research-desk/research/memql.toml"),
      "utf8",
    ))
  )
    throw new Error("Language manifest differs from canonical source; rebuild");
  for (const name of assets) {
    if (
      (await readFile(path.join(output, name), "utf8")) !==
      (await readFile(path.join(source, name), "utf8"))
    ) {
      throw new Error(`Stale output: ${name}; rebuild`);
    }
  }
  if (/\bon\w+=|<script(?![^>]*\bsrc=)/i.test(html))
    throw new Error("Inline script/handler is incompatible with the edge CSP");
  const walk = async (dir) =>
    (
      await Promise.all(
        (await readdir(dir, { withFileTypes: true })).map(async (entry) =>
          entry.isDirectory()
            ? walk(path.join(dir, entry.name))
            : [(await stat(path.join(dir, entry.name))).size],
        ),
      )
    ).flat();
  if (
    (await readFile(path.join(output, "editor-theme.css"), "utf8")) !==
    (await editorThemeCSS())
  )
    throw new Error("Editor theme preview is stale; rebuild");
  if (
    (await readFile(path.join(output, "brand/tokens.css"), "utf8")) !==
    (await readFile(path.join(root, "brand/tokens.css"), "utf8"))
  )
    throw new Error("Canonical brand tokens are stale; rebuild");
  if (
    (await readFile(path.join(output, "research-guide.md"), "utf8")) !==
    (await researchGuide())
  )
    throw new Error("Setup guide differs from the canonical example; rebuild");
  const sizes = await walk(output);
  return {
    files: sizes.length,
    bytes: sizes.reduce((a, b) => a + b, 0),
    output,
  };
}

async function editorThemeCSS() {
  const roles = {
    bg: "editor.background",
    fg: "editor.foreground",
    rail: "sideBar.background",
    line: "widget.border",
  };
  const themes = await Promise.all(
    ["light", "dark"].map(async (variant) =>
      JSON.parse(
        await readFile(
          path.join(source, `../themes/memql-${variant}-color-theme.json`),
          "utf8",
        ),
      ),
    ),
  );
  const values = themes.map((theme) => ({
    ...Object.fromEntries(
      Object.entries(roles).map(([role, key]) => [role, theme.colors[key]]),
    ),
    comment: theme.tokenColors.find((rule) => rule.name === "Comment").settings
      .foreground,
    string: theme.tokenColors.find((rule) => rule.name === "String").settings
      .foreground,
    number: theme.tokenColors.find(
      (rule) => rule.name === "Number and language constant",
    ).settings.foreground,
    keyword: theme.tokenColors.find(
      (rule) => rule.name === "Keyword, storage and annotation",
    ).settings.foreground,
  }));
  return (
    "/* Generated from the contributed MemQL editor themes. */\n:root {\n" +
    Object.keys(values[0])
      .map(
        (role) =>
          `  --editor-${role}: light-dark(${values[0][role]}, ${values[1][role]});`,
      )
      .join("\n") +
    "\n}\n"
  );
}

function showcasePieces(
  example,
  names = ["search", "ai", "cache", "automation"],
) {
  return names.map((name) => {
    const start = `// showcase:${name}:start\n`;
    const end = `// showcase:${name}:end`;
    const chunk = example.split(start)[1]?.split(end)[0]?.trim();
    if (!chunk || !example.includes(end))
      throw new Error(`Missing showcase region: ${name}`);
    return chunk;
  });
}

function showcaseModule(example) {
  return (
    `export const examples = ${JSON.stringify(showcasePieces(example), null, 2)};\n` +
    `export const coreExamples = ${JSON.stringify(showcasePieces(example, ["model", "predicates", "write"]), null, 2)};\n`
  );
}

async function researchGuide() {
  const text = await readFile(
    path.join(root, "examples/research-desk/README.md"),
    "utf8",
  );
  return text
    .replaceAll("(research/brief.memql)", "(brief.memql)")
    .replaceAll("(research/memql.toml)", "(memql.toml)")
    .replaceAll("](../../", "](https://github.com/znasllc-io/memql/blob/main/");
}

async function build() {
  // dist is the generated artifact, never source or browser-test evidence.
  await rm(output, { recursive: true, force: true });
  await mkdir(output, { recursive: true });
  for (const name of assets)
    await cp(path.join(source, name), path.join(output, name));
  await mkdir(path.join(output, "brand"), { recursive: true });
  // Bundle canonical brand assets at build time; never fork their source definitions.
  for (const name of [
    "fonts.css",
    "tokens.css",
    "fonts",
    "mark.svg",
    "favicon.svg",
  ]) {
    await cp(path.join(root, "brand", name), path.join(output, "brand", name), {
      recursive: true,
    });
  }
  await writeFile(
    path.join(output, "editor-theme.css"),
    await editorThemeCSS(),
  );
  // The canonical mark is an HTML-ready SVG fragment. Its prose comments contain
  // double hyphens, which XML image decoders reject. Strip comments only for the
  // standalone image; preserve the canonical geometry and source file.
  const mark = await readFile(path.join(root, "brand/mark.svg"), "utf8");
  await writeFile(path.join(output, "brand/mark.svg"), withoutComments(mark));
  const example = await readFile(
    path.join(root, "examples/research-desk/research/brief.memql"),
    "utf8",
  );
  await writeFile(path.join(output, "brief.memql"), example);
  await writeFile(
    path.join(output, "research-guide.md"),
    await researchGuide(),
  );
  await writeFile(
    path.join(output, "example-data.js"),
    showcaseModule(example),
  );
  await cp(
    path.join(root, "examples/research-desk/research/memql.toml"),
    path.join(output, "memql.toml"),
  );
}

/**
 * Every XML comment removed, not merely one pass of them.
 *
 * A SINGLE `replace(/<!--[\s\S]*?-->/g, "")` IS NOT A REMOVAL. The match is
 * non-greedy, so `<!--<!---->` loses the inner `<!---->` and leaves a bare
 * `<!--` behind -- the class of defect CodeQL calls
 * `js/incomplete-multi-character-sanitization`, and the reason the output can
 * still be the thing the strip existed to avoid. Repeating to a fixed point is
 * the removal: the string shrinks on every pass that changes it, so it
 * terminates, and it terminates only when no comment opener survives.
 *
 * The input here is this repository's own `brand/mark.svg` rather than
 * anything a request carries, so nothing hostile reaches it today. That is an
 * argument about the caller, not about the function, and the caller is a build
 * step somebody will point at a second asset.
 */
function withoutComments(svg) {
  let stripped = svg;
  for (let previous = ""; previous !== stripped; ) {
    previous = stripped;
    stripped = stripped.replace(/<!--[\s\S]*?-->/g, "");
  }
  return stripped;
}

async function main() {
  const args = process.argv.slice(2);
  if (args.length === 1 && args[0] === "--help") {
    console.log(
      "Usage: node build.mjs [--check|--help]\nBuild a static bundle in dist, or check an existing build. No deployment occurs.",
    );
    return;
  }
  if (args.length > 1 || args.some((arg) => arg !== "--check")) {
    console.log(
      JSON.stringify({
        ok: false,
        error: "Expected --check, --help, or no flags",
      }),
    );
    process.exitCode = 2;
    return;
  }
  if (!args.includes("--check")) await build();
  console.log(JSON.stringify({ ok: true, ...(await check()) }));
}
main().catch((error) => {
  console.error(error.message);
  console.log(JSON.stringify({ ok: false, error: error.message }));
  process.exitCode = 5;
});
