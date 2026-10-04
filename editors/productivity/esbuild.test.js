const esbuild = require("esbuild");
const fs = require("node:fs");
esbuild.buildSync({ entryPoints: ["src/markdownView.ts"], outfile: "dist-test/markdown-view.js", bundle: true, platform: "browser", format: "iife", target: "es2022" });
esbuild.build({ entryPoints: fs.readdirSync("test").filter(f => f.endsWith(".test.ts")).map(f => `test/${f}`),
  outdir: "dist-test", alias: { vscode: require("node:path").resolve("test/support/vscode.ts") }, external: ["jsdom"], bundle: true, platform: "node", format: "cjs", target: "node20", logLevel: "info",
}).catch(e => { console.error(e); process.exit(1); });
