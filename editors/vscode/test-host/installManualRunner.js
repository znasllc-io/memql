#!/usr/bin/env node
// Opens the main extension's interactive installer fixture in a private profile.
// Close this VS Code window when finished. No live installer scripts are run.
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const cp = require("node:child_process");
const { runTests, downloadAndUnzipVSCode } = require("@vscode/test-electron");

async function main() {
  const extension = path.resolve(__dirname, "..");
  const repo = path.resolve(extension, "../..");
  const binary = path.join(extension, "bin", `${process.platform}-${process.arch}`, "memql-lsp");
  cp.execFileSync("go", ["build", "-o", binary, "./cmd/memql-lsp"], { cwd: repo, stdio: "inherit" });
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "memql-installer-ui-"));
  const workspace = path.join(root, "workspace");
  const userData = path.join(root, "user-data");
  fs.mkdirSync(workspace);
  fs.mkdirSync(path.join(userData, "User"), { recursive: true });
  fs.writeFileSync(path.join(workspace, "qa.memql"), "// Disposable installer UI fixture.\n");
  fs.writeFileSync(path.join(userData, "User/settings.json"), JSON.stringify({
    "update.mode": "none", "telemetry.telemetryLevel": "off", "workbench.startupEditor": "none",
  }));
  fs.writeFileSync(path.join(userData, "User/keybindings.json"), JSON.stringify([
    { key: "ctrl+alt+i", command: "memql.qa.openInstaller" },
  ]));
  const installed = process.platform === "darwin" ? "/Applications/Visual Studio Code.app/Contents/MacOS/Electron" : "";
  const vscodeExecutablePath = process.env.MEMQL_VSCODE_EXECUTABLE ||
    (fs.existsSync(installed) ? installed : await downloadAndUnzipVSCode("stable"));
  console.log(`Installer UI fixture: ${root}\nCtrl+Alt+I reopens the fixture. The first bootstrap fails; Retry succeeds.`);
  try {
    await runTests({ vscodeExecutablePath, extensionDevelopmentPath: extension,
      extensionTestsPath: path.join(extension, "dist-host/test-host/installManual.js"),
      launchArgs: [workspace, "--new-window", "--disable-extensions", "--disable-workspace-trust", "--skip-welcome", "--skip-release-notes",
        "--user-data-dir", userData, "--extensions-dir", path.join(root, "extensions")],
      extensionTestsEnv: { MEMQL_EDITOR_TEST_STATE_DIR: path.join(root, "state"), MEMQL_INSTALL_QA_ROOT: repo },
    });
  } finally { fs.rmSync(root, { recursive: true, force: true }); }
}
main().catch(err => { console.error(err); process.exitCode = 1; });
