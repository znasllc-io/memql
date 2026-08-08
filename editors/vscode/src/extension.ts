import * as fs from 'fs';
import * as path from 'path';
import { commands, ExtensionContext, window, workspace } from 'vscode';
import {
  LanguageClient,
  LanguageClientOptions,
  ServerOptions,
  TransportKind,
} from 'vscode-languageclient/node';

import type { Concept } from '@znasllc-io/memql-sdk-core/client';

import { defaultClustersPath, readClustersFile, setSelectedCluster, upsertCluster } from './clusters/file.js';
import type { ClusterConfig } from './clusters/model.js';
import { ConnectionManager } from './connection/manager.js';
import { ClustersTreeProvider, type ClusterNode } from './views/clustersTree.js';
import { ConceptsTreeProvider } from './views/conceptsTree.js';
import { ConceptPanel } from './webview/conceptPanel.js';

let client: LanguageClient | undefined;
let connections: ConnectionManager | undefined;

// getConnectionManager exposes the live manager to later-registered views.
export function getConnectionManager(): ConnectionManager {
  if (connections === undefined) {
    throw new Error('memQL: connection manager accessed before activation');
  }
  return connections;
}

export function activate(context: ExtensionContext): void {
  const serverPath = resolveServerPath(context);
  if (!serverPath) {
    window.showErrorMessage(
      'MemQL: memql-lsp binary not found. Set "memql.lsp.serverPath", bundle a platform binary, or install memql-lsp on your PATH.'
    );
    return;
  }

  // The server serves the workspace root offline; point it at the first folder.
  const args = ['--stdio'];
  const root = workspace.workspaceFolders?.[0]?.uri.fsPath;
  if (root) {
    args.push('--root', root);
  }

  const serverOptions: ServerOptions = {
    run: { command: serverPath, args, transport: TransportKind.stdio },
    debug: { command: serverPath, args, transport: TransportKind.stdio },
  };

  const clientOptions: LanguageClientOptions = {
    documentSelector: [{ language: 'memql' }],
    synchronize: {
      // The server rebuilds its registry on watched .memql changes so a concept
      // added in one file becomes visible to completion/hover in the others.
      fileEvents: workspace.createFileSystemWatcher('**/*.memql'),
    },
  };

  client = new LanguageClient('memql', 'MemQL Language Server', serverOptions, clientOptions);
  // start() is async; surface a start failure instead of leaving an unhandled
  // rejection (the LanguageClient shows its own UI too, but this makes the
  // failure explicit and actionable).
  void client.start().catch((err) => {
    window.showErrorMessage(
      `MemQL: language server failed to start: ${err instanceof Error ? err.message : String(err)}`
    );
  });

  // The runtime surface reads credentials from the home directory and opens a
  // network connection, so it is gated on workspace trust. Language features
  // above are not.
  //
  // Workspace trust can transition Restricted -> Trusted within a running
  // session (the user clicks "Trust This Workspace"); VS Code's own guidance
  // for a "limited" untrustedWorkspaces extension is to listen for that and
  // light up the gated functionality without requiring a window reload. An
  // untrusted activation therefore arms a one-shot listener instead of just
  // returning.
  if (workspace.isTrusted) {
    registerRuntimeSurface(context);
  } else {
    const trustGranted = workspace.onDidGrantWorkspaceTrust(() => {
      trustGranted.dispose();
      registerRuntimeSurface(context);
    });
    context.subscriptions.push(trustGranted);
  }
}

function registerRuntimeSurface(context: ExtensionContext): void {
  // Defensive: the trust-granted listener above disposes itself before
  // calling in, so this should only ever run once, but guard here too so a
  // second call (from any future caller) can never double-register the tree,
  // watcher, and commands.
  if (connections !== undefined) {
    return;
  }

  const clustersPath = defaultClustersPath();
  connections = new ConnectionManager();

  const clustersTree = new ClustersTreeProvider(clustersPath, connections);
  context.subscriptions.push(
    window.registerTreeDataProvider('memqlClusters', clustersTree)
  );

  // The cockpit writes this file too; watch it so the tree stays truthful.
  const watcher = workspace.createFileSystemWatcher(clustersPath);
  watcher.onDidChange(() => clustersTree.refresh());
  watcher.onDidCreate(() => clustersTree.refresh());
  watcher.onDidDelete(() => clustersTree.refresh());
  context.subscriptions.push(watcher);

  const conceptsTree = new ConceptsTreeProvider(connections);
  context.subscriptions.push(
    window.registerTreeDataProvider('memqlConcepts', conceptsTree),
    commands.registerCommand('memql.concepts.refresh', () => conceptsTree.refresh())
  );

  context.subscriptions.push(
    commands.registerCommand('memql.concepts.open', (concept: Concept) => {
      if (connections === undefined) {
        return;
      }
      ConceptPanel.open(context, connections, concept);
    })
  );

  context.subscriptions.push(
    commands.registerCommand('memql.clusters.refresh', () => clustersTree.refresh()),
    commands.registerCommand('memql.clusters.disconnect', async () => {
      await connections?.disconnect();
      clustersTree.refresh();
    }),
    commands.registerCommand('memql.clusters.select', async (node?: ClusterNode) => {
      const target = node ?? (await pickCluster(clustersPath));
      if (target === undefined) {
        return;
      }
      await setSelectedCluster(clustersPath, target.cluster.name);
      clustersTree.refresh();
      await connections?.connect(target.cluster);
      const state = connections?.state;
      if (state?.status === 'error') {
        window.showErrorMessage(`memQL: ${state.message}`);
      }
    }),
    commands.registerCommand('memql.clusters.add', async () => {
      const created = await promptForCluster();
      if (created === undefined) {
        return;
      }
      await upsertCluster(clustersPath, created);
      clustersTree.refresh();
    }),
    commands.registerCommand('memql.clusters.edit', async (node?: ClusterNode) => {
      const target = node ?? (await pickCluster(clustersPath));
      if (target === undefined) {
        return;
      }
      const edited = await promptForCluster(target.cluster);
      if (edited === undefined) {
        return;
      }
      await upsertCluster(clustersPath, edited);
      clustersTree.refresh();
    })
  );
}

async function pickCluster(clustersPath: string): Promise<ClusterNode | undefined> {
  const file = await readClustersFile(clustersPath);
  const picked = await window.showQuickPick(
    file.clusters.map((cluster) => ({
      label: cluster.name,
      description: cluster.endpoint,
      cluster,
    })),
    { placeHolder: 'Select a memQL cluster' }
  );
  if (picked === undefined) {
    return undefined;
  }
  return { cluster: picked.cluster, selected: picked.cluster.name === file.selectedCluster };
}

// promptForCluster collects a cluster with native inputs rather than a webview:
// it is four fields, and a QuickInput sequence is both less code and more
// idiomatic than a custom form.
async function promptForCluster(existing?: ClusterConfig): Promise<ClusterConfig | undefined> {
  const name = await window.showInputBox({
    prompt: 'Cluster name (the key used in clusters.yaml)',
    value: existing?.name ?? '',
    ignoreFocusOut: true,
    validateInput: (v) => (v.trim() === '' ? 'A name is required' : undefined),
  });
  if (name === undefined) {
    return undefined;
  }

  const domain = await window.showInputBox({
    prompt: 'Domain (e.g. local.znas.io). The endpoint is composed as cockpit.<domain>:443.',
    value: existing?.domain ?? '',
    ignoreFocusOut: true,
  });
  if (domain === undefined) {
    return undefined;
  }

  const endpoint = await window.showInputBox({
    prompt: 'gRPC endpoint (host:port)',
    value: existing?.endpoint ?? (domain.trim() === '' ? '' : `cockpit.${domain.trim()}:443`),
    ignoreFocusOut: true,
    validateInput: (v) => (v.trim() === '' ? 'An endpoint is required' : undefined),
  });
  if (endpoint === undefined) {
    return undefined;
  }

  const pat = await window.showInputBox({
    prompt: 'Personal Access Token (mql_pat_...). Leave empty to authenticate in the memQL Cockpit instead.',
    value: existing?.pat ?? '',
    ignoreFocusOut: true,
    password: true,
  });
  if (pat === undefined) {
    return undefined;
  }

  const out: ClusterConfig = { name: name.trim(), endpoint: endpoint.trim() };
  if (domain.trim() !== '') {
    out.domain = domain.trim();
  }
  if (pat.trim() !== '') {
    out.pat = pat.trim();
  }
  return out;
}

export function deactivate(): Thenable<void> | undefined {
  // On a host deactivate/reload without a full process exit, an open
  // ConnectionManager WebSocket would otherwise outlive the extension.
  const stopClient = client?.stop();
  const disconnectConnections = connections?.disconnect();
  if (stopClient === undefined && disconnectConnections === undefined) {
    return undefined;
  }
  return Promise.all([stopClient, disconnectConnections]).then(() => undefined);
}

// resolveServerPath picks the memql-lsp binary in priority order:
//   1. the memql.lsp.serverPath USER setting (if non-empty),
//   2. a bundled platform binary at bin/<platform>-<arch>/memql-lsp,
//   3. 'memql-lsp' resolved from PATH.
//
// SECURITY: serverPath is read only from the USER (global) settings, never from
// workspace-scoped settings. A workspace-scoped value comes from a
// `.vscode/settings.json` inside the opened folder, which an attacker controls;
// honoring it would let a malicious repo point the extension at an arbitrary
// executable and run it (arbitrary code execution). The bundled binary and the
// PATH fallback are not workspace-controlled.
function resolveServerPath(context: ExtensionContext): string | undefined {
  const configured = workspace.getConfiguration('memql.lsp').inspect<string>('serverPath')?.globalValue;
  if (typeof configured === 'string' && configured.trim() !== '') {
    return configured;
  }

  const bundled = context.asAbsolutePath(
    path.join('bin', `${process.platform}-${process.arch}`, binaryName())
  );
  if (fs.existsSync(bundled)) {
    return bundled;
  }

  // Fall back to a binary on PATH. Resolve it here (rather than returning the
  // bare name for the OS to resolve at spawn) so an unresolvable binary yields
  // undefined -- that lets the caller's friendly "not found" message fire
  // instead of surfacing a raw ENOENT from the spawned process.
  return resolveOnPath(binaryName());
}

// resolveOnPath returns the absolute path to an executable `name` found on the
// PATH, or undefined if it is not on any PATH entry.
function resolveOnPath(name: string): string | undefined {
  const dirs = (process.env.PATH ?? '').split(path.delimiter);
  for (const dir of dirs) {
    if (dir === '') {
      continue;
    }
    const candidate = path.join(dir, name);
    try {
      fs.accessSync(candidate, fs.constants.X_OK); // X_OK degrades to existence on Windows
      return candidate;
    } catch {
      // Not on this PATH entry; keep looking.
    }
  }
  return undefined;
}

function binaryName(): string {
  return process.platform === 'win32' ? 'memql-lsp.exe' : 'memql-lsp';
}
