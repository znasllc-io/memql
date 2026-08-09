import * as fs from 'fs';
import * as path from 'path';
import {
  commands,
  Diagnostic,
  DiagnosticSeverity,
  env,
  ExtensionContext,
  languages,
  Position,
  ProgressLocation,
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

import { signInWithDeviceCode } from './auth/deviceCodeUi.js';
import { runAuthorizationFlow } from './auth/flow.js';
import {
  canSignIn,
  describeSignInFailure,
  performSignIn,
  signInCanRecover,
  type SignInTokenStore,
} from './auth/signin.js';
import { addCluster, defaultClustersPath, readClustersFileSafe, setSelectedCluster, upsertCluster, type ClusterUpdate } from './clusters/file.js';
import { displayLabel, type ClusterConfig } from './clusters/model.js';
import { persistSignIn, signOut as signOutCredentials } from './auth/store.js';
import { CredentialResolver } from './connection/credentials.js';
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

// activate wires the extension's TWO INDEPENDENT SURFACES: the language client
// and the runtime surface (Clusters / Concepts / Runs).
//
// They have separate preconditions and must fail separately. The language
// client needs a memql-lsp binary; the runtime surface needs workspace trust
// and nothing else. This function once resolved the binary FIRST and returned
// when it was missing, which skipped the runtime registration entirely
// (memql#3387): the three views still rendered -- their `when` clause only
// asks for trust -- but no tree data provider, watcher or command was ever
// registered, so they sat permanently empty behind an error message about a
// language server they need nothing from. That is the DEFAULT first-run state
// (no bundled binary, nothing on PATH), and `onView:memqlClusters` is an
// activation event, so clicking into Clusters was enough to hit it.
//
// Neither half may short-circuit the other. startLanguageClient reports its own
// failure and returns.
export function activate(context: ExtensionContext): void {
  startLanguageClient(context);

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

// startLanguageClient boots the memql-lsp client, or reports why it could not.
//
// It returns void rather than a success flag on purpose: no caller may make a
// decision out of the outcome. The runtime surface does not depend on the
// language server, and the one place inside the run surface that genuinely
// does (the CodeLens provider) reads module-level `client` and degrades on
// its own.
function startLanguageClient(context: ExtensionContext): void {
  const serverPath = resolveServerPath(context);
  if (serverPath === undefined) {
    // Names what is ACTUALLY lost. The old wording ("memql-lsp binary not
    // found") read as "the extension is dead", which is exactly the wrong
    // thing to tell someone whose Clusters view is empty for an unrelated
    // reason.
    window.showErrorMessage(
      'MemQL: language features (highlighting, diagnostics, completion, hover) are unavailable -- no memql-lsp binary was found. ' +
        'Set "memql.lsp.serverPath" in your user settings, bundle a platform binary, or install memql-lsp on your PATH. ' +
        'The Clusters, Concepts and Runs views do not need it and still work.'
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
  // The credential resolver (memql#3383 / memql#3385). This is the only place
  // the three things it needs actually exist:
  //
  //   - context.secrets  -- VS Code's SecretStorage, where the LONG-LIVED
  //     refresh token is kept. clusters.yaml is plaintext and owned by the
  //     memQL Cockpit, so the 30-day credential must not live there; the
  //     15-minute access token still does, because the Cockpit reads it too.
  //     See src/connection/credentials.ts for the full split.
  //   - a write-back into clusters.yaml, so a refreshed access token is there
  //     for the next connect (and for the Cockpit) instead of being re-earned.
  //   - the global fetch, for the /oauth/token exchange.
  connections = new ConnectionManager(
    undefined,
    new CredentialResolver({
      secrets: context.secrets,
      persist: async (clusterName, update) => {
        // undefined leaves the on-disk value alone; "" DELETES the key. So the
        // plaintext refresh token is removed only once SecretStorage has taken
        // custody of the rotated one -- otherwise the file holds the only copy.
        await upsertCluster(clustersPath, {
          name: clusterName,
          token: update.token,
          refreshToken: update.clearStoredRefreshToken ? '' : undefined,
        });
      },
    })
  );

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

  // The sign-in persistence seam (memql#3403 / memql#3404).
  //
  // This DELEGATES to src/auth/store.ts rather than reimplementing the split.
  // It is tempting to inline it -- write the access token to clusters.yaml,
  // the refresh token to SecretStorage, done -- and an earlier draft of this
  // file did exactly that. It is wrong for a reason that is invisible from
  // here: SecretStorage cannot be enumerated, so #3404 keeps an INDEX of which
  // clusters have secrets, and sweeps any secret whose cluster is gone. A
  // sign-in that wrote a secret without indexing it would look fine until the
  // next sweep silently deleted the credential it had just stored.
  const storeDeps = {
    secrets: context.secrets,
    writeCluster: (update: ClusterUpdate) => upsertCluster(clustersPath, update),
  };
  const signInStore: SignInTokenStore = {
    persistSignIn: (clusterName, credentials) =>
      persistSignIn(storeDeps, clusterName, {
        accessToken: credentials.accessToken,
        refreshToken: credentials.refreshToken,
        expiresAtEpochSeconds: credentials.expiresAtEpochSeconds,
        // The client_id is written separately by the ClientIdWriter below --
        // it is registered before the tokens exist, so it is not part of this
        // payload. "" leaves the stored value alone.
        clientId: '',
      }),
    signOut: (clusterName) => signOutCredentials(storeDeps, clusterName),
  };

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
        // A failure the CREDENTIAL causes now has a first-class recovery in the
        // editor, so it is offered as the primary action rather than described
        // in prose the operator has to act on somewhere else (memql#3403). The
        // decision of which reasons qualify is signInCanRecover's -- a dial
        // that failed because the cluster is unreachable is not made reachable
        // by a fresh token, and `notConfigured` means there is no endpoint at
        // all. canSignIn is the second half: a cluster naming no identity
        // service has nowhere to sign in to, and a button whose only outcome is
        // an error toast is worse than no button.
        const offer = signInCanRecover(state.reason) && canSignIn(target.cluster);
        const choice = offer
          ? await window.showErrorMessage(`memQL: ${state.message}`, 'Sign in')
          : await window.showErrorMessage(`memQL: ${state.message}`);
        if (choice === 'Sign in') {
          await signInToCluster(target.cluster, {
            clustersPath,
            store: signInStore,
            clustersTree,
          });
        }
      }
    }),
    // memql#3403. Reached from the Clusters view's context menu (which supplies
    // the node) and from the palette (which cannot, so it asks).
    commands.registerCommand('memql.clusters.signIn', async (node?: ClusterNode) => {
      const target = node ?? (await pickCluster(clustersPath));
      if (target === undefined || target.cluster.name === '') {
        return;
      }
      await signInToCluster(target.cluster, {
        clustersPath,
        store: signInStore,
        clustersTree,
      });
    }),
    // The counterpart: forget this cluster's session. The store owns what that
    // means in each of the two places a credential lives (memql#3404).
    commands.registerCommand('memql.clusters.signOut', async (node?: ClusterNode) => {
      const target = node ?? (await pickCluster(clustersPath));
      if (target === undefined || target.cluster.name === '') {
        return;
      }
      try {
        await signInStore.signOut(target.cluster.name);
      } catch (err) {
        window.showErrorMessage(
          `memQL: signing out of "${target.cluster.name}" failed: ${err instanceof Error ? err.message : String(err)}`
        );
        return;
      }
      // A live connection is dialing with the credential just revoked; leaving
      // it up would make "signed out" false for as long as the socket lasts.
      const state = connections?.state;
      if (state !== undefined && state.status !== 'disconnected' && state.clusterName === target.cluster.name) {
        await connections?.disconnect();
      }
      clustersTree.refresh();
      window.showInformationMessage(
        `memQL: signed out of "${target.cluster.name}". Run "memQL: Sign In" to authenticate again.`
      );
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

  // The DELIBERATE device-code sign-in (memql#3411). The fallback fires by
  // itself when the loopback flow proves this host cannot do it, but that
  // costs a two-minute callback deadline first -- so a user who already knows
  // their environment (a container, a hardened network, an SSH session with no
  // browser) can ask for the device code straight away.
  context.subscriptions.push(
    commands.registerCommand('memql.clusters.signInWithCode', async (node?: ClusterNode) => {
      const target = node ?? (await pickCluster(clustersPath));
      if (target === undefined) {
        return;
      }
      if (await signInWithDeviceCode(target.cluster, { clustersPath, secrets: context.secrets })) {
        clustersTree.refresh();
      }
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

interface SignInDeps {
  clustersPath: string;
  store: SignInTokenStore;
  clustersTree: ClustersTreeProvider;
}

// signInToCluster is the editor half of memql#3403's sign-in: progress,
// cancellation, and the two vscode.env capabilities the flow needs injected.
//
// WHY THE PROGRESS IS CANCELLABLE AND WHAT CANCELLING ACTUALLY DOES.
// A browser sign-in parks on a loopback listener for minutes at a time waiting
// for a person to finish a page they may already have closed. A notification
// with no way out would leave the operator watching a spinner with no
// affordance except reloading the window. The token is bridged to the
// AbortSignal src/auth/flow.ts accepts, so cancelling closes the listener and
// rejects with kind `cancelled` -- it is a real abort, not a hidden spinner.
//
// WHY THE FAILURE TOAST IS NOT AWAITED. Every rejection is reported through a
// fire-and-forget message box: awaiting one would keep this command pending
// until a human dismissed a notification, which makes the command unusable from
// any automated caller (the host smoke lane included) and buys nothing -- the
// message offers no choice to read back.
async function signInToCluster(cluster: ClusterConfig, deps: SignInDeps): Promise<boolean> {
  return window.withProgress(
    {
      location: ProgressLocation.Notification,
      title: `memQL: signing in to ${displayLabel(cluster)}`,
      cancellable: true,
    },
    async (progress, token) => {
      const aborter = new AbortController();
      const cancelSubscription = token.onCancellationRequested(() => aborter.abort());
      try {
        progress.report({ message: 'opening your browser...' });
        await performSignIn(cluster, {
          signal: aborter.signal,
          store: deps.store,
          persistClientId: async (clusterName, clientId) => {
            await upsertCluster(deps.clustersPath, { name: clusterName, clientId });
          },
          runFlow: (target, signal) =>
            runAuthorizationFlow(target, {
              signal,
              // asExternalUri is not decoration: under Remote-SSH, Codespaces
              // or a dev container the browser runs on a different machine
              // from this extension host, and the loopback URL has to be
              // rewritten into one that machine can reach. toString(true)
              // skips re-encoding -- the authorization URL's query is already
              // percent-encoded and encoding it twice corrupts the PKCE
              // challenge and the state.
              resolveExternalUri: async (url) =>
                (await env.asExternalUri(Uri.parse(url))).toString(true),
              openExternal: (url) => env.openExternal(Uri.parse(url)),
            }),
        });
      } catch (err) {
        const report = describeSignInFailure(cluster.name, err);
        if (report.level === 'error') {
          void window.showErrorMessage(report.message);
        } else if (report.level === 'warning') {
          void window.showWarningMessage(report.message);
        }
        return false;
      } finally {
        cancelSubscription.dispose();
      }

      deps.clustersTree.refresh();

      // Reconnect ONLY the working cluster. Exactly one connection exists at a
      // time (see ConnectionManager), so dialing a cluster the operator merely
      // signed into would silently switch which cluster every other view is
      // showing. When the sign-in came from a failed connect, this IS the
      // selected cluster and the reconnect is the point.
      //
      // Re-read from disk rather than reusing the in-memory config: the token
      // just persisted is the whole reason to reconnect, and the object this
      // function was called with predates it.
      const result = await readClustersFileSafe(deps.clustersPath);
      if (result.ok && result.file.selectedCluster === cluster.name) {
        const fresh = result.file.clusters.find((c) => c.name === cluster.name);
        if (fresh !== undefined) {
          await connections?.connect(fresh);
          const state = connections?.state;
          if (state?.status === 'error') {
            void window.showErrorMessage(`memQL: ${state.message}`);
            return true;
          }
        }
      }
      void window.showInformationMessage(`memQL: signed in to "${cluster.name}".`);
      return true;
    }
  );
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

  // A JWT ACCESS TOKEN, not a PAT (memql#3383). The old prompt asked for a
  // Personal Access Token, which a bff rejects before any lookup -- so an
  // operator who answered it correctly still could not connect, and nothing
  // said why.
  //
  // The field is now OPTIONAL IN PRACTICE (memql#3403): the extension can mint
  // its own credential through the browser, so leaving this empty and running
  // "memQL: Sign In" is the ordinary path rather than a dead end. Saying so
  // here is the difference between an operator who signs in and one who goes
  // hunting for a token to paste -- the prompt is where they are standing when
  // the question arises.
  const token = await window.showInputBox({
    prompt:
      'Access token (optional): the identity-issued JWT from POST <identity>/oauth/token. Leave empty and run "memQL: Sign In" to authenticate through your browser. A PAT (mql_pat_...) will not work -- the mesh verifies bearers via JWKS.',
    value: existing?.token ?? '',
    ignoreFocusOut: true,
    password: true,
  });
  if (token === undefined) {
    return undefined;
  }

  // The refresh token is collected here as the INGEST path only (memql#3385):
  // the credential resolver takes custody of it on the first exchange, moving
  // it into VS Code's SecretStorage and deleting it from the cockpit-shared
  // plaintext file. It is a 30-day credential, so leaving it on disk is the
  // thing this flow exists to avoid, not a step in it.
  const refreshToken = await window.showInputBox({
    prompt:
      'Refresh token (optional): the `refresh_token` from the same response. Stored in the editor\'s secret storage and used to renew the access token as it expires.',
    // Prefilled from whatever is still PENDING in the file. Normally nothing:
    // once the resolver has taken custody the key is gone from disk, and the
    // secret is deliberately not readable back into a text box. Prefilling the
    // pending case is what stops "add a cluster, then edit it before the first
    // connect" from silently dropping the token the operator just pasted.
    value: existing?.refreshToken ?? '',
    ignoreFocusOut: true,
    password: true,
  });
  if (refreshToken === undefined) {
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
  // disk alone" -- so clearing the token field to revoke a credential left the
  // old one sitting in clusters.yaml while the UI showed it gone.
  //
  // `local` is returned as a real boolean for the same reason: false here means
  // "the user chose not-local", which upsertCluster writes as an ABSENT key
  // (the cockpit's `omitempty` would drop a `local: false` anyway).
  return {
    name: name.trim(),
    endpoint: endpoint.trim(),
    domain: domain.trim(),
    token: token.trim(),
    refreshToken: refreshToken.trim(),
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
//
// The rejection is REPORTED rather than silent (memql#3387). Ignoring a value
// the user can read back in their own settings UI is its own trap: the setting
// is visibly set, the extension visibly does not use it, and nothing on screen
// explains the gap. reportIgnoredWorkspaceServerPath closes that.
function resolveServerPath(context: ExtensionContext): string | undefined {
  const inspected = workspace.getConfiguration('memql.lsp').inspect<string>('serverPath');
  reportIgnoredWorkspaceServerPath(inspected);

  const configured = inspected?.globalValue;
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

// reportIgnoredWorkspaceServerPath tells the user, once per activation, that a
// workspace-scoped memql.lsp.serverPath exists and is being ignored.
//
// A WARNING, not an error: nothing has failed. The user-level value (or the
// bundled binary, or PATH) still resolves, and the message exists only so the
// discrepancy between "the setting is set" and "the extension did not use it"
// is visible rather than something to be discovered by reading source.
//
// An empty workspace value is not reported. Writing the setting and clearing
// it again leaves `""` behind in the file, which asks for nothing and so
// deserves no message.
function reportIgnoredWorkspaceServerPath(
  inspected: { workspaceValue?: string; workspaceFolderValue?: string } | undefined
): void {
  const workspaceScoped = [inspected?.workspaceValue, inspected?.workspaceFolderValue].some(
    (value) => typeof value === 'string' && value.trim() !== ''
  );
  if (!workspaceScoped) {
    return;
  }
  window.showWarningMessage(
    'MemQL: "memql.lsp.serverPath" is set in this workspace and has been IGNORED. ' +
      'The path is read only from your user settings -- a workspace-supplied one would let an opened folder ' +
      'point the extension at any executable on your machine. Set it in User Settings if you meant it.'
  );
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
