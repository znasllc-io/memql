// Interactive installer QA in a real Extension Development Host. Only the
// capability-process boundary is simulated; forms, messages, graph execution,
// receipts, history and cleanup use the shipped controller. Never runs Docker,
// changes hosts/trust stores, or writes to the operator's install directory.
// Bundle with esbuild (vscode external), then pass --extensionTestsPath.
import * as vscode from "vscode";
import * as fs from "node:fs/promises";
import * as path from "node:path";
import { configureRegistryWriter, nativeRegistryWriter } from "../src/clusters/atomicWrite.js";
import { AddClusterPanel } from "../src/webview/addClusterPanel.js";
import { ClusterPresence } from "../src/clusters/presence.js";
import { loadGraphFile, type Verify } from "../src/install/graph.js";
import type { RunScript } from "../src/install/runner.js";
import { LocalRuns } from "../src/deploy/localRun.js";

export async function run(): Promise<void> {
  const stateDir = process.env.MEMQL_EDITOR_TEST_STATE_DIR;
  const root = process.env.MEMQL_INSTALL_QA_ROOT;
  if (!stateDir || !path.isAbsolute(stateDir) || !root) throw new Error("Explicit isolated QA paths are required");
  await fs.mkdir(stateDir, { recursive: true });
  configureRegistryWriter(nativeRegistryWriter(path.join(root, "editors/vscode/bin", `${process.platform}-${process.arch}`, "memql-lsp")));
  await vscode.extensions.getExtension("znasllc.memql")!.activate();
  await vscode.commands.executeCommand("workbench.view.extension.memql");
  const verify = new Map<string, Verify>();
  for (const name of ["install.json", "install-main.json", "uninstall.json"]) {
    const graph = await loadGraphFile(path.join(root, "scripts/install/graph", name));
    for (const step of graph.steps) if (!verify.has(step.script)) verify.set(step.script, step.verify);
  }
  const receiptFile = path.join(stateDir, "install-receipt.json");
  const clustersPath = path.join(stateDir, "clusters.yaml");
  const output = vscode.window.createOutputChannel("MemQL Installer QA");
  let failBootstrap = true;
  const runScript: RunScript = async (request) => {
    const capability = request.capability!;
    const condition = verify.get(capability);
    if (!condition) throw new Error(`Unrecognized QA capability: ${capability}`);
    await fs.appendFile(path.join(stateDir, "calls.jsonl"), JSON.stringify({ capability, params: request.params, scriptPath: request.scriptPath }) + "\n");
    request.onLog?.(`QA fixture: ${capability}`);
    await new Promise(resolve => setTimeout(resolve, capability === "k3d.up" ? 3000 : 600));
    const failed = capability === "k3d.up" && failBootstrap;
    if (failed) failBootstrap = false;
    const field = (condition.field ?? "").replace(/^result\./, "");
    const result: Record<string, unknown> = condition.kind === "resultTrue" ? { [field]: true }
      : condition.kind === "resultFalse" ? { [field]: false }
      : condition.kind === "resultNonEmpty" ? { [field]: "qa" }
      : condition.kind === "resultEquals" ? { [field]: condition.value } : {};
    Object.assign(result, { preExisting: false, ownerAccountExists: true });
    if (capability === "install.cloneStack") Object.assign(result, { dest: root, ref: request.params.branch ?? request.params.tag, refKind: request.params.branch ? "branch" : "tag", commit: "a".repeat(40) });
    if (capability === "install.binary") Object.assign(result, { path: path.join(stateDir, request.params.tool), installed: true });
    if (capability === "k3d.up") Object.assign(result, { cluster: "memql", argocdReady: !failed, workloadsReady: !failed });
    if (capability === "install.removeArtifact") Object.assign(result, { kind: request.params.kind, removed: true });
    const message = "ComparisonError: git fetch failed: RPC failed; curl 56 Recv failure: Connection reset by peer; fatal: early EOF";
    if (failed) request.onLog?.(message);
    return { argv: [], exitCode: failed ? 5 : 0, signal: null, stdout: "", stderr: failed ? message : "",
      envelope: { ok: !failed, capability, changed: !failed, result, error: failed ? { code: 5, message } : null } };
  };
  const presence = new ClusterPresence({ clustersPath, receiptPath: receiptFile, probe: async () => true, listClusters: async () => [] });
  const openInstaller = () => AddClusterPanel.show({ subscriptions: [] } as unknown as vscode.ExtensionContext, presence, {
    clustersPath, receiptFile, installRoot: root, runsDir: path.join(stateDir, "runs"),
    refreshTree: () => { void vscode.commands.executeCommand("memql.clusters.refresh"); },
    diagnostics: output, showDiagnostics: () => output.show(true), runScript,
    sudoIsFree: async () => true, sudoAccepts: async () => true,
    listLocalClusters: async () => [], runs: new LocalRuns(),
    removeRegistryEntry: async () => { await fs.rm(clustersPath, { force: true }); },
  });
  vscode.commands.registerCommand("memql.qa.openInstaller", openInstaller);
  openInstaller();
  // Keep the host open for clicks, keyboard navigation and native webview QA.
  await new Promise<void>(() => {});
}
