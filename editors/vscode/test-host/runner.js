#!/usr/bin/env node
// Downloads a real VS Code, launches it as an Extension Development Host, and
// runs the smoke lane inside it (memql#3302).
//
// PLAIN COMMONJS, NOT TYPESCRIPT, and deliberately so: this file runs in
// ordinary Node OUTSIDE the extension host, imports nothing but Node builtins
// and @vscode/test-electron, and never touches `vscode` or the SDK. It is the
// one piece of this lane that has no reason to go through the bundler, and
// keeping it out of the bundle keeps the "what runs where" boundary legible --
// everything under dist-host/ runs inside Electron, this does not.
//
// The tests themselves ARE bundled (esbuild.host.js), for the same reason
// esbuild.test.js bundles the unit tests: @znasllc-io/memql-sdk-core is pure
// ESM and the extension host is CommonJS, so a raw tsc emit would hit
// ERR_REQUIRE_ESM the moment a test imported anything that reaches the SDK.
const cp = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const { runTests } = require("@vscode/test-electron");

const EXT_ROOT = path.resolve(__dirname, "..");
const TESTS_ENTRY = path.join(EXT_ROOT, "dist-host", "test-host", "index.js");

// Node's platform/arch spelling, which is what resolveServerPath() in
// src/extension.ts looks under -- NOT Go's GOOS/GOARCH. scripts/vscode's
// packaging path stages the binary under the same name for the same reason.
function bundledServerPath() {
  const name = process.platform === "win32" ? "memql-lsp.exe" : "memql-lsp";
  return path.join(EXT_ROOT, "bin", `${process.platform}-${process.arch}`, name);
}

function serverOnPath() {
  const probe = process.platform === "win32" ? "where" : "which";
  const result = cp.spawnSync(probe, ["memql-lsp"], { encoding: "utf8" });
  return result.status === 0 && String(result.stdout).trim() !== "";
}

// Without a resolvable server binary, activate() shows its "binary not found"
// message and RETURNS -- before registering a single command, view, or
// watcher. The lane would then report one green activation case and five
// failures whose real cause is a missing build artifact. Refuse up front and
// say what to do instead.
function requireServerBinary() {
  if (fs.existsSync(bundledServerPath()) || serverOnPath()) {
    return;
  }
  console.error(
    [
      "ERROR: memql-lsp not found, so the extension would abort activation before registering anything.",
      `  looked for: ${bundledServerPath()}`,
      "  and:        memql-lsp on PATH",
      "",
      "  Build and stage it with:",
      `    go build -o ${bundledServerPath()} ./cmd/memql-lsp`,
      "  or run the whole lane through: make vscode-test-host",
    ].join("\n")
  );
  process.exit(1);
}

// A throwaway workspace. It must exist and must NOT contain the temp HOME:
// the watcher case asserts that ~/.memql/clusters.yaml is outside every
// workspace folder, which is the entire point of that case.
function makeWorkspace(root) {
  const workspace = path.join(root, "workspace");
  fs.mkdirSync(workspace, { recursive: true });
  fs.writeFileSync(
    path.join(workspace, "smoke.memql"),
    "// Opened so the language client has something in scope.\n",
    "utf8"
  );
  return workspace;
}

async function main() {
  requireServerBinary();

  if (!fs.existsSync(TESTS_ENTRY)) {
    console.error(
      `ERROR: ${TESTS_ENTRY} does not exist. Run \`node esbuild.host.js\` first (npm run test:host does).`
    );
    process.exit(1);
  }

  const root = fs.mkdtempSync(path.join(os.tmpdir(), "memql-vscode-host-"));
  const workspace = makeWorkspace(root);
  const home = path.join(root, "home");
  const userData = path.join(root, "user-data");
  const extensions = path.join(root, "extensions");
  for (const dir of [home, userData, extensions]) {
    fs.mkdirSync(dir, { recursive: true });
  }

  try {
    await runTests({
      version: process.env.MEMQL_VSCODE_VERSION || "stable",
      extensionDevelopmentPath: EXT_ROOT,
      extensionTestsPath: TESTS_ENTRY,
      launchArgs: [
        workspace,
        // Every other installed extension stays out of the window. The
        // extension under development is unaffected by this flag.
        "--disable-extensions",
        // The runtime surface is trust-gated, so an untrusted window would
        // register nothing and the lane would assert nothing. index.ts fails
        // loudly if this ever stops taking effect.
        "--disable-workspace-trust",
        // CI runners have no GPU and containers commonly have no usable
        // sandbox; both are Electron startup failures rather than test
        // failures, so they are ruled out here.
        "--disable-gpu",
        "--no-sandbox",
        "--disable-updates",
        "--skip-welcome",
        "--skip-release-notes",
        "--user-data-dir",
        userData,
        "--extensions-dir",
        extensions,
      ],
      extensionTestsEnv: {
        // The isolation the watcher case depends on. defaultClustersPath()
        // resolves through os.homedir(), which reads HOME on POSIX, so the
        // lane writes to a temp ~/.memql/clusters.yaml and never touches the
        // developer's real cluster registry.
        HOME: home,
        USERPROFILE: home,
        MEMQL_HOST_SMOKE_HOME: home,
      },
    });
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
}

main().catch((err) => {
  console.error("ERROR: the host smoke lane failed");
  console.error(err instanceof Error ? (err.stack ?? err.message) : String(err));
  process.exit(1);
});
