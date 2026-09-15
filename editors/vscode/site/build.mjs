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
const assets = ["index.html", "style.css", "site.js"];

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
    path.join(root, "examples/reading-list/reading.memql"),
    "utf8",
  );
  if (
    (await readFile(path.join(output, "reading.memql"), "utf8")) !== example
  ) {
    throw new Error("Download differs from canonical example; rebuild");
  }
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
  const sizes = await walk(output);
  return {
    files: sizes.length,
    bytes: sizes.reduce((a, b) => a + b, 0),
    output,
  };
}

async function build() {
  // dist is the generated artifact, never source or browser-test evidence.
  await rm(output, { recursive: true, force: true });
  await mkdir(output, { recursive: true });
  for (const name of assets)
    await cp(path.join(source, name), path.join(output, name));
  await mkdir(path.join(output, "brand"), { recursive: true });
  // Bundle canonical brand assets at build time; never fork their source definitions.
  for (const name of ["fonts.css", "fonts", "mark.svg", "favicon.svg"]) {
    await cp(path.join(root, "brand", name), path.join(output, "brand", name), {
      recursive: true,
    });
  }
  // The canonical mark is an HTML-ready SVG fragment. Its prose comments contain
  // double hyphens, which XML image decoders reject. Strip comments only for the
  // standalone image; preserve the canonical geometry and source file.
  const mark = await readFile(path.join(root, "brand/mark.svg"), "utf8");
  await writeFile(
    path.join(output, "brand/mark.svg"),
    mark.replace(/<!--[\s\S]*?-->/g, ""),
  );
  const example = await readFile(
    path.join(root, "examples/reading-list/reading.memql"),
    "utf8",
  );
  await writeFile(path.join(output, "reading.memql"), example);
  const pieces = example.trim().split(/\n\n(?=\/\/\/)/);
  if (pieces.length !== 4)
    throw new Error(
      "Expected concept, mutation, query and tool in the example",
    );
  await writeFile(
    path.join(output, "example-data.js"),
    `export const examples = ${JSON.stringify(pieces, null, 2)};\n`,
  );
  await cp(
    path.join(root, "examples/reading-list/memql.toml"),
    path.join(output, "memql.toml"),
  );
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
