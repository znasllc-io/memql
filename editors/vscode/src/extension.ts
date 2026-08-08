import * as fs from 'fs';
import * as path from 'path';
import { commands, ExtensionContext, RelativePattern, Uri, window, workspace } from 'vscode';
import {
  LanguageClient,
  LanguageClientOptions,
  ServerOptions,
  TransportKind,
} from 'vscode-languageclient/node';

import type { Concept } from '@znasllc-io/memql-sdk-core/client';

import { defaultClustersPath, readClustersFileSafe, setSelectedCluster, upsertCluster } from './clusters/file.js';
import type { ClusterConfig } from './clusters/model.js';
import { ConnectionManager } from './connection/manager.js';
import { ClustersTreeProvider, type ClusterNode } from './views/clustersTree.js';
import { ConceptsTreeProvider } from './views/conceptsTree.js';
import { ConceptPanel } from './webview/conceptPanel.js';

let client: LanguageClient | undefined;
let connections: ConnectionManager | undefined;

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
  //
  // MUST be a RelativePattern with a base Uri, never the bare path string.
  // Given a plain `string` glob, createFileSystemWatcher only watches paths
  // INSIDE the workspace folders -- and ~/.memql/clusters.yaml is outside any
  // workspace. The string form therefore never fired at all, making all three
  // handlers below dead code: a cluster added in the cockpit stayed invisible
  // until someone hit Refresh by hand, contradicting the documented "the file
  // is watched: an external edit refreshes the view".
  const watcher = workspace.createFileSystemWatcher(
    new RelativePattern(Uri.file(path.dirname(clustersPath)), path.basename(clustersPath))
  );
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
    // Not palette-invokable (contributes.menus.commandPalette hides it with
    // "when": "false") since it needs a Concept argument the palette can't
    // supply -- the Concepts tree item's inline command is the only wiring.
    // Guard on `concept` anyway: belt and braces against any future caller
    // (or a manifest edit that forgets the palette exclusion) invoking this
    // with no argument, which would otherwise throw inside ConceptPanel.open
    // on `concept.id`.
    commands.registerCommand('memql.concepts.open', (concept?: Concept) => {
      if (connections === undefined || concept === undefined) {
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
      await writeCluster(clustersPath, created, undefined, clustersTree);
    }),
    commands.registerCommand('memql.clusters.edit', async (node?: ClusterNode) => {
      const target = node ?? (await pickCluster(clustersPath));
      if (target === undefined) {
        return;
      }
      // The name field is editable, so the edited config may not identify the
      // node it came from. Pass the ORIGINAL name so upsertCluster renames that
      // node instead of appending a second one under the new name.
      const originalName = target.cluster.name;
      const edited = await promptForCluster(target.cluster);
      if (edited === undefined) {
        return;
      }
      await writeCluster(clustersPath, edited, originalName, clustersTree);
    })
  );
}

// writeCluster persists a cluster and refreshes the tree, surfacing a write
// failure (e.g. a rename onto a name already in use) as a message rather than
// letting the rejection escape as a raw command-error toast.
async function writeCluster(
  clustersPath: string,
  cluster: ClusterConfig,
  originalName: string | undefined,
  clustersTree: ClustersTreeProvider
): Promise<void> {
  try {
    await upsertCluster(clustersPath, cluster, originalName);
  } catch (err) {
    window.showErrorMessage(`memQL: ${err instanceof Error ? err.message : String(err)}`);
    return;
  }
  clustersTree.refresh();
}

async function pickCluster(clustersPath: string): Promise<ClusterNode | undefined> {
  // readClustersFileSafe, not readClustersFile: the Clusters TREE already
  // renders a malformed file as a readable row, and this path must agree with
  // it. The throwing variant turned "Select Cluster" from the palette into a
  // raw command-error toast for a file the tree was calmly explaining.
  const result = await readClustersFileSafe(clustersPath);
  if (!result.ok) {
    window.showErrorMessage(`memQL: ${result.error}`);
    return undefined;
  }
  const file = result.file;
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

  // Every collected field is returned, INCLUDING the empty ones. An empty
  // string means "the user cleared this input", which upsertCluster turns into
  // a key delete. Omitting the key instead would mean "leave whatever is on
  // disk alone" -- so clearing the PAT field to revoke a token left the old
  // token sitting in clusters.yaml while the UI showed it gone.
  return {
    name: name.trim(),
    endpoint: endpoint.trim(),
    domain: domain.trim(),
    pat: pat.trim(),
  };
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
