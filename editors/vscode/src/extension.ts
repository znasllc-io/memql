import * as fs from 'fs';
import * as path from 'path';
import {
  commands,
  Diagnostic,
  DiagnosticSeverity,
  ExtensionContext,
  languages,
  Position,
  Range,
  RelativePattern,
  Uri,
  window,
  workspace,
} from 'vscode';
import {
  LanguageClient,
  LanguageClientOptions,
  ServerOptions,
  TransportKind,
} from 'vscode-languageclient/node';

import { callTool } from '@znasllc-io/memql-sdk-core/tools';
import { AutomationClient } from '@znasllc-io/memql-sdk-core/automation';
import { browseConceptPage } from '@znasllc-io/memql-sdk-core/client';
import type { Concept } from '@znasllc-io/memql-sdk-core/client';
import type { ConceptLike } from '@znasllc-io/memql-view-kit';

import { addCluster, defaultClustersPath, readClustersFileSafe, setSelectedCluster, upsertCluster } from './clusters/file.js';
import type { ClusterConfig } from './clusters/model.js';
import { ConnectionManager } from './connection/manager.js';
import {
  COMMAND_RUN,
  COMMAND_RUN_AUTOMATION,
  COMMAND_RUN_WITH,
  RUNNABLE_CONSTRUCTS_METHOD,
  parseRunnableConstructs,
  type AutomationTarget,
  type RunTarget,
} from './constructs/runnable.js';
import { RunnableCodeLensProvider } from './constructs/lensProvider.js';
import {
  AutomationRunner,
  type AutomationRunEngine,
  type AutomationRunRequest,
} from './run/automationRun.js';
import { assembleBundle, type BundleSource, type WorkspaceSources } from './run/bundle.js';
import {
  IMPORTS_CAPABILITY,
  IMPORTS_METHOD,
  importPaths,
  parseImports,
} from './constructs/imports.js';
import type { MappedDiagnostic } from './run/diagnostics.js';
import { groupByFile } from './run/diagnostics.js';
import { RunOrchestrator, type RunCluster, type RunEngine } from './run/orchestrator.js';
import {
  readRunConfigs,
  runConfigPath,
  upsertRunConfig,
  removeRunConfig,
  writeRunConfigs,
  RUN_CONFIG_RELATIVE_PATH,
  type AutomationRunConfig,
  type RunConfig,
} from './run/runConfig.js';
import { ClustersTreeProvider, type ClusterNode } from './views/clustersTree.js';
import { ConceptsTreeProvider } from './views/conceptsTree.js';
import { RunsTreeProvider, type RunsTreeNode } from './views/runsTree.js';
import { AutomationRunPanel, type AutomationPanelHost } from './webview/automationPanel.js';
import { ClusterPanel } from './webview/clusterPanel.js';
import { ConceptPanel } from './webview/conceptPanel.js';
import { ResultPanel, RunPanel, conceptMap, type RunPanelHost } from './webview/runPanel.js';

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
  //
  // AND THE BASE DIRECTORY MUST EXIST FIRST. A watcher outside the workspace is
  // non-recursive, and a non-recursive watch of a path that does not exist yet
  // cannot be established. On the declared engines floor (VS Code 1.91) that
  // watcher then stays dead FOR THE WHOLE SESSION even after the directory
  // appears -- so a user who had never run the Cockpit (no ~/.memql at all,
  // i.e. every new user) got exactly the same silent no-refresh the string glob
  // caused. Current stable retries and recovers after a few seconds; the floor
  // does not. Found by the host smoke lane (memql#3302), which is the only
  // place it is observable: nothing throws, and every unit test still passes.
  //
  // mkdir is not a side effect this extension is shy about -- ~/.memql is its
  // own directory, shared with the Cockpit, and Add Cluster creates it anyway.
  const clustersDir = path.dirname(clustersPath);
  try {
    fs.mkdirSync(clustersDir, { recursive: true });
  } catch {
    // A home directory we cannot write is not worth failing activation over;
    // the tree still reads (and reports) the file, only live refresh is lost.
  }
  const watcher = workspace.createFileSystemWatcher(
    new RelativePattern(Uri.file(clustersDir), path.basename(clustersPath))
  );
  watcher.onDidChange(() => clustersRegistryChanged(clustersTree));
  watcher.onDidCreate(() => clustersRegistryChanged(clustersTree));
  watcher.onDidDelete(() => clustersRegistryChanged(clustersTree));
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
      // addCluster, not upsertCluster: an add whose name collides with an
      // existing cluster must be refused, not silently turned into an edit
      // that deletes every field this form left blank. See addCluster.
      await writeCluster(clustersTree, () => addCluster(clustersPath, created));
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
      await writeCluster(clustersTree, () => upsertCluster(clustersPath, edited, originalName));
    }),
    // The Cluster tab (memql#3312): topology, deployment history, and the
    // deploy actions. Reached from the Clusters tree's inline action, which
    // supplies the node -- and from the palette, which cannot, so it falls
    // back to the selected cluster rather than doing nothing. That fallback
    // is why this command carries the trust clause instead of
    // "when": "false" like the other argument-taking commands.
    commands.registerCommand('memql.cluster.open', async (node?: ClusterNode) => {
      if (connections === undefined) {
        return;
      }
      const target = node ?? (await pickCluster(clustersPath));
      if (target === undefined || target.cluster.name === '') {
        return;
      }
      ClusterPanel.open(context, connections, target.cluster.name);
    })
  );

  registerRunSurface(context, clustersPath, connections);
}

// registerRunSurface wires memql#3309: CodeLens run affordances, the run
// orchestrator, the arg form / result tabs, and the Runs tree.
//
// Kept in its own function rather than folded into registerRuntimeSurface so
// the trust gate above stays one readable decision, and so the run surface's
// dependencies are visible as parameters instead of as reads off module state.
function registerRunSurface(
  context: ExtensionContext,
  clustersPath: string,
  conns: ConnectionManager
): void {
  const workspaceRoot = workspace.workspaceFolders?.[0]?.uri.fsPath;

  // A collection of our OWN, separate from the language server's. The server
  // publishes parse/semantic diagnostics for the buffer; these are the
  // ENGINE's answer about the assembled bundle, and folding them together
  // would make a run's failures vanish on the next keystroke when the server
  // republishes.
  const runDiagnostics = languages.createDiagnosticCollection('memql-run');
  context.subscriptions.push(runDiagnostics);

  // The concept descriptors the result view renders rows against. Refreshed on
  // connect rather than fetched per result: it is per-cluster data that
  // changes only when the cluster does, and a result should not wait on a
  // second round-trip to draw.
  let concepts: ReadonlyMap<string, ConceptLike> = new Map();

  const orchestrator = new RunOrchestrator({
    cluster: () => currentRunCluster(clustersPath, conns),
    engine: () => buildRunEngine(conns),
    assemble: (target) => assembleForTarget(target, workspaceRoot),
    confirmWrite: async (message) => {
      // A MODAL. A dismissable toast would let the write proceed while the
      // developer's attention is elsewhere, which defeats the whole point of
      // asking.
      const answer = await window.showWarningMessage(message, { modal: true }, 'Run');
      return answer === 'Run';
    },
    publishDiagnostics: (mapped) => publishRunDiagnostics(runDiagnostics, mapped),
  });
  runOrchestrator = orchestrator;
  // Warm the name -> config cache the synchronous cluster() read depends on.
  void refreshClusterCache(clustersPath);

  // Every connection-state change ends the stream that held any session-define.
  // Bumping the epoch here is what makes the next run re-inject before
  // honouring itself -- without it a re-run after a reconnect silently
  // executes the DEPLOYED construct and returns a perfectly good wrong answer.
  context.subscriptions.push({
    dispose: conns.onDidChangeState((state) => {
      orchestrator.noteStreamReset();
      if (state.status === 'connected') {
        void conns.query?.listConcepts().then(
          (list) => {
            concepts = conceptMap(list);
          },
          () => {
            // A failed concept list only costs display cards on the result
            // view, which degrades to the row id. Not worth a popup.
          }
        );
      } else {
        concepts = new Map();
      }
    }),
  });

  const host: RunPanelHost = {
    run: (target, values) => orchestrator.run(target, values),
    saveConfig: async (target, name, values) => {
      if (workspaceRoot === undefined) {
        throw new Error(
          'A run configuration is saved in the workspace, so this needs an open folder.'
        );
      }
      const config: RunConfig = {
        name,
        kind: target.kind,
        construct: target.name,
        args: values,
      };
      const relative = workspaceRelative(workspaceRoot, target.uri);
      if (relative !== undefined) config.file = relative;
      await writeRunConfigs(runConfigPath(workspaceRoot), (current) =>
        upsertRunConfig(current, config)
      );
      runsTree.refresh();
    },
    concepts: () => concepts,
    openRow: (conceptId, rowId) => {
      void openRowInConcepts(context, conns, conceptId, rowId);
    },
  };

  // memql#3310's automation half. It shares the orchestrator's write gate --
  // one acknowledgement policy and one reset across both run surfaces -- and
  // otherwise needs none of B2's machinery, because an automation cannot be
  // session-defined and so has nothing to bundle, validate or inject.
  const automationRunner = new AutomationRunner({
    cluster: () => currentRunCluster(clustersPath, conns),
    engine: () => buildAutomationEngine(conns),
    confirmWrite: async (message) => {
      // A MODAL, exactly as the mutation confirmation is: an automation run
      // has the larger blast radius of the two, so it certainly does not get
      // the dismissable variant.
      const answer = await window.showWarningMessage(message, { modal: true }, 'Run');
      return answer === 'Run';
    },
    writeGate: orchestrator.writeGate,
  });

  const automationHost: AutomationPanelHost = {
    run: (target, request, trace, onProgress) =>
      automationRunner.run(target, request, trace, onProgress),
    saveConfig: async (target, name, request) => {
      if (workspaceRoot === undefined) {
        throw new Error(
          'A run configuration is saved in the workspace, so this needs an open folder.'
        );
      }
      const config: RunConfig = {
        name,
        kind: 'automation',
        construct: target.name,
        // Always empty: an automation declares no args. The trigger event
        // lives in the `automation` block instead -- see run/runConfig.ts.
        args: {},
        automation: automationConfigBlock(request),
      };
      const relative = workspaceRelative(workspaceRoot, target.uri);
      if (relative !== undefined) config.file = relative;
      await writeRunConfigs(runConfigPath(workspaceRoot), (current) =>
        upsertRunConfig(current, config)
      );
      runsTree.refresh();
    },
    browseRows: async (conceptId, cursor) => {
      const query = conns.query;
      if (query === undefined) {
        throw new Error('Not connected. Select a cluster in the Clusters view.');
      }
      return browseConceptPage(query, conceptId, {
        pageSize: 100,
        ...(cursor === '' ? {} : { cursor }),
      });
    },
    concept: (conceptId) => concepts.get(conceptId),
  };

  const runsTree = new RunsTreeProvider(workspaceRoot);
  context.subscriptions.push(window.registerTreeDataProvider('memqlRuns', runsTree));

  // The run-config file is plain text a developer (or an agent) edits
  // directly, so the tree has to follow the file rather than only its own
  // writes.
  if (workspaceRoot !== undefined) {
    const runsWatcher = workspace.createFileSystemWatcher(
      new RelativePattern(Uri.file(workspaceRoot), RUN_CONFIG_RELATIVE_PATH.split(path.sep).join('/'))
    );
    runsWatcher.onDidChange(() => runsTree.refresh());
    runsWatcher.onDidCreate(() => runsTree.refresh());
    runsWatcher.onDidDelete(() => runsTree.refresh());
    context.subscriptions.push(runsWatcher);
  }

  // The CodeLens provider. Registered only once the workspace is trusted, in
  // line with everything else in the runtime surface -- an untrusted window
  // renders no Run affordance at all, so there is nothing to click and no
  // implication that there could be.
  if (client !== undefined) {
    const lensProvider = new RunnableCodeLensProvider({
      sendRequest: (method, params, token) =>
        token === undefined
          ? (client as LanguageClient).sendRequest(method, params)
          : (client as LanguageClient).sendRequest(method, params, token),
      experimentalCapabilities: () =>
        (client as LanguageClient).initializeResult?.capabilities.experimental as
          | Record<string, unknown>
          | undefined,
    });
    context.subscriptions.push(
      languages.registerCodeLensProvider({ language: 'memql' }, lensProvider)
    );
  }

  context.subscriptions.push(
    // Palette-hidden ("when": false): it takes a RunTarget the palette cannot
    // supply. The CodeLens is the only caller.
    commands.registerCommand(COMMAND_RUN, async (target?: RunTarget) => {
      if (target === undefined) return;
      // Run goes straight through when every required argument can be
      // satisfied without asking; otherwise it opens the form pre-filled.
      // Anything else would either refuse a one-click run on a no-argument
      // query or silently send an empty required argument.
      if (target.args.some((a) => a.required)) {
        RunPanel.open(context, host, target);
        return;
      }
      ResultPanel.show(context, host, await orchestrator.run(target, {}));
    }),
    commands.registerCommand(COMMAND_RUN_WITH, (target?: RunTarget) => {
      if (target === undefined) return;
      RunPanel.open(context, host, target);
    }),
    // Palette-hidden for the same reason as the other run commands: it takes
    // an AutomationTarget the palette cannot supply. The CodeLens and the Runs
    // tree are the only callers.
    //
    // It opens the FORM rather than running anything, even for a scheduled
    // automation that genuinely fires with an empty event -- the form is where
    // the developer is told the deployed definition is what will run, and a
    // one-click path past that would defeat the whole banner.
    commands.registerCommand(COMMAND_RUN_AUTOMATION, (target?: AutomationTarget) => {
      if (target === undefined) return;
      AutomationRunPanel.open(context, automationHost, target);
    }),
    commands.registerCommand('memql.runs.refresh', () => runsTree.refresh()),
    commands.registerCommand('memql.runs.open', async () => {
      if (workspaceRoot === undefined) {
        window.showErrorMessage('memQL: run configurations live in the workspace; open a folder first.');
        return;
      }
      // Opens the actual file. The point of the format is that it IS plain
      // editable text, and the fastest way to make that true rather than
      // merely claimed is to hand the developer the file.
      const uri = Uri.file(runConfigPath(workspaceRoot));
      try {
        await window.showTextDocument(await workspace.openTextDocument(uri));
      } catch {
        // Not created until the first save. Open an untitled buffer at the
        // right path instead of erroring at someone who just wants to see the
        // format.
        await window.showTextDocument(
          await workspace.openTextDocument({ language: 'json', content: '{\n  "version": 1,\n  "runs": []\n}\n' })
        );
      }
    }),
    commands.registerCommand('memql.runs.execute', async (node?: RunsTreeNode) => {
      if (node === undefined || node.kind !== 'run') return;
      // An automation configuration OPENS THE FORM pre-filled rather than
      // running straight away. Every other kind's saved configuration is a
      // complete, replayable call; an automation's is a saved trigger event
      // whose blast radius is its whole action chain, and the form is where
      // the deployed-definition banner and the payload are both visible before
      // the click. Nothing in this extension auto-runs, and a saved automation
      // is the entry a repository is most likely to ship.
      if (node.config.kind === 'automation') {
        const automationTarget = await automationTargetForConfig(node.config, workspaceRoot);
        if (automationTarget === undefined) return;
        AutomationRunPanel.open(
          context,
          automationHost,
          automationTarget,
          requestFromConfigBlock(node.config.automation)
        );
        return;
      }
      const target = await targetForConfig(node.config, workspaceRoot);
      if (target === undefined) return;
      ResultPanel.show(context, host, await orchestrator.run(target, node.config.args));
    }),
    commands.registerCommand('memql.runs.delete', async (node?: RunsTreeNode) => {
      if (node === undefined || node.kind !== 'run' || workspaceRoot === undefined) return;
      const confirmed = await window.showWarningMessage(
        `Delete the run configuration "${node.config.name}"?`,
        { modal: true },
        'Delete'
      );
      if (confirmed !== 'Delete') return;
      try {
        await writeRunConfigs(runConfigPath(workspaceRoot), (current) =>
          removeRunConfig(current, node.config.name)
        );
      } catch (err) {
        window.showErrorMessage(`memQL: ${err instanceof Error ? err.message : String(err)}`);
        return;
      }
      runsTree.refresh();
    })
  );
}

// clustersRegistryChanged refreshes the tree AND drops the write-confirmation
// acknowledgements. An operator who re-points a cluster NAME at a different
// endpoint would otherwise carry an acknowledgement over to a cluster they
// confirmed nothing about.
function clustersRegistryChanged(clustersTree: ClustersTreeProvider): void {
  clustersTree.refresh();
  runOrchestrator?.noteClusterRegistryChanged();
  void refreshClusterCache(defaultClustersPath());
}

// The orchestrator, held at module scope only so clustersRegistryChanged (a
// file-watcher callback registered before the run surface exists) can reach it.
let runOrchestrator: RunOrchestrator | undefined;

// currentRunCluster resolves the selected cluster down to the three facts a run
// needs. Deliberately NOT the whole ClusterConfig: the PAT must never travel
// into the orchestrator, and from there into a webview or a log.
function currentRunCluster(
  clustersPath: string,
  conns: ConnectionManager
): RunCluster | undefined {
  const state = conns.state;
  if (state.status !== 'connected' && state.status !== 'connecting') return undefined;
  const cluster = clusterCache.get(state.clusterName);
  if (cluster === undefined) {
    // The cache is warmed by the select command; a miss means the file changed
    // underneath us. Reporting the connected name with local=false is the safe
    // degradation -- it prompts on a mutation rather than skipping the prompt.
    void refreshClusterCache(clustersPath);
    return { name: state.clusterName, label: state.clusterName, local: false };
  }
  return {
    name: cluster.name,
    label: cluster.displayName !== undefined && cluster.displayName !== '' ? cluster.displayName : cluster.name,
    local: cluster.local === true,
  };
}

// A tiny name -> config cache so the synchronous RunDeps.cluster() does not
// have to do file IO. Refreshed whenever clusters.yaml changes.
const clusterCache = new Map<string, ClusterConfig>();

async function refreshClusterCache(clustersPath: string): Promise<void> {
  const result = await readClustersFileSafe(clustersPath);
  if (!result.ok) return;
  clusterCache.clear();
  for (const c of result.file.clusters) clusterCache.set(c.name, c);
}

// buildRunEngine adapts the live connection to the four calls a run makes.
// Undefined whenever anything is missing, which the orchestrator reports as
// "not connected" rather than failing mid-run.
function buildRunEngine(conns: ConnectionManager): RunEngine | undefined {
  const authoring = conns.authoring;
  const query = conns.query;
  const dispatcher = conns.dispatcher;
  if (authoring === undefined || query === undefined || dispatcher === undefined) {
    return undefined;
  }
  return {
    validateBundle: (sources) => authoring.validateBundle(sources),
    sessionDefineBundle: (sources) => authoring.sessionDefineBundle(sources),
    executeNamed: async (name, call) => {
      const result = await query.executeNamed(name, call);
      // rawNodes() over rows(): the result view renders through view-kit and
      // the concept browser's projection, both of which want the row
      // intrinsics (id, concept, createdAt) preserved rather than flattened
      // away. rows() falls back to the flattened bundle form when the payload
      // is a plain data array, which is exactly the shape a logic construct
      // returns, so both are consulted.
      const nodes = result.rawNodes();
      return { rows: nodes.length > 0 ? nodes : result.rows(), raw: result.raw() };
    },
    callTool: (name, args) => callTool(dispatcher, { name, arguments: args }),
  };
}

// assembleForTarget builds the bundle from the EDITOR's view of the workspace:
// an open document's live text (dirty or not), falling back to the file on
// disk for anything not open.
//
// The ACTIVE TEXT is captured SYNCHRONOUSLY, before anything is awaited. That
// is what the walk's old synchrony was really protecting -- the buffer moving
// between the decision to run and the bytes that get sent -- and it survives
// the import lookup becoming a request (memql#3335). Dependency text is read
// as the walk reaches it, which is the same guarantee it had before: whatever
// the editor holds at that moment.
async function assembleForTarget(target: RunTarget, workspaceRoot: string | undefined) {
  const uri = Uri.parse(target.uri);
  const activePath = uri.fsPath;
  const open = workspace.textDocuments.find((d) => d.uri.toString() === target.uri);
  const activeText = open !== undefined ? open.getText() : readFileOrThrow(activePath);

  const sources: WorkspaceSources = {
    resolveImport: (dotted) => resolveImportPath(dotted, workspaceRoot, activePath),
    read: (p) => readSource(p),
    imports: (p, text) => requestImports(p, text),
  };
  return assembleBundle(activePath, activeText, sources);
}

// requestImports asks the language server which modules `text` imports.
//
// FEATURE-DETECTED, like the CodeLens path: an older memql-lsp on the PATH is
// an ordinary situation (serverPath is a user setting and the PATH fallback is
// whatever is installed), and a MethodNotFound surfacing as a toast in the
// middle of a run would be a worse answer than the degradation.
//
// Every failure degrades to "no imports", which bundles the active file alone.
// That means a dirty dependency runs as its deployed version -- the same
// bounded, visible cost the walk always had for an import it could not
// resolve. What it deliberately does NOT do is fall back to scanning the text
// here: a second parser is the thing this request exists to delete, and a
// fallback one would be a second parser that only runs when nobody is looking.
//
// The TEXT is sent rather than only the URI because the walk reaches files the
// editor has never opened -- a CLEAN file is a legitimate route to a DIRTY one
// -- so the server holds no copy of them. The bytes analysed are then exactly
// the bytes the bundle is assembled from.
async function requestImports(filePath: string, text: string): Promise<string[]> {
  if (client === undefined) return [];
  const experimental = client.initializeResult?.capabilities.experimental as
    | Record<string, unknown>
    | undefined;
  if (experimental === undefined || experimental[IMPORTS_CAPABILITY] !== true) return [];
  try {
    const raw = await client.sendRequest(IMPORTS_METHOD, {
      textDocument: { uri: Uri.file(filePath).toString() },
      text,
    });
    return importPaths(parseImports(raw));
  } catch {
    return [];
  }
}

function readFileOrThrow(p: string): string {
  try {
    return fs.readFileSync(p, 'utf8');
  } catch (err) {
    throw new Error(
      `cannot read ${p}: ${err instanceof Error ? err.message : String(err)} -- open the file and try again`
    );
  }
}

// readSource prefers the EDITOR's copy. An open document is the only place an
// unsaved edit exists, and unsaved edits are the entire reason the bundle
// carries dependencies at all.
function readSource(p: string): BundleSource | undefined {
  const open = workspace.textDocuments.find((d) => d.uri.fsPath === p);
  if (open !== undefined) return { text: open.getText(), dirty: open.isDirty };
  try {
    return { text: fs.readFileSync(p, 'utf8'), dirty: false };
  } catch {
    return undefined;
  }
}

// resolveImportPath maps a dotted import (`cognition.shapes`) to a file.
//
// The candidates mirror how the tree is actually laid out -- `dsl/<ns>/<file>.memql`
// under a workspace folder -- plus a sibling-relative form, which is what a
// product DSL bundle looks like when the workspace root IS the bundle. The
// first candidate that EXISTS wins; an import that resolves nowhere is skipped
// by the walk, which simply means that dependency resolves against the live
// registry.
function resolveImportPath(
  dotted: string,
  workspaceRoot: string | undefined,
  activePath: string
): string | undefined {
  const relative = `${dotted.split('.').join(path.sep)}.memql`;
  const roots: string[] = [];
  if (workspaceRoot !== undefined) {
    roots.push(path.join(workspaceRoot, 'dsl'), workspaceRoot);
  }
  // The active file's own namespace root: `<...>/dsl/<ns>/queries.memql` means
  // `<...>/dsl` is where a sibling namespace lives.
  roots.push(path.dirname(path.dirname(activePath)));
  for (const root of roots) {
    const candidate = path.join(root, relative);
    if (fs.existsSync(candidate)) return candidate;
  }
  return undefined;
}

// publishRunDiagnostics writes the mapped failures into the Problems panel.
//
// The collection is CLEARED first, on every publish including the empty one.
// Without that a failure fixed on the next run would stay on screen until some
// later run happened to fail in the same file.
function publishRunDiagnostics(
  collection: ReturnType<typeof languages.createDiagnosticCollection>,
  mapped: MappedDiagnostic[]
): void {
  collection.clear();
  for (const [file, diagnostics] of groupByFile(mapped)) {
    if (file === '') continue;
    collection.set(
      Uri.file(file),
      diagnostics.map((d) => {
        const diagnostic = new Diagnostic(
          new Range(
            new Position(d.start.line, d.start.character),
            new Position(d.end.line, d.end.character)
          ),
          d.message,
          DiagnosticSeverity.Error
        );
        diagnostic.source = 'memql (run)';
        return diagnostic;
      })
    );
  }
}

function workspaceRelative(workspaceRoot: string, uri: string): string | undefined {
  const fsPath = Uri.parse(uri).fsPath;
  const relative = path.relative(workspaceRoot, fsPath);
  if (relative === '' || relative.startsWith('..') || path.isAbsolute(relative)) return undefined;
  return relative.split(path.sep).join('/');
}

// targetForConfig rebuilds a full RunTarget from a saved configuration by
// asking the LANGUAGE SERVER about the file the configuration names.
//
// It deliberately does not reconstruct the arg list from the saved values. The
// declared order is what buildNamedCall renders with, and the construct's args
// may have changed since the configuration was written -- so the authority has
// to be the current buffer, read by the one parser, not the stored snapshot.
async function targetForConfig(
  config: RunConfig,
  workspaceRoot: string | undefined
): Promise<RunTarget | undefined> {
  const found = await constructForConfig(config, workspaceRoot);
  if (found === undefined) return undefined;
  return {
    uri: found.uri,
    kind: found.construct.kind,
    name: found.construct.name,
    args: found.construct.args,
  };
}

// automationTargetForConfig is targetForConfig's automation counterpart. It
// carries the TRIGGER rather than the args, for the same reason the lens does:
// the trigger is what decides the form, and the args are always empty.
//
// It re-resolves through the language server rather than trusting the saved
// entry, exactly as targetForConfig does -- a trigger can have been edited
// since the configuration was written, and the form built from a stale one
// would offer a row picker over the wrong concept.
async function automationTargetForConfig(
  config: RunConfig,
  workspaceRoot: string | undefined
): Promise<AutomationTarget | undefined> {
  const found = await constructForConfig(config, workspaceRoot);
  if (found === undefined) return undefined;
  const target: AutomationTarget = { uri: found.uri, name: found.construct.name };
  if (found.construct.trigger !== undefined) target.trigger = found.construct.trigger;
  return target;
}

// constructForConfig is the shared lookup: open the file the configuration
// names and ask the LANGUAGE SERVER to describe it.
//
// The authority is the current buffer read by the one parser, never the stored
// snapshot -- the construct may have been renamed, re-triggered or had its
// args changed since the configuration was written.
async function constructForConfig(
  config: RunConfig,
  workspaceRoot: string | undefined
): Promise<{ uri: string; construct: ReturnType<typeof parseRunnableConstructs>[number] } | undefined> {
  if (config.file === undefined || workspaceRoot === undefined) {
    window.showErrorMessage(
      `memQL: the run configuration "${config.name}" names no file, so there is no buffer to run. Add a "file" pointing at the .memql file that declares ${config.construct}.`
    );
    return undefined;
  }
  const uri = Uri.file(path.join(workspaceRoot, config.file.split('/').join(path.sep)));
  let document;
  try {
    document = await workspace.openTextDocument(uri);
  } catch (err) {
    window.showErrorMessage(
      `memQL: cannot open ${config.file}: ${err instanceof Error ? err.message : String(err)}`
    );
    return undefined;
  }
  if (client === undefined) return undefined;
  // The one call in this extension that asks the server for constructs OUTSIDE
  // provideCodeLenses, so it needs its own failure story: a server that
  // predates memql/runnableConstructs rejects with MethodNotFound, and an
  // unhandled rejection here would surface as a raw command-error toast on a
  // perfectly ordinary "your language server is older than your extension".
  let raw: unknown;
  try {
    raw = await client.sendRequest(RUNNABLE_CONSTRUCTS_METHOD, {
      textDocument: { uri: document.uri.toString() },
    });
  } catch (err) {
    window.showErrorMessage(
      `memQL: the language server could not describe ${config.file}: ${err instanceof Error ? err.message : String(err)}`
    );
    return undefined;
  }
  const found = parseRunnableConstructs(raw).find(
    (c) => c.name === config.construct && c.kind === config.kind
  );
  if (found === undefined) {
    window.showErrorMessage(
      `memQL: ${config.file} declares no ${config.kind} named ${config.construct}. The construct was renamed, or the file does not currently parse.`
    );
    return undefined;
  }
  return { uri: document.uri.toString(), construct: found };
}

// automationConfigBlock renders a run request into the persisted block, and
// requestFromConfigBlock reads it back. Kept as a symmetric pair so the two
// directions cannot drift: a field added to one without the other silently
// stops round-tripping through the file.
function automationConfigBlock(request: AutomationRunRequest): AutomationRunConfig {
  const block: AutomationRunConfig = {};
  if (request.payload !== undefined) block.payload = request.payload;
  if (request.concept !== undefined && request.concept !== '') block.concept = request.concept;
  if (request.targetNodeType !== undefined && request.targetNodeType !== '') {
    block.targetNodeType = request.targetNodeType;
  }
  if (request.includeStepOutput === true) block.includeStepOutput = true;
  return block;
}

function requestFromConfigBlock(block: AutomationRunConfig | undefined): AutomationRunRequest {
  const request: AutomationRunRequest = {};
  if (block === undefined) return request;
  if (block.payload !== undefined) request.payload = block.payload;
  if (block.concept !== undefined && block.concept !== '') request.concept = block.concept;
  if (block.targetNodeType !== undefined && block.targetNodeType !== '') {
    request.targetNodeType = block.targetNodeType;
  }
  if (block.includeStepOutput === true) request.includeStepOutput = true;
  return request;
}

// buildAutomationEngine adapts the live connection to the one call an
// automation run makes. Undefined whenever anything is missing, which the
// runner reports as "not connected" rather than failing mid-run.
//
// A NEW AutomationClient per call, deliberately: the client is a thin wrapper
// over whatever Dispatcher is live right now, and ConnectionManager rebuilds
// its dispatcher on every reconnect. A cached client would hold the dead
// stream's dispatcher and park forever on a run nothing will ever answer.
function buildAutomationEngine(conns: ConnectionManager): AutomationRunEngine | undefined {
  const dispatcher = conns.dispatcher;
  if (dispatcher === undefined) return undefined;
  const client = new AutomationClient(dispatcher);
  return {
    runAutomation: (automation, request, hooks) =>
      client.run({
        automation,
        ...(request.payload !== undefined ? { payload: request.payload } : {}),
        ...(request.concept !== undefined ? { concept: request.concept } : {}),
        ...(request.targetNodeType !== undefined
          ? { targetNodeType: request.targetNodeType }
          : {}),
        ...(request.includeStepOutput === true ? { includeStepOutput: true } : {}),
        onAccepted: hooks.onAccepted,
        onStep: hooks.onStep,
      }),
  };
}

// openRowInConcepts resolves the row's concept descriptor and opens the
// Concepts tab on it -- the "click a row to open it in the Concepts surface"
// link from a result.
async function openRowInConcepts(
  context: ExtensionContext,
  conns: ConnectionManager,
  conceptId: string,
  rowId: string
): Promise<void> {
  const query = conns.query;
  if (query === undefined || conceptId === '') return;
  let list: Concept[];
  try {
    list = await query.listConcepts();
  } catch (err) {
    window.showErrorMessage(`memQL: ${err instanceof Error ? err.message : String(err)}`);
    return;
  }
  const concept = list.find((c) => c.id === conceptId);
  if (concept === undefined) {
    window.showWarningMessage(
      `memQL: ${conceptId} is not registered on the connected cluster, so row ${rowId} has no Concepts view.`
    );
    return;
  }
  ConceptPanel.open(context, conns, concept);
}

// writeCluster runs a registry write and refreshes the tree, surfacing a
// failure (a rename onto a name already in use, an add onto an existing name)
// as a message rather than letting the rejection escape as a raw
// command-error toast.
//
// It takes the write as a thunk rather than a (cluster, originalName) pair
// because add and edit no longer call the same function: add goes through
// addCluster, which refuses a duplicate name, and edit through upsertCluster
// with the original name for the rename path.
async function writeCluster(
  clustersTree: ClustersTreeProvider,
  write: () => Promise<void>
): Promise<void> {
  try {
    await write();
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

  // The `local` flag gates the mutation confirmation (memql#3309). A QuickPick
  // rather than a text box because it is a boolean, and because the labels can
  // then say what the flag DOES -- "local" on its own is not a question anyone
  // can answer correctly without being told the consequence.
  //
  // The default selection follows the existing value, and a NEW cluster
  // defaults to "not local": absent means not local everywhere else in this
  // codebase, and the safe direction for a flag that disables a confirmation
  // is off.
  const localChoice = await window.showQuickPick(
    [
      {
        label: 'Not local',
        description: 'Running a mutation against this cluster asks for confirmation first',
        value: false,
      },
      {
        label: 'Local',
        description: 'Disposable data -- mutations run without a confirmation prompt',
        value: true,
      },
    ],
    {
      placeHolder:
        existing?.local === true
          ? 'Currently marked local. Is this cluster disposable?'
          : 'Is this cluster disposable? (currently: not local)',
      ignoreFocusOut: true,
    }
  );
  if (localChoice === undefined) {
    return undefined;
  }

  // Every collected field is returned, INCLUDING the empty ones. An empty
  // string means "the user cleared this input", which upsertCluster turns into
  // a key delete. Omitting the key instead would mean "leave whatever is on
  // disk alone" -- so clearing the PAT field to revoke a token left the old
  // token sitting in clusters.yaml while the UI showed it gone.
  //
  // `local` is returned as a real boolean for the same reason: false here means
  // "the user chose not-local", which upsertCluster writes as an ABSENT key
  // (the cockpit's `omitempty` would drop a `local: false` anyway).
  return {
    name: name.trim(),
    endpoint: endpoint.trim(),
    domain: domain.trim(),
    pat: pat.trim(),
    local: localChoice.value,
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
