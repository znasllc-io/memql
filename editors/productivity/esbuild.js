const esbuild = require("esbuild");
const fs = require("node:fs");
async function main() {
  fs.copyFileSync("../../LICENSE", "LICENSE");
  fs.rmSync("out", { recursive: true, force: true });
  await Promise.all([
    esbuild.build({ entryPoints: ["src/extension.ts"], outfile: "out/extension.js", bundle: true, platform: "node", format: "cjs", target: "node20", external: ["vscode"], logLevel: "info" }),
    esbuild.build({ entryPoints: ["src/extension.ts"], outfile: "out/webExtension.js", bundle: true, platform: "browser", format: "cjs", target: "es2022", external: ["vscode"], logLevel: "info", metafile: true }).then(r => fs.writeFileSync("out/web-meta.json", JSON.stringify(r.metafile))),
    esbuild.build({ entryPoints: ["src/pdfView.ts", "src/markdownView.ts", "src/templateView.ts"], outdir: "out", bundle: true, platform: "browser", format: "iife", target: "es2022", logLevel: "info" }),
  ]);
  fs.copyFileSync("node_modules/pdfjs-dist/legacy/build/pdf.worker.mjs", "out/pdf.worker.mjs");
  for (const assets of ["cmaps", "standard_fonts", "wasm"]) fs.cpSync(`node_modules/pdfjs-dist/${assets}`, `out/${assets}`, { recursive: true });
  fs.copyFileSync("node_modules/pdfjs-dist/LICENSE", "out/PDFJS-LICENSE");
  fs.copyFileSync("node_modules/pdf-lib/LICENSE.md", "out/PDFLIB-LICENSE");
  fs.copyFileSync("node_modules/markdown-it/LICENSE", "out/MARKDOWNIT-LICENSE");
}
main().catch(e => { console.error(e); process.exit(1); });
