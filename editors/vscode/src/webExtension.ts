// VS Code browser adapter: shared connection/authentication logic, no native
// process, disk, loopback listener, or LSP executable in this bundle.
import * as vscode from "vscode";
import { ConnectionManager, DEFAULT_RECONNECT_DELAYS_MS } from "./connection/managerCore.js";
import { CredentialResolver } from "./connection/credentials.js";
import { BrowserRegistry } from "./clusters/browserRegistry.js";
import { EditorConnection, validateEditorDomain } from "./connection/api.js";
import { persistSignIn, signOut } from "./auth/store.js";
import { runDeviceCodeFlow, deviceCodeOpenTarget } from "./auth/deviceGrant.js";
import { revokeRefreshToken, signOutMessage } from "./auth/revoke.js";
import type { ClusterConfig } from "./clusters/model.js";

export function activate(context: vscode.ExtensionContext) {
  const registry = new BrowserRegistry(context.globalState, context.secrets);
  const store = { secrets: context.secrets, writeCluster: registry.write };
  let selected: ClusterConfig | undefined;
  let signInAbort: AbortController | undefined;
  let selection = 0;
  const stateChanged = new vscode.EventEmitter<string | undefined>();
  const manager = new ConnectionManager(undefined, new CredentialResolver({
    secrets: context.secrets, persist: (name, update) => registry.write({ name, token: update.token }),
  }), store, keys => {
    void vscode.commands.executeCommand("setContext", "memql.clusterSelected", keys.clusterSelected);
    void vscode.commands.executeCommand("setContext", "memql.connected", keys.connected);
    void vscode.commands.executeCommand("setContext", "memql.connectionState", keys.connectionState);
  }, { delaysMs: DEFAULT_RECONNECT_DELAYS_MS, reload: name => registry.read(name) });
  const connection = new EditorConnection({
    session: () => selected && manager.state.status === "connected" && manager.query && manager.bearer
      ? { cluster: selected, query: manager.query, bearer: manager.bearer } : undefined,
    connect: async domain => {
      requireTrust();
      if (selected?.domain !== domain) {
        const answer = await vscode.window.showInformationMessage(`Connect MemQL to ${domain}?`, { modal: true }, "Connect");
        if (answer !== "Connect") return;
      }
      await select(domain);
      if (manager.state.status === "error" && manager.state.reason !== "unreachable") await signIn();
    },
  });
  const status = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 20);
  status.command = "memql.clusters.select";
  const unsubscribe = manager.onDidChangeState(state => {
    connection.changed();
    status.text = state.status === "connected" ? `$(plug) ${state.clusterName}` : "$(plug) MemQL";
    status.tooltip = state.status === "error" ? state.message : `MemQL: ${state.status}`;
    stateChanged.fire(undefined);
  });
  status.text = "$(plug) MemQL";
  status.show();
  context.subscriptions.push(status, stateChanged, { dispose: unsubscribe }, { dispose() { signInAbort?.abort(); void manager.disconnect(); } });

  function requireTrust(): void {
    if (!vscode.workspace.isTrusted) throw new Error("Trust this workspace before connecting MemQL.");
  }
  async function select(domain: string): Promise<void> {
    requireTrust();
    const ticket = ++selection;
    signInAbort?.abort();
    await manager.disconnect();
    if (ticket !== selection) return;
    await registry.select(domain);
    const cluster = await registry.read(domain);
    if (ticket !== selection) return;
    selected = cluster;
    await manager.connect(cluster);
    stateChanged.fire(undefined);
  }
  async function choose(): Promise<void> {
    requireTrust();
    const value = await vscode.window.showQuickPick([...registry.domains(), "Add cluster…"], { title: "Connect to a MemQL cluster" });
    if (value === "Add cluster…") return add();
    if (value) await select(value);
  }
  async function add(): Promise<void> {
    requireTrust();
    const domain = await vscode.window.showInputBox({ title: "Cluster domain", placeHolder: "memql.example.com",
      validateInput: value => { try { validateEditorDomain(value); return undefined; } catch (e) { return (e as Error).message; } } });
    if (domain) { await select(validateEditorDomain(domain)); await signIn(); }
  }
  async function signIn(): Promise<void> {
    requireTrust();
    if (!selected) { await choose(); if (!selected) return; }
    signInAbort?.abort();
    const attempt = new AbortController();
    signInAbort = attempt;
    const cluster = selected;
    const ticket = selection;
    await vscode.window.withProgress({ location: vscode.ProgressLocation.Notification, title: `Sign in to ${cluster.name}`, cancellable: true }, async (progress, cancellation) => {
      const cancel = cancellation.onCancellationRequested(() => attempt.abort());
      try {
        const tokens = await runDeviceCodeFlow(cluster, {
          signal: attempt.signal,
          onUserCode: authorization => {
            progress.report({ message: `Approve code ${authorization.userCode} in your browser.` });
            void vscode.env.openExternal(vscode.Uri.parse(deviceCodeOpenTarget(authorization)));
          },
        });
        if (attempt.signal.aborted || selected?.domain !== cluster.domain) return;
        await persistSignIn(store, cluster.name, tokens);
        const authenticated = await registry.read(cluster.name);
        if (attempt.signal.aborted || selection !== ticket) return;
        selected = authenticated;
        await manager.connect(authenticated);
      } finally { cancel.dispose(); if (signInAbort === attempt) signInAbort = undefined; }
    });
  }
  async function disconnect(): Promise<void> { selection++; signInAbort?.abort(); await manager.disconnect(); }
  async function signOutSelected(): Promise<number | undefined> {
    requireTrust();
    if (!selected) return;
    const cluster = selected;
    const ticket = ++selection;
    signInAbort?.abort();
    await manager.disconnect();
    const revocation = await signOut(store, cluster.name, refresh => revokeRefreshToken(cluster.issuer!, refresh, (url, init) => fetch(url, { ...init, credentials: "omit", redirect: "error" })));
    const signedOut = await registry.read(cluster.name);
    if (selection === ticket) selected = signedOut;
    void vscode.window.showInformationMessage(signOutMessage(cluster.name, revocation));
    return ticket;
  }
  const commands: Record<string, (...args: any[]) => unknown> = {
    "memql.clusters.add": add,
    "memql.clusters.select": choose,
    "memql.clusters.signIn": signIn,
    "memql.clusters.signInWithCode": signIn,
    "memql.deployments.signIn": signIn,
    "memql.clusters.disconnect": disconnect,
    "memql.clusters.signOut": signOutSelected,
    "memql.clusters.refresh": () => stateChanged.fire(undefined),
    "memql.clusters.connection": () => vscode.window.showInformationMessage(selected ? `${selected.domain}: ${manager.state.status}` : "Connect to a cluster first."),
    "memql.clusters.openConsole": () => selected && vscode.env.openExternal(vscode.Uri.parse(`https://os.${selected.domain}`)),
    "memql.clusters.remove": async () => {
      if (!selected) return;
      const domain = selected.name;
      const ticket = await signOutSelected();
      await registry.remove(domain);
      if (selection === ticket) selected = undefined;
      connection.changed(); stateChanged.fire(undefined);
    },
  };
  for (const [name, fn] of Object.entries(commands)) context.subscriptions.push(vscode.commands.registerCommand(name, (...args) =>
    Promise.resolve().then(() => fn(...args)).catch(err => vscode.window.showErrorMessage(`MemQL: ${(err as Error).message}`))));
  context.subscriptions.push(vscode.window.registerTreeDataProvider("memqlClusters", {
    onDidChangeTreeData: stateChanged.event,
    getChildren: () => registry.domains(),
    getTreeItem: domain => ({ label: domain, description: selected?.name === domain ? manager.state.status : undefined,
      command: { command: "memql.web.connect", title: "Connect", arguments: [domain] }, iconPath: new vscode.ThemeIcon("server") }),
  }), vscode.commands.registerCommand("memql.web.connect", (domain: string) => select(domain).catch(e => vscode.window.showErrorMessage(String(e)))));
  if (vscode.workspace.isTrusted && registry.selected()) void select(registry.selected()).catch(() => {});
  return { connection };
}
