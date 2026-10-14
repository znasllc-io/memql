const esbuild = require("esbuild");
const fs = require("node:fs");
for (const file of fs.existsSync("dist-test") ? fs.readdirSync("dist-test") : []) if (file.endsWith(".test.js")) fs.rmSync(`dist-test/${file}`);
esbuild.buildSync({ entryPoints: ["src/markdownView.ts"], outfile: "dist-test/markdown-view.js", bundle: true, platform: "browser", format: "iife", target: "es2022" });
esbuild.buildSync({ entryPoints: ["src/emailView.ts"], outfile: "dist-test/email-view.js", bundle: true, platform: "browser", format: "iife", target: "es2022" });
esbuild.buildSync({ entryPoints: ["src/newEmailView.ts"], outfile: "dist-test/new-email-view.js", bundle: true, platform: "browser", format: "iife", target: "es2022" });
esbuild.build({ entryPoints: fs.readdirSync("test").filter(f => f.endsWith(".test.ts")).map(f => `test/${f}`),
  outdir: "dist-test", alias: { vscode: require("node:path").resolve("test/support/vscode.ts") }, external: ["jsdom"], bundle: true, platform: "node", format: "cjs", target: "node20", logLevel: "info",
}).catch(e => { console.error(e); process.exit(1); });
