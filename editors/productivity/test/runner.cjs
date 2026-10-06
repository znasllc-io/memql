const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const esbuild = require("esbuild");
const cp = require("node:child_process");
async function main() {
  const root = path.resolve(__dirname, "..");
  const core = path.resolve(root, "../vscode");
  const fixture = fs.mkdtempSync(path.join(os.tmpdir(), "memql-editor-host-"));
  const suite = path.join(root, "dist-test/host.js");
  const diagnostics = path.join(root, "dist-test/host-logs");
  fs.rmSync(diagnostics, { recursive: true, force: true });
  let passed = false;
  await esbuild.build({ entryPoints: [path.join(__dirname, "host.ts")], outfile: suite, bundle: true,
    platform: "browser", format: "cjs", target: "es2022", external: ["vscode"] });
  try {
    if (process.argv.includes("--desktop")) {
      const { runTests, downloadAndUnzipVSCode } = require(path.join(core, "node_modules/@vscode/test-electron"));
      const version = process.env.MEMQL_VSCODE_VERSION || "stable";
      let executable = await downloadAndUnzipVSCode({ version });
      if (process.platform === "darwin" && !fs.existsSync(executable)) {
        const contents = path.dirname(path.dirname(executable));
        const name = cp.execFileSync("/usr/libexec/PlistBuddy", ["-c", "Print CFBundleExecutable", path.join(contents,"Info.plist")],{encoding:"utf8"}).trim();
        executable = path.join(contents,"MacOS",name);
      }
      await runTests({ version, vscodeExecutablePath: executable, extensionDevelopmentPath: [core, root], extensionTestsPath: suite,
        extensionTestsEnv: { MEMQL_EDITOR_TEST_STATE_DIR: fixture },
        // Desktop CI may leave the test window occluded or without focus.
        // Match browser automation's foreground scheduling for these isolated
        // test profiles; keep every render assertion and deadline unchanged.
        launchArgs: [fixture, "--disable-workspace-trust", "--disable-gpu", "--no-sandbox", "--skip-welcome", "--skip-release-notes",
          "--disable-renderer-backgrounding", "--disable-backgrounding-occluded-windows", "--disable-background-timer-throttling",
          "--user-data-dir", path.join(fixture,"user-data"), "--extensions-dir", path.join(fixture,"extensions")] });
    } else {
      const { runTests } = require(path.join(core, "node_modules/@vscode/test-web"));
      await runTests({ quality: "stable", browserType: "chromium", headless: true, extensionDevelopmentPath: root,
        extensionPaths: [core], extensionTestsPath: suite, folderPath: fixture, port: 3217 });
    }
    passed = true;
  } finally {
    if (!passed) {
      const logs = path.join(fixture, "user-data/logs");
      if (fs.existsSync(logs)) {
        try {
          fs.cpSync(logs, diagnostics, { recursive: true });
          console.error(`Editor host logs preserved in ${diagnostics}`);
        } catch (error) { console.error("Could not preserve editor host logs:", error); }
      }
    }
    fs.rmSync(fixture, { recursive: true, force: true });
  }
}
main().catch(error => { console.error(error); process.exit(1); });
