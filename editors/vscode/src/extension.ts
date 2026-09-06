import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import {
  CancellationToken,
  CodeLens,
  commands,
  ConfigurationTarget,
  Diagnostic,
  DiagnosticSeverity,
  env,
  EventEmitter,
  ExtensionContext,
  languages,
  OutputChannel,
  Position,
  ProgressLocation,
  Range,
  RelativePattern,
  TreeView,
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

import { SingleFlight } from './async/singleFlight.js';
import {
  runDeviceCodeFlow,
  signInWithDeviceCodeFallback,
  type DeviceAuthorization,
} from './auth/deviceCode.js';
import {
  announceDeviceCodeFallback,
  deviceCodeProgressLine,
  showDeviceCodeActions,
} from './auth/deviceCodeUi.js';
import { isAuthFlowError } from './auth/errors.js';
import {
  canSignIn,
  describeSignInFailure,
  performSignIn,
  selectSignInRunner,
  signInCanRecover,
  type SignInFlow,
  type SignInTokenStore,
} from './auth/signin.js';
import {
  OfferMemory,
  decidePasskeyOffer,
  enrolmentStillNeeded,
  passkeyAlreadyEnrolledMessage,
  passkeyOfferMessage,
} from './auth/passkeyOffer.js';
import {
  ClusterCredentialStore,
  persistSignIn,
  reconcileClusterCredentials,
  signOut as signOutCredentials,
} from './auth/store.js';
import { revokeRefreshToken, signOutMessage, type RevocationOutcome } from './auth/revoke.js';
import { addCluster, defaultClustersPath, readClustersFileSafe, setSelectedCluster, upsertCluster, type ClusterUpdate } from './clusters/file.js';
import { refreshTokenFieldPlan, resolveCredentialInput, tokenFieldPlan } from './clusters/form.js';
import { displayLabel, needsAuth, type ClusterConfig } from './clusters/model.js';
import { clusterRowText } from './clusters/status.js';
import { ClusterPresence } from './clusters/presence.js';
import {
  claimProbeSignal,
  probeClaimStateForCluster,
  setupUrlForCluster,
} from './clusters/claimState.js';
import {
  isFirstCredentialPending,
  receiptNamesAnotherCluster,
  resolveOwnershipRoute,
  type OwnershipRoute,
} from './clusters/ownershipRoute.js';
import {
  SIGN_IN_DEVICE_CODE,
  SIGN_IN_EDIT_CLUSTER,
  SIGN_IN_RETRY,
  signInRecoveryActions,
} from './clusters/signInRecovery.js';
import { removeClusterCompletely, saveClusterEdit } from './clusters/registry.js';
import { AddClusterPanel } from './webview/addClusterPanel.js';
import { currentEditorKind } from './webview/theme.js';
import {
  OFFER_DISMISS,
  OFFER_MESSAGE,
  OFFER_SWITCH,
  THEME_OFFER_ANSWERED_KEY,
  memqlThemeFor,
  shouldOfferMemqlTheme,
} from './theme/themeOffer.js';
import { CredentialResolver } from './connection/credentials.js';
import { apiBaseUrlFor, composeEndpointFromDomain, identityBaseUrlFor } from './connection/endpoint.js';
import { ConnectionManager, type ConnectionState } from './connection/manager.js';
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
import { TrainingCodeLensProvider } from './constructs/trainingLens.js';
import { TrainingDecorations } from './constructs/decorations.js';
import { ClusterCatalogPublisher } from './constructs/clusterCatalog.js';
import {
  COMMAND_DEMOTE,
  COMMAND_DRY_RUN,
  COMMAND_PROMOTE,
  COMMAND_REBUILD,
  COMMAND_STAGE,
  COMMAND_SHOW_LIST,
  COMMAND_TRY_IN_SESSION,
  TRAINING_STATE_CAPABILITY,
  TRAINING_STATE_METHOD,
  parseTrainingConstructs,
  type TrainingConstruct,
} from './state/training.js';
import {
  TrainingActions,
  type TrainingEngine,
  type TrainingOutcome,
  type TrainingRequest,
  type TrainingScope,
} from './training/actions.js';
import {
  assembleClosure,
  assembleConstruct,
  type TrainingBundle,
  type TrainingWorkspace,
} from './training/closure.js';
import { outcomeReport } from './training/outcomeReport.js';
import type { TrainingPrompt } from './training/report.js';
import { sessionLensPlans } from './training/session.js';
import {
  defaultReceiptPath,
  readReceipt,
  recordedDomain,
  recordedOwner,
  recordedStackDir,
} from './install/receipt.js';
import { maskHomePath, redactForDisplay } from './install/secrets.js';
import {
  briefMessage,
  recordDiagnostic,
  type DiagnosticSink,
} from './state/diagnostics.js';
import { runCapabilityScript } from './install/runner.js';
import { EnrolmentError, openEnrolmentLink } from './install/enrolment.js';
import { OWNERSHIP_LINK_TTL, OwnershipError, mintOwnershipLink } from './clusters/takeOwnership.js';
import { defaultRunsDir, reconcileOrphanedRuns } from './state/runLog.js';
import {
  DEPLOYMENT_CONCEPT,
  DEPLOYMENT_NODE_SPEC_CONCEPT,
} from './state/deploymentHistory.js';
import { resolveInstallRoot } from './install/root.js';
import { ClusterVersionRefresher } from './version/learners.js';
import { createVersionCollector } from './version/collectors.js';
import { releaseCache } from './version/releaseCache.js';
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
  runConfigPath,
  upsertRunConfig,
  removeRunConfig,
  writeRunConfigs,
  RUN_CONFIG_RELATIVE_PATH,
  type AutomationRunConfig,
  type RunConfig,
} from './run/runConfig.js';
import { ClustersTreeProvider, type ClusterNode } from './views/clustersTree.js';
import { DeploymentsTreeProvider, type DeploymentNode } from './views/deploymentsTree.js';
import {
  connectionContextKeys,
  CLUSTER_SELECTED_KEY,
  CONNECTED_KEY,
  NOT_CONNECTED_REFUSAL,
} from './state/connectionContext.js';
import { DEPLOYMENTS_INSTANCE_KEY } from './state/deploymentsCatalog.js';
import { DeploymentPanel, type DeploymentPanelDeps } from './webview/deploymentPanel.js';
import { SITE_CONCEPT, consoleTarget } from './clusters/consoleUrl.js';
import { isCatalogUri } from './constructs/catalogTarget.js';
import { roleVisibility } from './deploy/actions.js';
import { DeployControlClient } from '@znasllc-io/memql-sdk-core/deploy';
import { IdentityAdminClient } from '@znasllc-io/memql-sdk-core/identityadmin';
import { DataTreeProvider } from './views/dataTree.js';
import { ConstructsTreeProvider, type ConstructNode } from './views/constructsTree.js';
import { ReadonlyMarker } from './constructs/readonlyDecorations.js';
import {
  CLUSTER_DOCUMENT_SCHEME,
  detailsRefusal,
  panelClusterRefusal,
} from './constructs/clusterDocument.js';
import {
  ClusterDocumentLens,
  ClusterDocumentProvider,
  openClusterDocument,
} from './constructs/clusterDocuments.js';
import { ConstructPanel, openFileAtSignature, type ConstructPanelDeps } from './webview/constructPanel.js';
import {
  catalogFrom,
  classifyCatalogFailure,
  toCatalogConstruct,
  type CatalogState,
} from './state/constructCatalog.js';
import { ConstructsClient } from '@znasllc-io/memql-sdk-core/constructs';
import { RunsTreeProvider, type RunsTreeNode } from './views/runsTree.js';
import { AutomationRunPanel, type AutomationPanelHost } from './webview/automationPanel.js';
import { ConnectionPanel } from './webview/connectionPanel.js';
import { ConceptPanel } from './webview/conceptPanel.js';
import {
  describeOpenRequest,
  openRequestUri,
  parseOpenRequest,
  type OpenArtifactRequest,
  type OpenRequest,
} from './handoff/openRequest.js';
import { landingFor, matchCluster, workspaceCandidates } from './handoff/resolve.js';
import { storePending, takePending } from './handoff/pending.js';
import {
  ARTIFACT_DOCUMENT_SCHEME,
  artifactContentUrl,
  artifactDelivery,
  artifactFileName,
  artifactProvenanceLine,
  languageIdFor,
} from './library/artifactDocument.js';
import {
  ArtifactDocumentProvider,
  offerArtifactSave,
  openArtifactDocument,
} from './library/artifactDocuments.js';
import { resolveArtifactMeta } from './library/artifactMeta.js';
import { ResultPanel, RunPanel, conceptMap, type RunPanelHost } from './webview/runPanel.js';

let client: LanguageClient | undefined;
let connections: ConnectionManager | undefined;

/**
 * What the console handoff needs that only registerRuntimeSurface builds.
 *
 * The URI handler is registered in activate(), which runs in an UNTRUSTED
 * window too -- so the things it reaches for (the registry path, the tree it
 * refreshes after a write, the panel's dependency block) may legitimately not
 * exist yet. Held as one record rather than as four more module-scoped `let`s
 * so "is the runtime surface up" stays a single question with a single answer.
 */
interface HandoffSurface {
  context: ExtensionContext;
  clustersPath: string;
  clustersTree: ClustersTreeProvider;
  constructPanelDeps: () => ConstructPanelDeps;
}
let handoffSurface: HandoffSurface | undefined;

// The output-channel family (memql#4194). MemQL Training already existed; these
// two carry what the information policy moved OUT of panels, toasts and
// tooltips: Install holds capability stderr and run refusals, Connection holds
// transport, sign-in and language-server failures. Created in activate() so the
// language-client path can reach them in an untrusted window too.
let installOutput: OutputChannel | undefined;
let connectionOutput: OutputChannel | undefined;

function sinkFor(output: OutputChannel | undefined): DiagnosticSink {
  return { appendLine: (line) => output?.appendLine(line) };
}

/** Record a failure's raw detail into a channel, redacted (memql#4194). */
function noteDiagnostic(output: OutputChannel | undefined, headline: string, detail: string): void {
  recordDiagnostic(sinkFor(output), headline, redactForDisplay(detail, os.homedir()), new Date().toISOString());
}

/**
 * The policy's toast shape: a short headline, the caller's own actions, and a
 * "Show details" that reveals the channel holding the record. Returns the
 * caller's action when one was clicked; reveal is handled here.
 */
async function offerDetails(
  severity: 'error' | 'warning',
  output: OutputChannel | undefined,
  headline: string,
  ...actions: string[]
): Promise<string | undefined> {
  const details = 'Show details';
  const choice = await (severity === 'error'
    ? window.showErrorMessage(headline, ...actions, details)
    : window.showWarningMessage(headline, ...actions, details));
  if (choice === details) {
    output?.show(true);
    return undefined;
  }
  return choice;
}

/** The action an enrolment failure toast carries when the link survives it. */
const COPY_ENROLMENT_LINK = 'Copy link';

/**
 * Puts a minted enrolment link on the clipboard (memql#4618).
 *
 * The shape `copyRecoveryKey` established for the one-time recovery key
 * (webview/addClusterPanel.ts): copy, then say plainly whether it worked. Loud
 * on failure, because an operator who believes they copied a credential they did
 * not is worse off than one who was told to get it another way.
 *
 * THE LINK IS NEVER LOGGED. install/enrolment.ts states the rule -- it goes from
 * the mint to the opener and is written nowhere -- and the clipboard is where
 * the operator just asked for it, which a diagnostic channel is not. So the
 * failure record below carries the clipboard error and not the link.
 */
async function copyEnrolmentLink(url: string): Promise<void> {
  try {
    await env.clipboard.writeText(url);
  } catch (err) {
    noteDiagnostic(
      connectionOutput,
      'copying the enrolment link failed',
      err instanceof Error ? err.message : String(err)
    );
    void offerDetails(
      'error',
      connectionOutput,
      'MemQL: the enrolment link could not be copied to the clipboard.'
    );
    return;
  }
  void window.showInformationMessage(
    `MemQL: enrolment link copied. Open it in a browser on a machine that can reach this cluster -- it is single-use and expires in ${OWNERSHIP_LINK_TTL}.`
  );
}

// Which clusters will not get the passkey offer this session: those the
// operator declined it on (memql#3902), and those the ownership walk has
// claimed (memql#4078). Module-scoped so it lives exactly as long as the
// extension host does: both markers are session answers, not preferences
// written to disk. See OfferMemory.
const passkeyOfferMemory = new OfferMemory();

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
export function activate(context: ExtensionContext): MemqlExtensionApi {
  installOutput = window.createOutputChannel('MemQL Install');
  connectionOutput = window.createOutputChannel('MemQL Connection');
  context.subscriptions.push(installOutput, connectionOutput);

  // The console's handoff (memql#4251). Registered HERE rather than inside the
  // trust gate, and before either surface comes up, because a `vscode://` link
  // is what ACTIVATES the extension: `onUri` starts it, VS Code delivers the
  // uri to whatever handler exists when activate() returns, and a handler
  // registered behind the trust gate would simply not exist for the link that
  // woke the editor. What the gate protects is the ACTION, which handleOpenUri
  // refuses on its own when the runtime surface is not up.
  context.subscriptions.push(
    window.registerUriHandler({
      handleUri: (uri) => {
        void handleOpenUri(uri).catch(noteHandoffFailure);
      },
    })
  );

  startLanguageClient(context);

  // The one-time offer to wear the brand in the workbench too (memql#4421).
  //
  // OUTSIDE THE TRUST GATE, deliberately. It reads two editor settings and
  // writes one; it touches no credential, opens no connection and looks at
  // nothing in the workspace. Putting it behind the gate would mean an
  // operator in a restricted folder never learns the themes exist.
  offerMemqlThemeOnce(context);

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

  // The host smoke lane's handle on the handoff (memql#4251). `ext.activate()`
  // resolves with whatever this returns, which is the only way a test running
  // inside a real editor can drive a uri without asking the operator to click
  // VS Code's own "allow this extension to open the URI" prompt.
  return { handleOpenUri };
}

/**
 * Show the MemQL colour-theme offer, at most once ever.
 *
 * The adapter half of src/theme/themeOffer.ts, which owns every decision here
 * and is tested without an editor. This function's only job is to fetch the
 * three inputs, present the notification, and record the answer.
 *
 * ANY RESOLUTION COUNTS AS AN ANSWER, including the operator dismissing the
 * toast with its close button (`undefined`). That is deliberately wider than
 * "they pressed one of our two buttons": a VS Code notification carrying
 * actions stays on screen until it is acted on, so `undefined` here means a
 * person deliberately got rid of it -- and re-asking someone who has already
 * waved it away is exactly the nagging D4 rules out. The cost of reading it
 * this way is one lost offer in the rare case the notification is dismissed by
 * something other than the operator; the cost of the other reading is a prompt
 * that returns every session forever.
 *
 * The theme is written to GLOBAL settings, because `workbench.colorTheme` is a
 * property of this person's editor rather than of the folder they opened. A
 * workspace-scoped write would re-theme one project and look broken in the
 * next window.
 */
function offerMemqlThemeOnce(context: ExtensionContext): void {
  const editorKind = currentEditorKind();
  const decision = {
    answered: context.globalState.get<boolean>(THEME_OFFER_ANSWERED_KEY) === true,
    activeThemeLabel: workspace.getConfiguration('workbench').get<string>('colorTheme') ?? '',
    editorKind,
  };
  if (!shouldOfferMemqlTheme(decision)) return;

  void (async () => {
    const choice = await window.showInformationMessage(
      OFFER_MESSAGE,
      OFFER_SWITCH,
      OFFER_DISMISS
    );
    await context.globalState.update(THEME_OFFER_ANSWERED_KEY, true);
    if (choice !== OFFER_SWITCH) return;
    await workspace
      .getConfiguration('workbench')
      .update('colorTheme', memqlThemeFor(editorKind), ConfigurationTarget.Global);
  })();
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
    // `scheme: 'file'` (memql#4248): cluster documents (`memql-cluster:`) must
    // not receive import-resolution diagnostics for files that are not on disk.
    //
    // IT COSTS MORE THAN IT LOOKS LIKE, and deliberately. The narrowing is by
    // scheme rather than by exclusion, so it also drops `untitled:` MemQL
    // buffers (a new unsaved file gets no completion or hover until it is
    // saved) and `git:` documents (no language features in a diff view).
    // Stated here so the next reader recognises it as the price of the rule
    // rather than as a bug -- widening the selector puts the diagnostics back
    // on cluster documents, which is the defect this fixed.
    documentSelector: [{ language: 'memql', scheme: 'file' }],
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
    const detail = err instanceof Error ? err.message : String(err);
    noteDiagnostic(connectionOutput, 'the language server failed to start', detail);
    void offerDetails('error', connectionOutput, 'MemQL: the language server failed to start.');
  });
}

// -----------------------------------------------------------------------------
// The console handoff: vscode://znasllc.memql/open (memql#4251)
// -----------------------------------------------------------------------------

/** What `activate()` hands back, so the host smoke lane can drive a link. */
export interface MemqlExtensionApi {
  handleOpenUri(uri: Uri): Promise<HandoffOutcome>;
}

/**
 * What one handoff did, for a caller that cannot see a toast.
 *
 * `detail` is a short machine-ish phrase (the refused field, the landing kind),
 * never a raw error: the raw text goes to the MemQL Connection channel through
 * the redactor, like every other failure in this file (memql#4194).
 */
export interface HandoffOutcome {
  /**
   * `saved` is the artifact path's own landing (memql#4748): the content was
   * delivered as a FILE rather than a buffer, because it is binary or past the
   * size an editor takes. It is distinct from `opened` because nothing is on
   * screen afterwards -- a caller that treats them as the same cannot tell "it
   * is in front of you" from "it is on your disk".
   */
  outcome: 'refused' | 'untrusted' | 'noCluster' | 'notLoaded' | 'opened' | 'saved';
  detail: string;
}

/** How long a handoff waits for the cluster it just asked to be selected. */
const HANDOFF_CONNECT_TIMEOUT_MS = 30_000;

/** How long a handoff waits for a folder it just added to appear in the workspace. */
const HANDOFF_ADD_FOLDER_TIMEOUT_MS = 5_000;

/**
 * Where a DETACHED handoff promise's failure goes.
 *
 * Three call sites hand a promise to nobody -- the uri handler VS Code calls
 * (whose return value it ignores), the replay after a reload, and the
 * add-a-cluster offer -- and an unattached rejection in any of them is an
 * extension-host log line the operator never sees. A throw out of
 * `memql.clusters.select` is the realistic one, and today it showed them
 * nothing at all. Same shape as `void client.start().catch(...)` above: the raw
 * text to the channel through the redactor, a sentence to the operator.
 */
function noteHandoffFailure(err: unknown): void {
  const detail = err instanceof Error ? err.message : String(err);
  noteDiagnostic(connectionOutput, 'Handoff from the console failed', detail);
  void offerDetails('error', connectionOutput, 'MemQL: the console handoff could not be completed.');
}

/**
 * Opens what a console link names, in four steps: READ the link, MATCH a
 * registered cluster, CONNECT to it, LAND on the construct.
 *
 * WHAT A LINK MAY DO, AND WHAT IT MAY NEVER DO. A link may select a cluster
 * that is ALREADY REGISTERED, connect it through the flows the Clusters view
 * uses (sign-in prompts included), and open a document. It may never add a
 * cluster, sign in silently, run anything, or write settings. The one write it
 * can reach -- adding a cluster -- happens only after a person clicks the offer
 * AND completes every prompt, which is `promptForCluster` refusing to be
 * automated rather than a check made here.
 *
 * VS CODE'S OWN PROMPT IS THE CONSENT GATE. The editor asks "allow this
 * extension to open the URI?" before this function is ever called, so a second
 * confirmation here would train the operator to click through both.
 *
 * NOTHING FROM THE LINK REACHES A PATH. `originPath` comes from the cluster's
 * catalog after the construct is found; the link carries only a domain, a kind
 * and a registry key, each validated in handoff/openRequest.ts.
 */
async function handleOpenUri(uri: Uri): Promise<HandoffOutcome> {
  // 1. READ THE LINK. Any web page can fire one, so this is the only step that
  //    runs before anything is trusted.
  const request = parseOpenRequest({ path: uri.path, query: uri.query });
  if ('error' in request) {
    window.showErrorMessage(`MemQL: this link cannot be opened -- ${request.error}.`);
    noteDiagnostic(connectionOutput, 'Handoff refused', request.error);
    return { outcome: 'refused', detail: request.error };
  }

  const surface = handoffSurface;
  if (connections === undefined || surface === undefined) {
    // Not an error: an untrusted window is a deliberate state, and the remedy
    // is one click in the workbench rather than anything about the link.
    //
    // LOGGED LIKE EVERY OTHER OUTCOME. The channel is created in activate(), so
    // it exists here even though the runtime surface does not -- and this is
    // the outcome most likely to be reported as "the link did nothing", which
    // is exactly the report a missing line cannot be answered from.
    noteDiagnostic(
      connectionOutput,
      'Handoff from the console',
      `${request.domain} ${describeOpenRequest(request)} -> untrusted workspace`
    );
    window.showWarningMessage('MemQL: trust this workspace to open constructs from the console.');
    return { outcome: 'untrusted', detail: 'the runtime surface is not registered' };
  }
  const manager = connections;

  // 2. MATCH A REGISTERED CLUSTER. The link names a DOMAIN and the registry is
  //    keyed by name, so the match is the domain the add/edit flow stored (or
  //    the endpoint it composes). An unregistered domain is offered, never
  //    added.
  const registry = await readClustersFileSafe(surface.clustersPath);
  if (!registry.ok) {
    window.showErrorMessage(`MemQL: ${registry.error}`);
    // 'Handoff from the console', not 'Handoff refused': the link was fine and the
    // REGISTRY was not, which is the outcome this returns. A headline naming
    // the other outcome sends a reader looking for a malformed link.
    noteDiagnostic(connectionOutput, 'Handoff from the console', `the cluster registry could not be read: ${registry.error}`);
    return { outcome: 'noCluster', detail: 'the cluster registry could not be read' };
  }
  const match = matchCluster(registry.file.clusters, request.domain, registry.file.selectedCluster);
  if (match.kind === 'none') {
    noteDiagnostic(
      connectionOutput,
      'Handoff from the console',
      `${request.domain} ${describeOpenRequest(request)} -> no registered cluster`
    );
    // DETACHED, never awaited (the shape memql#4079 established above). A
    // non-modal notification carrying a BUTTON does not time out -- it sits in
    // the notification centre until somebody answers it -- so awaiting one
    // would leave this handoff unresolved for as long as it is ignored. The
    // outcome is already decided either way: nothing opened, and adding a
    // cluster is a separate act the operator performs afterwards.
    void (async () => {
      const choice = await window.showInformationMessage(
        `MemQL: no registered cluster for ${request.domain}.`,
        'Add cluster...'
      );
      if (choice !== 'Add cluster...') return;
      // The ORDINARY prompts, prefilled with what the link stated and nothing
      // else. A dismissal at any field returns undefined and writes nothing.
      const edited = await promptForCluster({
        name: request.domain.split('.')[0] ?? request.domain,
        domain: request.domain,
        endpoint: composeEndpointFromDomain(request.domain),
      });
      if (edited) {
        await writeCluster(surface.clustersTree, () => addCluster(surface.clustersPath, edited));
      }
    })().catch(noteHandoffFailure);
    return { outcome: 'noCluster', detail: `no registered cluster for ${request.domain}` };
  }
  const cluster = match.cluster;
  if (match.alsoMatched.length > 0) {
    // NAMED, NOT HIDDEN. Two entries for one domain is a developer with two
    // credentials, and which one answered is the first thing they will want to
    // know if the catalog looks wrong.
    void window.showInformationMessage(
      `MemQL: ${request.domain} is registered as ${cluster.name}; also as ${match.alsoMatched.join(', ')}.`
    );
  }

  // 3. CONNECT. Through `memql.clusters.select`, which is the SAME command the
  //    Clusters view runs -- it persists the selection and dials with the
  //    existing sign-in prompts, so a handoff cannot acquire a credential by a
  //    route the tree does not have.
  const current = manager.state;
  if (current.status !== 'connected' || current.clusterName !== cluster.name) {
    await commands.executeCommand('memql.clusters.select', { cluster, selected: false });
    const settled = await awaitConnection(manager, cluster.name, HANDOFF_CONNECT_TIMEOUT_MS);
    if (settled.status !== 'connected' || settled.clusterName !== cluster.name) {
      const why =
        settled.status === 'error'
          ? briefMessage(settled.message)
          : `${cluster.name} did not connect`;
      // The select command has already reported its own failure in full (with
      // the Sign in offer where one applies); this only says which handoff was
      // abandoned, so the two toasts do not say the same thing twice.
      noteDiagnostic(
        connectionOutput,
        'Handoff from the console',
        `${cluster.name} ${describeOpenRequest(request)} -> not connected (${settled.status})`
      );
      // THE ARTIFACT PATH SAYS ONE MORE THING, and the asymmetry is deliberate
      // (memql#4748). A construct link is clicked in a browser tab the person
      // is still looking at, so the select command's own toast lands where they
      // are. An artifact link is fired by MemQL OS from a desktop the editor
      // has just covered up: the OS shows "VS Code did not answer" only if the
      // page is STILL VISIBLE after 2.5s, and taking focus hides it -- so
      // without a sentence here, a signed-out cluster is a window that came
      // forward and did nothing, with the one explanation on a surface the
      // person can no longer see. Non-modal, and it names the cluster rather
      // than repeating the select command's diagnosis.
      if (request.target === 'artifact') {
        void window.showInformationMessage(
          `MemQL: sign in to ${cluster.name} to open this artifact.`
        );
      }
      return { outcome: 'noCluster', detail: why };
    }
  }

  // 3b. AN ARTIFACT FORKS HERE, before the catalog. An artifact is a ROW, not a
  //     loaded construct: it has no kind/name in ListConstructs, no origin path,
  //     and no landing to choose between -- so the catalog read below would
  //     always miss, and reporting that miss as "this cluster has no artifact
  //     <id> loaded" would be a sentence about the wrong thing entirely.
  if (request.target === 'artifact') {
    return await landOnArtifact(manager, cluster, request);
  }

  // 4. LAND ON THE CONSTRUCT. The catalog is the authority for where its file
  //    lives; the link never says.
  const dispatcher = manager.dispatcher;
  if (dispatcher === undefined) {
    noteDiagnostic(
      connectionOutput,
      'Handoff from the console',
      `${cluster.name} ${describeOpenRequest(request)} -> no dispatcher`
    );
    return { outcome: 'noCluster', detail: `${cluster.name} is not connected` };
  }
  let catalog;
  try {
    catalog = (await new ConstructsClient(dispatcher).listConstructs()).constructs;
  } catch (err) {
    // A cluster predating ListConstructs answers with an envelope the client
    // does not recognise, which throws. NOT rendered as an empty catalog: that
    // would read as "this cluster has nothing loaded", which is the one wrong
    // answer available here.
    const detail = err instanceof Error ? err.message : String(err);
    noteDiagnostic(connectionOutput, `reading the catalog from "${cluster.name}" failed`, detail);
    void offerDetails('error', connectionOutput, `MemQL: ${cluster.name} could not list its constructs.`);
    return { outcome: 'noCluster', detail: 'the catalog could not be read' };
  }

  const found = catalog.find((c) => c.kind === request.kind && c.name === request.name);
  const existing = found === undefined ? undefined : await findInWorkspace(found.originPath);
  const landing = landingFor({
    construct: found === undefined ? undefined : { origin: found.origin, originPath: found.originPath },
    existingIn: existing === undefined ? undefined : { folder: existing.folder, relativePath: existing.relativePath },
    clusterLocal: cluster.local === true,
    checkout: recordedStackDir(await readReceipt(defaultReceiptPath()).catch(() => null)),
    workspaceFolderCount: workspace.workspaceFolders?.length ?? 0,
  });

  // ONE LINE FOR EVERY HANDOFF, whatever happened -- written BEFORE the landing
  // is performed, so a landing that then fails is a second line rather than no
  // line at all. The channel is where an operator reconstructs "I clicked the
  // link and got the wrong thing" afterwards, and a path that logged only on
  // failure would have nothing to compare against.
  noteDiagnostic(
    connectionOutput,
    'Handoff from the console',
    `${cluster.name} ${describeOpenRequest(request)} -> ${landing.kind}`
  );

  if (landing.kind === 'notLoaded' || found === undefined) {
    void window.showInformationMessage(
      `MemQL: ${cluster.name} has no ${request.kind} ${request.name} loaded.`
    );
    return { outcome: 'notLoaded', detail: landing.kind };
  }

  try {
    switch (landing.kind) {
      case 'detailPage':
        // Promoted: there is no file anywhere, and the page renders the source
        // the cluster holds.
        // `cluster.name` is the cluster this catalog was listed from, which is
        // what the panel's buttons must be measured against later.
        ConstructPanel.open(surface.context, toCatalogConstruct(found), surface.constructPanelDeps(), cluster.name);
        return { outcome: 'opened', detail: landing.kind };

      case 'workspaceFile':
        // Defined by construction: landingFor answers workspaceFile only when
        // it was given an `existingIn`, which is exactly when `existing` is set.
        await openFileAtSignature(existing!.uri, found.kind, found.name);
        return { outcome: 'opened', detail: landing.kind };

      case 'clusterDocument':
        await openClusterDocument({
          cluster: cluster.name,
          originPath: found.originPath,
          kind: found.kind,
          name: found.name,
        });
        return { outcome: 'opened', detail: landing.kind };

      case 'openCheckout':
        // The inline finish is built HERE, beside the other landings, because
        // it IS one: once the checkout is part of this workspace the answer is
        // the workspaceFile landing, and failing that the clusterDocument one.
        // openCheckoutFor decides how the folder arrives, not what to do with
        // the construct afterwards.
        return await openCheckoutFor(surface, request, landing.checkout, landing.mode, async () => {
          const nowIn = await findInWorkspace(found.originPath);
          if (nowIn !== undefined) {
            await openFileAtSignature(nowIn.uri, found.kind, found.name);
            noteDiagnostic(
              connectionOutput,
              'Handoff from the console',
              `${cluster.name} ${request.kind} ${request.name} -> workspaceFile (after adding the checkout)`
            );
            return { outcome: 'opened', detail: 'workspaceFile' };
          }
          await openClusterDocument({
            cluster: cluster.name,
            originPath: found.originPath,
            kind: found.kind,
            name: found.name,
          });
          noteDiagnostic(
            connectionOutput,
            'Handoff from the console',
            `${cluster.name} ${request.kind} ${request.name} -> clusterDocument (the added checkout does not hold it)`
          );
          return { outcome: 'opened', detail: 'clusterDocument' };
        });
    }
  } catch (err) {
    const detail = err instanceof Error ? err.message : String(err);
    noteDiagnostic(connectionOutput, `opening ${request.kind} ${request.name} failed`, detail);
    void offerDetails('error', connectionOutput, `MemQL: ${request.kind} ${request.name} could not be opened.`);
    // `${kind} failed`, never the bare landing kind: a caller comparing details
    // must not read a failure as the landing it was aiming for.
    return { outcome: 'noCluster', detail: `${landing.kind} failed` };
  }
}

/**
 * Step 4 for an ARTIFACT: read the row, decide where its content belongs, put
 * it there (memql#4748).
 *
 * THE ROW IS READ BEFORE A SINGLE BYTE IS FETCHED, and the order is the design.
 * `libraryArtifactById` filters on `ownerUserId == actor.userId`, so it answers
 * three questions at once -- does this artifact exist, may this person read it,
 * and what is it -- over the stream that is already open. Only then is there
 * anything to decide, and the decision (buffer or file) is made from the row's
 * own `kind` / `format` / `mimeType` / `size`. Downloading first to find out
 * what arrived is the one shape this must not have: it costs the whole transfer
 * to answer a question the row already answered, and it costs it in exactly the
 * case -- a large binary -- where the answer is "do not put this in memory".
 *
 * A 404 AFTER ALL THAT MEANS SOMETHING ELSE. The content route makes every
 * refusal a 404 on purpose (not-found and not-yours are deliberately
 * indistinguishable), but this path has already established both under the same
 * actor -- so a 404 here is the third case: the cluster has no downloadable
 * body for this kind of artifact. `document`-backed artifacts are the live
 * example; the export route serves note / generated_output / memory bodies and
 * file bytes, and nothing else.
 */
async function landOnArtifact(
  manager: ConnectionManager,
  cluster: ClusterConfig,
  request: OpenArtifactRequest
): Promise<HandoffOutcome> {
  const query = manager.query;
  const bearer = manager.bearer;
  if (query === undefined || bearer === undefined) {
    noteDiagnostic(
      connectionOutput,
      'Handoff from the console',
      `${cluster.name} ${describeOpenRequest(request)} -> no connection`
    );
    return { outcome: 'noCluster', detail: `${cluster.name} is not connected` };
  }

  let lookup;
  try {
    lookup = await resolveArtifactMeta(query, request.id);
  } catch (err) {
    const detail = err instanceof Error ? err.message : String(err);
    noteDiagnostic(connectionOutput, `reading artifact ${request.id} from "${cluster.name}" failed`, detail);
    void offerDetails('error', connectionOutput, `MemQL: ${cluster.name} could not read this artifact.`);
    return { outcome: 'noCluster', detail: 'the artifact could not be read' };
  }
  if (!lookup.found) {
    noteDiagnostic(
      connectionOutput,
      'Handoff from the console',
      `${cluster.name} ${describeOpenRequest(request)} -> no such artifact`
    );
    // ONE SENTENCE FOR TWO CONDITIONS, because the graph gives one answer for
    // both: an owner-gated query cannot distinguish "there is no such row" from
    // "that row is not yours", and a message that guessed would be wrong half
    // the time in the direction that leaks.
    void window.showInformationMessage(
      `MemQL: ${cluster.name} has no artifact you can open at that id.`
    );
    return { outcome: 'notLoaded', detail: 'no such artifact' };
  }

  const meta = lookup.meta;
  const fileName = artifactFileName(meta);
  const base = apiBaseUrlFor(cluster);
  if (base === undefined) {
    noteDiagnostic(
      connectionOutput,
      'Handoff from the console',
      `${cluster.name} ${describeOpenRequest(request)} -> no https address`
    );
    void window.showErrorMessage(
      `MemQL: no https address is known for ${cluster.name}. Give it a domain in ~/.memql/clusters.yaml.`
    );
    return { outcome: 'noCluster', detail: 'no https address for the cluster' };
  }

  const delivery = artifactDelivery(meta);
  // ONE LINE FOR EVERY HANDOFF, written BEFORE the landing is performed -- the
  // rule the construct path keeps, and the provenance record the epic asks for:
  // which cluster, which row, what it is and how big.
  noteDiagnostic(
    connectionOutput,
    'Handoff from the console',
    `${artifactProvenanceLine(cluster.name, meta)} -> ${delivery.kind} as ${fileName}` +
      (lookup.archived ? ' (archived)' : '')
  );

  try {
    if (delivery.kind === 'editor') {
      await openArtifactDocument(
        { cluster: cluster.name, artifactId: request.id, fileName },
        languageIdFor(meta.format, meta.mimeType)
      );
      return { outcome: 'opened', detail: 'artifactDocument' };
    }

    // SAVE TO DISK, with the reason said out loud BEFORE the dialog. A save
    // dialog appearing where an editor was expected reads as the wrong thing
    // having happened; the same dialog after "this is a PDF" reads as the right
    // one. Detached, because a non-modal notification does not time out and the
    // dialog must not wait behind it.
    void window.showInformationMessage(
      `MemQL: ${fileName} is offered as a file because ${delivery.reason}.`
    );
    const saved = await offerArtifactSave({
      url: artifactContentUrl(base, request.id),
      bearer,
      fileName,
    });
    if (saved.outcome === 'cancelled') {
      // NOT AN ERROR, and the same answer openCheckoutFor gives a cancelled
      // modal: nothing opened, nothing failed, and `refused` is reserved for a
      // link this extension would not act on at all.
      noteDiagnostic(
        connectionOutput,
        'Handoff from the console',
        `${cluster.name} ${describeOpenRequest(request)} -> save cancelled`
      );
      return { outcome: 'notLoaded', detail: 'cancelled' };
    }
    if (saved.outcome === 'failed') {
      noteDiagnostic(
        connectionOutput,
        `saving ${fileName} from "${cluster.name}" failed`,
        `${saved.failure.reason}: ${saved.failure.detail}`
      );
      void offerDetails('error', connectionOutput, `MemQL: ${fileName} could not be saved.`);
      return { outcome: 'noCluster', detail: `save failed (${saved.failure.reason})` };
    }
    void window.showInformationMessage(`MemQL: saved ${fileName}.`);
    noteDiagnostic(connectionOutput, 'Handoff from the console', `${cluster.name} saved ${fileName}`);
    return { outcome: 'saved', detail: 'artifactFile' };
  } catch (err) {
    const detail = err instanceof Error ? err.message : String(err);
    noteDiagnostic(connectionOutput, `opening artifact ${fileName} failed`, detail);
    void offerDetails('error', connectionOutput, `MemQL: ${fileName} could not be opened.`);
    // `${kind} failed`, never the bare delivery kind, for the reason the
    // construct path states: a caller comparing details must not read a failure
    // as the landing it was aiming for.
    return { outcome: 'noCluster', detail: `${delivery.kind} failed` };
  }
}

/**
 * Opens the local cluster's checkout, having PARKED the request first.
 *
 * PARKED BECAUSE A RESTART IS POSSIBLE; FINISHED INLINE BECAUSE IT IS NOT
 * GUARANTEED. Opening a folder in this window certainly restarts the extension
 * host, so the request has to be in globalState before that call or it dies
 * with the host. Adding a folder is the opposite case and used to be treated as
 * the same one: `updateWorkspaceFolders` only terminates the extensions when
 * the FIRST folder changes, and this adds at the end -- so in a window that
 * already has folders nothing reloads, nothing replays, and the construct never
 * opened at all while the outcome still said `opened`. The parked request then
 * sat live in globalState for the whole TTL, where the next window to activate
 * would consume it.
 *
 * So the add branch takes the request straight back out and lands the construct
 * itself. If a restart DOES happen first, this code never runs and the replay
 * is the one that finishes the job; the two cannot both fire, because the take
 * is exactly-once.
 *
 * A cancelled prompt takes it back out too: a request nobody acted on must not
 * surface in the next window the operator happens to open.
 */
async function openCheckoutFor(
  surface: HandoffSurface,
  request: OpenRequest,
  checkout: string,
  mode: 'thisWindow' | 'ask',
  landInThisWindow: () => Promise<HandoffOutcome>
): Promise<HandoffOutcome> {
  await storePending(surface.context.globalState, request, Date.now());
  if (mode === 'thisWindow') {
    // Nothing is open, so there is nothing to disturb and nothing to ask about.
    // This one really does restart the host; the replay finishes it.
    await openFolderOrUnpark(surface, Uri.file(checkout), false);
    return { outcome: 'opened', detail: 'openCheckout' };
  }
  // MODAL, because the alternatives are mutually exclusive and both rearrange
  // the operator's window. This is the one handoff step that can lose work if
  // it is answered by accident.
  const pick = await window.showInformationMessage(
    `Open the local checkout (${checkout}) to edit this construct?`,
    { modal: true },
    'Open in new window',
    'Add to this workspace'
  );
  if (pick === 'Open in new window') {
    await openFolderOrUnpark(surface, Uri.file(checkout), true);
    return { outcome: 'opened', detail: 'openCheckout' };
  }
  if (pick === 'Add to this workspace') {
    const folder = Uri.file(checkout);
    workspace.updateWorkspaceFolders(workspace.workspaceFolders?.length ?? 0, 0, { uri: folder });
    // updateWorkspaceFolders returns a "was it started" boolean, not a promise:
    // the folder appears when onDidChangeWorkspaceFolders fires. Waiting is
    // what makes the lookup below see it.
    await waitForWorkspaceFolder(folder.fsPath, HANDOFF_ADD_FOLDER_TIMEOUT_MS);
    // Still running, so nothing reloaded and nothing is going to replay this.
    await takePending(surface.context.globalState, Date.now());
    // No branch for "the folder never arrived": the lookup simply does not find
    // the file, and landing on the cluster's own copy is a better answer than
    // an error about a folder the operator can see is missing.
    return await landInThisWindow();
  }
  await takePending(surface.context.globalState, Date.now());
  // Nothing opened, and `refused` is reserved for a link this extension would
  // not act on at all -- a caller must be able to tell the two apart.
  return { outcome: 'notLoaded', detail: 'cancelled' };
}

/**
 * Opens a folder, taking the parked request back out if the command rejects.
 *
 * The request is parked BEFORE the call because a successful open destroys this
 * extension host mid-statement -- there is no "after" to park it in. That makes
 * a FAILED open the leak: nothing reloads, so nothing replays, and a live
 * request sits in profile-wide globalState for the rest of its TTL where an
 * unrelated window's activation will consume it and open a construct nobody
 * asked that window for. The same shape the add-to-workspace branch closes,
 * arriving by the other door.
 *
 * Rethrows: the caller's own catch classifies it, and swallowing it here would
 * report a handoff as `opened` when no folder ever opened.
 */
async function openFolderOrUnpark(surface: HandoffSurface, folder: Uri, forceNewWindow: boolean): Promise<void> {
  try {
    await commands.executeCommand('vscode.openFolder', folder, { forceNewWindow });
  } catch (err) {
    await takePending(surface.context.globalState, Date.now());
    throw err;
  }
}

/**
 * Waits for a folder to be present in the workspace, or for the wait to expire.
 *
 * Resolves either way -- the caller's next step is a filesystem lookup that
 * answers the question properly, so a timeout here is a hint about how long to
 * bother waiting rather than a verdict.
 */
function waitForWorkspaceFolder(fsPath: string, timeoutMs: number): Promise<void> {
  const present = (): boolean => (workspace.workspaceFolders ?? []).some((f) => f.uri.fsPath === fsPath);
  if (present()) return Promise.resolve();
  return new Promise((resolve) => {
    let subscription: { dispose(): void } | undefined;
    const finish = (): void => {
      clearTimeout(timer);
      subscription?.dispose();
      resolve();
    };
    const timer = setTimeout(finish, timeoutMs);
    subscription = workspace.onDidChangeWorkspaceFolders(() => {
      if (present()) finish();
    });
  });
}

/**
 * Which workspace folder holds a catalog path, if any.
 *
 * TWO LAYOUTS, because the catalog's path is relative to the DSL TREE ROOT
 * (`cognition/queries.memql`) while a repository checkout keeps that tree under
 * `dsl/`. Trying only the path as reported is what makes an engine checkout --
 * the case a local cluster's operator is most likely to have open -- look like
 * a machine that does not have the file.
 */
async function findInWorkspace(
  originPath: string
): Promise<{ folder: string; relativePath: string; uri: Uri } | undefined> {
  // NO CANDIDATES MEANS NOT HERE. workspaceCandidates refuses a path that
  // escapes its folder (a `..` segment, an absolute path) by returning none at
  // all, so this is where that refusal becomes an answer rather than a probe.
  const candidates = workspaceCandidates(originPath);
  if (candidates.length === 0) return undefined;
  for (const folder of workspace.workspaceFolders ?? []) {
    for (const candidate of candidates) {
      const uri = Uri.joinPath(folder.uri, candidate);
      try {
        await workspace.fs.stat(uri);
        return { folder: folder.uri.fsPath, relativePath: candidate, uri };
      } catch {
        // Not in this folder under this layout; try the next.
      }
    }
  }
  return undefined;
}

/**
 * Waits for the manager to settle after a select, or says why it did not.
 *
 * `memql.clusters.select` awaits its own dial, so the state is USUALLY already
 * final when it returns -- which is why the current state is consulted before
 * anything subscribes. The wait is for the case it is not: a connect superseded
 * by another, or a dial still in flight behind a sign-in prompt.
 *
 * `disconnected` counts as settled. It is not a state a dial recovers from on
 * its own, so waiting out the timeout on one would only make a fast failure
 * slow.
 */
function awaitConnection(
  manager: ConnectionManager,
  clusterName: string,
  timeoutMs: number
): Promise<ConnectionState> {
  const settled = (s: ConnectionState): boolean =>
    s.status === 'disconnected' ||
    ((s.status === 'connected' || s.status === 'error') && s.clusterName === clusterName);
  if (settled(manager.state)) return Promise.resolve(manager.state);
  return new Promise((resolve) => {
    let unsubscribe: (() => void) | undefined;
    const finish = (state: ConnectionState): void => {
      clearTimeout(timer);
      unsubscribe?.();
      resolve(state);
    };
    const timer = setTimeout(() => finish(manager.state), timeoutMs);
    unsubscribe = manager.onDidChangeState((state) => {
      if (settled(state)) finish(state);
    });
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
  //
  // Built HERE, above the ConnectionManager, because the manager needs it too:
  // it is the seam a REFUSED credential is cleared through (memql#3529), and
  // clearing is the same write signing out performs. Every other consumer
  // (sign-in, sign-out, rename, the secret sweep) takes the same object, which
  // is the point -- one place decides how a credential is stored, so there is
  // one place it can be removed from.
  const storeDeps = {
    secrets: context.secrets,
    writeCluster: (update: ClusterUpdate) => upsertCluster(clustersPath, update),
  };

  // The credential resolver (memql#3383 / memql#3385). This is the only place
  // the three things it needs actually exist:
  //
  //   - context.secrets  -- VS Code's SecretStorage, where the LONG-LIVED
  //     refresh token is kept. clusters.yaml is plaintext and owned by the
  //     MemQL Cockpit, so the 30-day credential must not live there; the
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
    }),
    // The reactive half (memql#3529): when the cluster REFUSES a bearer we
    // believed was good, the manager refreshes once, retries once, and -- if
    // that is refused too -- clears the credential through this seam so the
    // next action starts a clean sign-in instead of replaying a dead one.
    storeDeps,
    // THE CONTEXT KEYS' ONE WRITER (memql#4424). The manager decides both
    // values -- it is the only object that knows when either changes -- and
    // this closure is the `vscode` half it is not allowed to import. Every
    // `viewsWelcome` and every Deployments title-menu entry is keyed on what
    // goes through here, so a second writer anywhere would be a race whose
    // winner depends on listener registration order.
    //
    // NOT AWAITED, and there is nothing to await it for: `setContext` is
    // fire-and-forget by design, and the manager publishes state to its
    // listeners in the same tick, so the keys and the repaint reach the
    // workbench together.
    (keys) => {
      void commands.executeCommand('setContext', CLUSTER_SELECTED_KEY, keys.clusterSelected);
      void commands.executeCommand('setContext', CONNECTED_KEY, keys.connected);
    }
  );

  // THE ONE PLACE a dial failure's raw text is recorded (memql#4194). Every
  // surface that mentions the failure afterwards -- tree tooltip, toast,
  // Connection page -- shows the brief form and points here, so shortening
  // them loses nothing.
  connections.onDidChangeState((state) => {
    if (state.status === 'error') {
      noteDiagnostic(
        connectionOutput,
        `dial to "${state.clusterName}" failed (${state.reason})`,
        state.message
      );
    }
  });

  // The version machinery (epic memql#3989). Constructed here, driven by the
  // tree: nothing below does any work until getChildren asks it to, which is
  // what keeps activation offline.
  //
  // ONE refresher and the ONE shared release cache. Both are shared on purpose
  // -- the cache's single-flight is only meaningful across callers, and the
  // refresher holds the per-cluster supersession guards that stop two
  // overlapping refreshes of the same cluster from racing each other onto disk.
  const clusterVersions = new ClusterVersionRefresher({
    collect: createVersionCollector({
      // Lazy: only the local-cluster status source needs it, and resolving it
      // at activation would make startup depend on a path nothing has asked for.
      repoRoot: () => resolveInstallRoot(context.extensionPath),
      readReceipt: () => readReceipt(defaultReceiptPath()).catch(() => null),
      runCapability: runCapabilityScript,
      // The two live sources are closures rather than clients, so the
      // collector module stays free of connection plumbing. Both return
      // nothing unless this editor happens to be connected to the very
      // cluster being refreshed -- asking a live connection about a DIFFERENT
      // cluster would record one cluster's version against another.
      deployStatus: async (cluster) => {
        if (!connectedTo(connections, cluster.name)) return null;
        const status = await deploymentStatusForThisCluster(connections);
        return status === null
          ? null
          : { version: status.version ?? '', engineVersion: status.engineVersion ?? '' };
      },
      liveVersion: async (cluster) => {
        if (!connectedTo(connections, cluster.name)) return '';
        const query = connections?.query;
        if (query === undefined) return '';
        const result = await query.executeNamed('memqlVersion', 'memqlVersion()');
        return readReportedVersion(result.rows());
      },
      // The release the connected engine's binary was CUT FROM, stated on the
      // handshake (memql#3998) and the most trustworthy of the five
      // (memql#4018). Same gate as the two live sources above and for the same
      // reason -- a connection can only answer for the cluster it is connected
      // to -- and no RPC at all: the value arrived with ServerHello.
      helloVersion: async (cluster) => {
        if (!connectedTo(connections, cluster.name)) return '';
        return connections?.engineVersion ?? '';
      },
    }),
    write: (name, version) => upsertCluster(clustersPath, { name, version }),
  });

  const clustersTree = new ClustersTreeProvider(
    clustersPath,
    connections,
    releaseCache,
    clusterVersions
  );
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


  // The Deployments tree (memql#3737). Below Clusters in the container since
  // the clusters-first IA (memql#4195): the extension's mission is managing
  // CLUSTERS -- what you can reach, and installing/repairing the local one --
  // so the clusters list is the home surface, with the machine's deployment
  // detail one view further down.
  //
  // Every collaborator is passed as a THUNK rather than a value: the connection
  // and its query client both change without this view being told, and a
  // captured `undefined` from activation time would leave the connected
  // cluster's history permanently unreadable.
  // Declared before the provider so the provider's `setDescription` callback
  // can reach it, and assigned right after `createTreeView` below. A `let` and
  // not a constructor argument, because the view cannot exist before the
  // provider it renders -- and making the provider hold the view would put an
  // ordering constraint into a class whose whole job is to be orderless.
  let deploymentsView: TreeView<DeploymentNode> | undefined;
  const deploymentsTree = new DeploymentsTreeProvider({
    clustersPath,
    receiptPath: defaultReceiptPath(),
    presence: () => presence.get(),
    // The ONE connection answer, read rather than re-derived (design D1). Note
    // it is the manager's state through the shared mapping, NOT this file's own
    // reading of it: the workbench evaluates the manifest's `when` clauses
    // against the same booleans, and a view that disagreed with them would
    // render an empty tree with no welcome in it.
    connectionContext: () => connectionContextKeys(connections?.state ?? { status: 'disconnected' }),
    // The instance line, promoted out of the wrapper row that used to carry it
    // (memql#4426).
    setDescription: (description) => {
      if (deploymentsView !== undefined) deploymentsView.description = description;
    },
    // What the view title menu's instance actions are scoped by, now that
    // there is no row to scope them with.
    setInstanceContext: (value) => {
      void commands.executeCommand('setContext', DEPLOYMENTS_INSTANCE_KEY, value);
    },
    // The SHARED cache the Clusters tree uses (memql#3996). Single-flight, so
    // both trees open still cost one `git ls-remote`; constructing a second one
    // here would double the work and let the two surfaces disagree about what
    // the newest release is.
    releases: releaseCache,
    connection: () => {
      const state = connections?.state;
      if (state === undefined || state.status === 'disconnected') return undefined;
      return { clusterName: state.clusterName, connected: state.status === 'connected' };
    },
    // Deployment history is ORDINARY CONCEPT ROWS, read through the same
    // browseConceptPage the concept browser uses -- no deploy-control bridge
    // involved (memql#3311 records that decision). Issued as one pair so the
    // records and their per-tier specs describe one instant.
    readDeployments: () => {
      const query = connections?.query;
      if (query === undefined) return undefined;
      return async () => {
        const [deployments, specs] = await Promise.all([
          browseConceptPage(query, DEPLOYMENT_CONCEPT, { pageSize: 200 }),
          browseConceptPage(query, DEPLOYMENT_NODE_SPEC_CONCEPT, { pageSize: 200 }),
        ]);
        return { deployments: deployments.rows, specs: specs.rows };
      };
    },
  });

  /**
   * Everything the instance page needs, built ONCE for every command that opens
   * it (memql#4246).
   *
   * A function rather than a value, because half of it is thunks that must read
   * the LIVE connection at the moment they are called -- and a second literal
   * beside it would be a second answer to what the page is given. That is not
   * hypothetical: the rebuild command below opens the same page, and a page
   * opened with a subtly different `refreshTree` would leave the tree stale
   * after exactly one of the two entry points.
   */
  const deploymentPanelDeps = (): DeploymentPanelDeps => ({
    catalog: {
      clustersPath,
      receiptPath: defaultReceiptPath(),
      presence: () => presence.get(),
    },
    // Same shared cache as the two trees (memql#3996): the page's `latest`
    // fact and the row's availability clause are the same claim, and they
    // must not be able to differ.
    releases: releaseCache,
    // The same two thunks the tree takes, for the same reason: a remote
    // instance's version AND its history are the connected cluster's rows,
    // and the connection changes without this page being told.
    connection: () => {
      const state = connections?.state;
      if (state === undefined || state.status === 'disconnected') return undefined;
      return { clusterName: state.clusterName, connected: state.status === 'connected' };
    },
    readDeployments: () => {
      const query = connections?.query;
      if (query === undefined) return undefined;
      return async () => {
        const [deployments, specs] = await Promise.all([
          browseConceptPage(query, DEPLOYMENT_CONCEPT, { pageSize: 200 }),
          browseConceptPage(query, DEPLOYMENT_NODE_SPEC_CONCEPT, { pageSize: 200 }),
        ]);
        return { deployments: deployments.rows, specs: specs.rows };
      };
    },
    // Rebuilt per call from the LIVE dispatcher rather than cached: the
    // ConnectionManager drops it the moment the socket dies, and a cached
    // client would go on writing into a dead stream.
    deployPort: () => {
      const dispatcher = connections?.dispatcher;
      return dispatcher === undefined ? undefined : new DeployControlClient(dispatcher);
    },
    readRole: async () => {
      const query = connections?.query;
      if (query === undefined) return roleVisibility(undefined);
      const access = await query.getMyAccess().catch(() => null);
      return roleVisibility(access?.clusterRole);
    },
    confirm: (prompt, phrase) =>
      Promise.resolve(
        window.showInputBox({
          title: 'MemQL: confirm',
          prompt,
          placeHolder: phrase,
          ignoreFocusOut: true,
        }),
      ),
    installRoot: installRootFor(context),
    receiptFile: defaultReceiptPath(),
    refreshTree: () => {
      // The presence memo is invalidated too: a deployment that succeeded
      // is one of the two events that change the verdict deterministically,
      // and waiting out the TTL would show the operator the machine as it
      // was before their run.
      presence.invalidate();
      deploymentsTree.refresh();
      clustersTree.refresh();
    },
    // THE RE-PARENTING SEAM. Installing, repairing and uninstalling are the
    // wizard's flows, opened from the instance page rather than reimplemented
    // behind it (design section 5.2: re-parented, not rewritten).
    openInstallFlow: (action) => {
      AddClusterPanel.show(context, presence, addClusterDeps(), action);
    },
  });

  // createTreeView, NOT registerTreeDataProvider (memql#4426). The two register
  // the same provider; only the first hands back the TreeView object, and
  // `TreeView.description` is where the selected cluster's facts now live --
  // the subtitle beside the view's name, which is what let the wrapper row go.
  // The view is disposable and goes into the same subscriptions list the
  // registration used to.
  deploymentsView = window.createTreeView('memqlDeployments', {
    treeDataProvider: deploymentsTree,
  });
  context.subscriptions.push(
    deploymentsView,
    commands.registerCommand('memql.deployments.refresh', () => deploymentsTree.refresh()),
    // Create deployment on a machine with NO local cluster is the install
    // graph, which is the same run the "+" opened -- re-parented, not
    // rewritten. Scoped to the ABSENT selection only (memql#4426: the view
    // title menu's `memql.deploymentsInstance == memqlLocalInstanceAbsent`,
    // where it used to be the absent instance ROW), and offered a second time
    // from the Deployments and Clusters welcomes, which is where an operator
    // with nothing installed actually is. Moving an INSTALLED instance to
    // another tag is a different flow and lands with the instance page (#3739).
    commands.registerCommand('memql.deployments.createDeployment', () => {
      AddClusterPanel.show(context, presence, addClusterDeps(), 'install');
    }),
    // The instance page (memql#3739). It takes the catalog inputs rather than a
    // resolved instance, because the page re-reads the machine every time it is
    // revealed -- a receipt written by a run the page itself started is the
    // ordinary case, and a page holding a snapshot from when it opened would
    // report the version it replaced.
    commands.registerCommand('memql.deployments.open', () => {
      // THE SELECTION, not the local instance (memql#4426). This used to be
      // reached from an instance ROW, which named its own instance; the row is
      // gone and the command is now in the view title menu, where there is
      // nothing to name one. Falling back to the local instance the way the
      // palette entry did would open the wrong page for every operator whose
      // selected cluster is remote -- silently, since both pages look right.
      //
      // "" is passed through when nothing is selected, which the panel reads as
      // the local instance. That is the palette's old behaviour preserved for
      // the palette's old case: the command is not offered in the title menu
      // without `memql.clusterSelected`, so this only happens from the palette.
      DeploymentPanel.show(context, deploymentPanelDeps(), deploymentsTree.selectedInstance()?.name ?? '');
    }),
    // Selecting a deployment opens it (memql#4427). The row carries the run and
    // the instance it belongs to, because the detail page's action buttons are
    // the INSTANCE's role-gated set, contextualised by what this run did --
    // there is no second catalog of run-scoped verbs.
    commands.registerCommand('memql.deployments.openRun', (node?: DeploymentNode) => {
      if (node === undefined || node.kind !== 'run') return;
      DeploymentPanel.showRun(context, deploymentPanelDeps(), node.instance, node.run.id);
    }),
    // "Rebuild Local Cluster From Checkout" (memql#4246). Registered here, in
    // the Deployments surface, because that is where it BELONGS -- but the
    // caller that matters most is the `edited` training lens, which offers it
    // beside a construct whose source no longer matches what the cluster
    // loaded. That lens is the reason the id lives in state/training.ts.
    //
    // It opens the page rather than running: a rebuild takes minutes and
    // changes which images a cluster runs, so it goes through the same
    // checklist an operator reaching it from the instance row sees. A command
    // that started a 45-minute build from a lens click would be a different
    // thing entirely.
    commands.registerCommand(COMMAND_REBUILD, async () => {
      await DeploymentPanel.openAction(context, deploymentPanelDeps(), 'rebuildFromCheckout');
    }),
    // "Open Local Checkout" (memql#4246) -- the ONE place this editor opens
    // the directory the install cloned, shared by the instance row's inline
    // icon, the Connection page and the install wizard's done screen. All
    // three post `openCheckout` and land here rather than each resolving the
    // path and calling `vscode.openFolder` itself.
    commands.registerCommand('memql.deployments.openCheckout', async () => {
      const receipt = await readReceipt(defaultReceiptPath()).catch(() => null);
      const dir = recordedStackDir(receipt);
      // "" is UNKNOWN, never "somewhere else" -- recordedStackDir's own rule,
      // restated at the one call site that turns it into an action rather
      // than a hint. A machine registered by hand, or an install that never
      // reached the clone step, has nothing here to open.
      if (dir === '') {
        void window.showInformationMessage(
          'MemQL: no checkout is recorded for the local cluster. Install or repair it to clone one.'
        );
        return;
      }
      // A NEW WINDOW ONLY WHEN ONE IS ALREADY OPEN. With nothing open, this IS
      // the window the operator is looking at, and forcing a second one would
      // leave this one sitting empty. With something open, replacing it in
      // place would discard whatever they were doing there.
      const hasWorkspace = (workspace.workspaceFolders ?? []).length > 0;
      await commands.executeCommand('vscode.openFolder', Uri.file(dir), {
        forceNewWindow: hasWorkspace,
      });
    }),
  );
  // The connection decides a remote instance's version and its history -- and,
  // since memql#4426, WHICH cluster this view is about and whether it renders
  // at all. A connect or a drop is therefore not merely new content: it is the
  // difference between the timeline and the "Not connected" welcome, so the
  // repaint here is what makes the welcome appear and disappear on time.
  connections.onDidChangeState(() => deploymentsTree.refresh());
  // The Deployments tree reads three files that change underneath it, all in
  // ~/.memql and all outside any workspace -- so each needs a RelativePattern
  // over an existing base directory, for the two reasons the clusters watcher
  // above documents at length.
  //
  //   clusters.yaml       the instance list (the same file, a second reader)
  //   install-receipt.json the local instance's version and domain
  //   runs/               the local run log: one file per run, rewritten per
  //                       step, so a run in flight repaints the tree as it goes
  const runsDir = defaultRunsDir();
  try {
    fs.mkdirSync(runsDir, { recursive: true });
  } catch {
    // Same call as the clusters directory above: a home we cannot write is not
    // worth failing activation over, and the tree still reads what is there.
  }
  // Close out anything a previous host left mid-flight, BEFORE the tree reads
  // the directory (memql#3886). At this instant this host is driving no runs, so
  // every non-terminal record on disk is orphaned by definition -- see
  // reconcileOrphanedRuns for why that is a stronger signal than any liveness
  // check. Fire-and-forget: a home we cannot write is already tolerated above,
  // and the tree refresh at the end repaints whatever was closed.
  void reconcileOrphanedRuns(runsDir, new Date().toISOString())
    .then((closed) => {
      if (closed.length > 0) deploymentsTree.refresh();
    })
    .catch(() => {
      // An unreadable or unwritable run log is not worth failing activation
      // over, for the same reason the mkdir above swallows its error.
    });

  const deploymentsWatchers = [
    workspace.createFileSystemWatcher(
      new RelativePattern(Uri.file(clustersDir), path.basename(clustersPath))
    ),
    workspace.createFileSystemWatcher(
      new RelativePattern(Uri.file(clustersDir), path.basename(defaultReceiptPath()))
    ),
    workspace.createFileSystemWatcher(new RelativePattern(Uri.file(runsDir), '*.json')),
  ];
  for (const w of deploymentsWatchers) {
    w.onDidChange(() => deploymentsTree.refresh());
    w.onDidCreate(() => deploymentsTree.refresh());
    w.onDidDelete(() => deploymentsTree.refresh());
    context.subscriptions.push(w);
  }

  // The Constructs tree (memql#3752): every construct the connected cluster has
  // LOADED, read from the live registries -- so a promoted construct appears
  // the moment it is promoted. A different question from the pack browser's,
  // which answers "show me this file".
  // The read-only marking (memql#3762) rides the SAME catalog fetch. It has to:
  // it classifies a file by the origin the catalog reports, so a second fetch
  // would be a second answer that can disagree with the tree about which files
  // are core.
  const readonlyMarker = new ReadonlyMarker(context.workspaceState);

  const constructsTree = new ConstructsTreeProvider({
    connections,
    connectionContext: () => connectionContextKeys(connections?.state ?? { status: 'disconnected' }),
    load: async (): Promise<CatalogState> => {
      const dispatcher = connections?.dispatcher;
      if (dispatcher === undefined) {
        // NOT AN EMPTY CATALOG. An empty list reads as "this cluster has no
        // constructs", which is the one wrong answer available here.
        //
        // Undefined for the marker too, and for the same reason spelled the
        // other way round: no cluster is not an answer, so nothing is marked
        // rather than everything.
        await readonlyMarker.update(undefined, undefined);
        // REACHED ONLY WITH A CLUSTER SELECTED (memql#4425): the provider
        // returns `[]` before it calls this when nothing is, so the manifest's
        // welcome can render over the empty tree. What is left here is a
        // cluster that WAS chosen and holds no live session -- which is a fact
        // about something, and gets a row rather than a welcome.
        return { kind: 'unreachable' };
      }
      try {
        const result = await new ConstructsClient(dispatcher).listConstructs();
        // The receipt read that answers "which checkout does this cluster
        // rebuild from" happens HERE -- once per catalog fetch, which is also
        // the only moment the marking is recomputed. Not in the decoration
        // provider, which is asked once per file in the explorer and again on
        // every repaint.
        await refreshRecordedCheckout();
        // `currentRunCluster` deliberately, not a second read of clusters.yaml:
        // it is what the run path's write confirmation consults, so "may I edit
        // this file" and "will this write ask first" cannot disagree about
        // whether a cluster is local.
        await readonlyMarker.update(
          result.constructs,
          connections === undefined ? undefined : currentRunCluster(clustersPath, connections)
        );
        return catalogFrom(result.constructs);
      } catch (err) {
        // A cluster predating the message answers with an envelope the client
        // does not recognise, which throws -- rendered as a stated version
        // mismatch naming ListConstructs, never as a blank view.
        //
        // A FAILED FETCH CLEARS THE MARKING as surely as a disconnection does.
        // Keeping the last cluster's answer would leave a developer's checkout
        // marked read-only on the authority of a call that just failed.
        await readonlyMarker.update(undefined, undefined);
        return classifyCatalogFailure(err);
      }
    },
  });
  // Cluster documents (memql#4248): a construct's file, served read-only from
  // the cluster that loaded it, for the ordinary case where the catalog's path
  // names a tree this machine does not have. Every decision is in
  // constructs/clusterDocument.ts; this is where the provider meets the live
  // connection it fetches over.
  const clusterDocuments = new ClusterDocumentProvider({
    connections,
    // The information policy (memql#4194) for a fetch nobody is waiting on: the
    // raw text goes through the redactor into the channel, the buffer gets a
    // notice, and the toast is short and points at the record.
    onError: (headline, detail) => {
      noteDiagnostic(connectionOutput, headline, detail);
      void offerDetails('error', connectionOutput, 'MemQL: a cluster document could not be read from its cluster.');
    },
  });
  // Library artifacts (memql#4748): a file MemQL OS handed over, served
  // read-only from the cluster that holds it. Same split as cluster documents
  // -- every decision in library/artifactDocument.ts, this is where the
  // provider meets the live connection it fetches over.
  const artifactDocuments = new ArtifactDocumentProvider({
    connections,
    // THE REGISTRY RESOLVES THE ADDRESS, not the uri. The document names its
    // cluster by REGISTRY NAME, and clusters.yaml is the one place that says
    // what host that cluster is served from -- read per fetch rather than
    // captured, because an operator may fix a missing domain and reopen the
    // document without restarting the editor.
    apiBaseUrl: async (clusterName) => {
      const result = await readClustersFileSafe(clustersPath);
      const entry = result.ok ? result.file.clusters.find((c) => c.name === clusterName) : undefined;
      return entry === undefined ? undefined : apiBaseUrlFor(entry);
    },
    onError: (headline, detail) => {
      noteDiagnostic(connectionOutput, headline, detail);
      void offerDetails('error', connectionOutput, 'MemQL: a Library artifact could not be read from its cluster.');
    },
  });
  // ONE FACTORY, THREE CALL SITES: they all open the same singleton panel and
  // must hand it the same behaviour. The closure reads `connections` when the
  // BUTTON IS PRESSED rather than when the panel was opened, which is the only
  // reading that can see a connection that has since changed.
  //
  // BUT IT IS NOT THE READING THAT DECIDES (memql#4253). Seeing the current
  // connection is what lets these refuse; it is not permission to USE it. The
  // panel hands back the cluster its record came from, and a mismatch is
  // refused through `panelClusterRefusal` -- the same comparison the
  // cluster-document lens makes, for the same reason. Serving `prod`'s bytes
  // under a record read from `staging`, or opening `prod`'s console for a
  // concept picked out of `staging`'s catalog, is the failure both prevent.
  const constructPanelDeps = (): ConstructPanelDeps => ({
    viewSourceFromCluster: async (construct, panelCluster) => {
      const state = connections?.state;
      const connected = state?.status === 'connected' ? state.clusterName : undefined;
      const refusal = panelClusterRefusal(panelCluster, connected, 'read its source');
      if (refusal !== undefined) {
        void window.showInformationMessage(refusal);
        return;
      }
      // Defined whenever there was no refusal: panelClusterRefusal answers with
      // the connect offer when nothing is connected.
      const clusterName = connected!;
      try {
        await openClusterDocument({
          cluster: clusterName,
          originPath: construct.originPath,
          kind: construct.kind,
          name: construct.name,
        });
      } catch (err) {
        // The information policy (memql#4194): the raw text goes to the
        // channel, never into the document and never into the toast.
        const detail = err instanceof Error ? err.message : String(err);
        noteDiagnostic(connectionOutput, `reading ${construct.originPath} from "${clusterName}" failed`, detail);
        void offerDetails('error', connectionOutput, `MemQL: ${clusterName} could not serve that file.`);
      }
    },
    // Rows live inside the cluster, not on this machine, so this hands off to
    // THE "BROWSE ROWS" ACTION IS GONE (epic memql#4984). It opened the
    // portal's `/concepts/<id>` page for a construct; the portal is retired
    // and MemQL OS has no concept browser, so there is no page to open. The
    // action was REMOVED rather than pointed at the console root -- a menu
    // item that opens a page which does not answer is worse than an absent
    // one, because the person clicks it, gets a 404, and has learnt nothing
    // about where the rows are. `viewSourceFromCluster` above, the other half
    // of memql#4252's division of labour, is unaffected: it opens a construct's
    // SOURCE, which the extension renders itself.
  });

  // Everything the console handoff (memql#4251) needs from this function, in
  // one place, at the point the last of it exists. The handler itself lives at
  // module scope because activate() registers it before this runs -- and, in an
  // untrusted window, instead of it.
  handoffSurface = { context, clustersPath, clustersTree, constructPanelDeps };

  context.subscriptions.push(
    readonlyMarker,
    clusterDocuments,
    artifactDocuments,
    window.registerFileDecorationProvider(readonlyMarker),
    workspace.registerTextDocumentContentProvider(CLUSTER_DOCUMENT_SCHEME, clusterDocuments),
    // A `memql-artifact:` uri with no registered content provider does not fail
    // loudly: openTextDocument rejects with "cannot open", which reads as the
    // DOCUMENT being broken rather than as the registration being absent. Same
    // reasoning as the scheme above, and activation.test.ts pins both.
    workspace.registerTextDocumentContentProvider(ARTIFACT_DOCUMENT_SCHEME, artifactDocuments),
    languages.registerCodeLensProvider(
      { scheme: CLUSTER_DOCUMENT_SCHEME, language: 'memql' },
      new ClusterDocumentLens()
    ),
    window.registerTreeDataProvider('memqlConstructs', constructsTree),
    // BOTH READERS OF THE CLUSTER'S CATALOG, because there are two and only one
    // of them was refreshed here (memql#4246). The tree redraws its rows; the
    // language server holds a SECOND copy, pushed by ClusterCatalogPublisher,
    // which is what every construct's training state is decided against. After
    // a rebuild the cluster has loaded a different tree, so a refresh that
    // moved only the rows would leave the lens saying `edited` about source the
    // cluster now matches -- still offering the rebuild that just ran.
    commands.registerCommand('memql.constructs.refresh', () => {
      constructsTree.refresh();
      void refreshTrainingSurfaces();
    }),
    // Not palette-invokable ("when": "false"): it needs the construct the tree
    // row carries, which the palette cannot supply. Guarded anyway, so a
    // future caller with no argument cannot throw inside the panel.
    commands.registerCommand('memql.constructs.open', (node?: ConstructNode) => {
      if (node?.kind !== 'construct') {
        return;
      }
      // The tree's rows come from the connected cluster's catalog, so that is
      // the cluster this record was read from. "" when nothing is connected --
      // the tree cannot have rows then, but the panel is told the truth rather
      // than a name that happens to be current.
      const state = connections?.state;
      ConstructPanel.open(
        context,
        node.construct,
        constructPanelDeps(),
        state?.status === 'connected' ? state.clusterName : ''
      );
    }),
    // The cluster document's lens posts this: the way BACK from source to the
    // detail page. Palette-hidden for the same reason as `open` -- it takes a
    // {cluster, kind, name} the palette cannot supply. The construct is re-read
    // from the cluster rather than carried in the uri, because the uri holds an
    // address and the page needs the record.
    commands.registerCommand('memql.constructs.showDetails', async (key?: { cluster?: string; kind?: string; name?: string }) => {
      // No argument is unreachable through the lens and is not worth a message:
      // there is nothing to name in one.
      if (key?.kind === undefined || key?.name === undefined) {
        return;
      }
      // THE DOCUMENT'S CLUSTER DECIDES, not the connection in hand. The lens
      // outlives the connection that produced the bytes, so resolving this key
      // against a cluster that merely happens to be connected would render a
      // DIFFERENT cluster's construct of the same name with nothing saying so.
      const state = connections?.state;
      const refusal = detailsRefusal(
        key.cluster ?? '',
        state?.status === 'connected' ? state.clusterName : undefined
      );
      if (refusal !== undefined) {
        void window.showInformationMessage(refusal);
        return;
      }
      const dispatcher = connections?.dispatcher;
      if (dispatcher === undefined) {
        // Reachable when the lens carried no cluster claim to refuse on: a
        // click that does nothing would read as the extension being broken.
        void window.showInformationMessage('MemQL: connect to a cluster to read its constructs.');
        return;
      }
      try {
        const result = await new ConstructsClient(dispatcher).listConstructs();
        const found = result.constructs.find((c) => c.kind === key.kind && c.name === key.name);
        if (found === undefined) {
          void window.showInformationMessage(`MemQL: the cluster has no ${key.kind} ${key.name} loaded.`);
          return;
        }
        // `key.cluster` is the lens's own claim, already checked against the
        // connection by detailsRefusal above -- so it is both the cluster this
        // record came from and a value the panel can be held to.
        ConstructPanel.open(context, toCatalogConstruct(found), constructPanelDeps(), key.cluster ?? '');
      } catch (err) {
        const detail = err instanceof Error ? err.message : String(err);
        noteDiagnostic(connectionOutput, `listing constructs for ${key.kind} ${key.name} failed`, detail);
        void offerDetails('error', connectionOutput, 'MemQL: the cluster could not list its constructs.');
      }
    })
  );

  const conceptsTree = new DataTreeProvider(connections, () =>
    connectionContextKeys(connections?.state ?? { status: 'disconnected' })
  );
  context.subscriptions.push(
    window.registerTreeDataProvider('memqlData', conceptsTree),
    commands.registerCommand('memql.data.refresh', () => conceptsTree.refresh())
  );

  context.subscriptions.push(
    // Not palette-invokable (contributes.menus.commandPalette hides it with
    // "when": "false") since it needs a Concept argument the palette can't
    // supply -- the Concepts tree item's inline command is the only wiring.
    // Guard on `concept` anyway: belt and braces against any future caller
    // (or a manifest edit that forgets the palette exclusion) invoking this
    // with no argument, which would otherwise throw inside ConceptPanel.open
    // on `concept.id`.
    commands.registerCommand('memql.data.open', (concept?: Concept) => {
      if (connections === undefined || concept === undefined) {
        return;
      }
      ConceptPanel.open(context, connections, concept);
    })
  );

  const signInStore: SignInTokenStore = {
    persistSignIn: (clusterName, credentials) =>
      persistSignIn(storeDeps, clusterName, {
        accessToken: credentials.accessToken,
        refreshToken: credentials.refreshToken,
        expiresAtEpochSeconds: credentials.expiresAtEpochSeconds,
        // No client_id travels with a sign-in: it is the operator's own
        // `clientId` override, or the well-known first-party id compiled into
        // both halves (auth/wellKnownClient.ts). "" leaves any stored value
        // alone, which is what keeps an entry from the old registration path
        // working untouched.
        clientId: '',
      }),
    // NO REVOCATION ON THIS PATH, deliberately (memql#4625). This is the
    // SIGN-IN flow's cleanup seam -- what it forgets is a credential a
    // sign-in produced and then could not use, so there is no established
    // server session to end. The user-initiated sign-out is the revoking
    // path, and it is the `memql.clusters.signOut` command below.
    signOut: (clusterName) => signOutCredentials(storeDeps, clusterName).then(() => undefined),
  };

  // sweepOrphanedCredentials deletes SecretStorage entries whose cluster is no
  // longer in clusters.yaml (memql#3515).
  //
  // It is the other half of "SecretStorage cannot be enumerated". #3404 keeps an
  // index precisely so a sweep is possible, and then nothing ever swept: a
  // cluster removed by hand from clusters.yaml, or by the Cockpit, left its
  // refresh token behind for the life of the profile. The extension's own remove
  // path clears credentials directly (removeClusterCompletely), so this is about
  // the edits that did NOT come through it.
  //
  // Safe to call on any read: it only ever deletes secrets for names the file no
  // longer carries. Failures are swallowed -- a keyring that is locked or absent
  // is a normal state, and a housekeeping sweep is not worth a toast.
  const sweepOrphanedCredentials = async (): Promise<void> => {
    try {
      const result = await readClustersFileSafe(clustersPath);
      // A malformed file is NOT an empty one. Sweeping against a parse failure
      // would read "no clusters" and delete every credential on the machine,
      // which is the one way this function could do real damage.
      if (!result.ok) return;
      await reconcileClusterCredentials(
        { secrets: context.secrets },
        result.file.clusters.map((c) => c.name)
      );
    } catch {
      // Deliberately silent; see above.
    }
  };
  void sweepOrphanedCredentials();

  // What the "+" branches on. Held for the life of the surface rather than
  // built per click, because the memo (and the single-flight it wraps) is the
  // whole reason opening the menu twice does not dial twice.
  const presence = new ClusterPresence({ clustersPath });

  // "Forget this cluster", as one call: the entry, the stored credential and
  // the live connection. Lifted out of the memql.clusters.remove handler
  // because the add-a-cluster page needs the same operation after an uninstall
  // -- a cluster that is off the machine must not stay in the list -- and two
  // spellings of it would be two answers to "what does removing a cluster
  // touch?" (memql#3476).
  const removeRegistryEntry = (name: string): Promise<ClusterConfig> => {
    // Only a LIVE connection to this cluster counts; a disconnected state can
    // still name the cluster it was last dialled to.
    const state = connections?.state;
    const connectedClusterName =
      state !== undefined && state.status !== 'disconnected' ? state.clusterName : undefined;
    return removeClusterCompletely(clustersPath, name, {
      secrets: context.secrets,
      connectedClusterName,
      disconnect: () => connections?.disconnect(),
    });
  };

  // What the add-a-cluster page needs from activation. Built per open rather
  // than held, so `installRootFor` is answered against the context that is
  // actually running the extension.
  const addClusterDeps = (): {
    clustersPath: string;
    refreshTree: () => void;
    installRoot: string;
    receiptFile: string;
    removeRegistryEntry: (name: string) => Promise<ClusterConfig>;
    diagnostics: DiagnosticSink;
  } => ({
    clustersPath,
    refreshTree: () => clustersTree.refresh(),
    installRoot: installRootFor(context),
    // The MemQL Install channel (memql#4194): where every run's full,
    // redacted stderr lives.
    diagnostics: sinkFor(installOutput),
    // ONE receipt path for the install that writes it, the uninstall that
    // reverses it and the repair that reads a key path back out of it. The
    // page used to resolve it three times for itself.
    receiptFile: defaultReceiptPath(),
    removeRegistryEntry,
  });

  context.subscriptions.push(
    commands.registerCommand('memql.clusters.refresh', () => clustersTree.refresh()),
    // The escape hatch out of the release listing's ten-minute TTL
    // (memql#3992). An operator who has just cut a release should not have to
    // wait out a timer to see it offered.
    commands.registerCommand('memql.clusters.refreshReleases', () =>
      clustersTree.refreshReleases()
    ),
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
      // THE REGISTRY WINS OVER A CALLER WITH NO ENDPOINT (#3905). This command
      // dials what it is handed, and the install hand-off used to hand it a
      // placeholder built from the cluster's name alone -- so a successful
      // install ended by reporting "not configured. Set an endpoint" about a
      // cluster whose endpoint it had just written, and withheld the "Sign in"
      // button because `notConfigured` is not credential-recoverable.
      //
      // The caller is fixed. This is the wall behind it, in the shape
      // `install/secrets.ts` already argues for: the fix goes where the value is
      // produced, and the wall goes where it is consumed, because the next
      // caller to pass a half-built config has not been written yet. Only an
      // EMPTY endpoint is overridden -- a caller naming a different one is
      // making a deliberate statement and is left alone.
      let dialing = target.cluster;
      if (dialing.endpoint.trim() === '') {
        const registry = await readClustersFileSafe(clustersPath);
        const known = registry.ok
          ? registry.file.clusters.find((c) => c.name === dialing.name)
          : undefined;
        if (known !== undefined && known.endpoint.trim() !== '') dialing = known;
      }
      await connections?.connect(dialing);
      const state = connections?.state;
      // THE END OF A SUCCESSFUL INSTALL COMES THROUGH HERE (memql#3909).
      //
      // The install hand-off selects the cluster it just registered, and this
      // command dials -- so a cluster built thirty seconds ago, whose owner
      // account by design holds no human credential, arrived at the error
      // branch below every single time. It told the operator to hand-edit
      // clusters.yaml with a JWT and offered them Sign in, which cannot work.
      //
      // That is not a failure to report; it is the one step left. Offered as
      // information rather than an error, and wired to the action that
      // actually completes it -- which then chains on to sign-in and the
      // console by itself.
      //
      // MODAL, uniquely among this file's information messages (memql#4078).
      // As a toast this was the first of three stacked notifications after the
      // first fully-green install, and it evaporated before the operator could
      // read it -- decapitating the walk it introduces. VS Code gives no
      // duration control on a toast, but a modal stays until answered, and
      // this is the one popup that can justify one: the single required next
      // step, fired once, at the end of an install the operator just watched.
      // The modal's built-in Cancel is the "later"; nothing else in the walk
      // may be modal, or the walk becomes a wizard that traps.
      //
      // AND DETACHED, never awaited (memql#4079). The dial's caller is
      // sometimes the install panel's hand-off, whose next paint is the done
      // screen carrying the ONE-TIME recovery key reveal. Awaiting the modal
      // here -- and, behind "Set up now", the whole ownership walk -- would
      // hold that paint hostage until the walk ended, burying the only chance
      // there will ever be to show the key behind the popup that introduces
      // the walk. The dial is complete at this point, and both calls below
      // surface their own failures, so answering the caller first loses
      // nothing.
      if (state?.status === 'error' && isFirstCredentialPending(state.reason, await ownershipRouteFor(dialing))) {
        void (async () => {
          const choice = await window.showInformationMessage(
            `MemQL: "${displayLabel(dialing)}" is installed and running.`,
            {
              modal: true,
              detail:
                "One step left: create this cluster's owner passkey. Your browser " +
                'will open -- approve the passkey prompt there, then come back to sign in.',
            },
            'Set up now'
          );
          if (choice === 'Set up now') {
            await commands.executeCommand('memql.clusters.takeOwnership', {
              cluster: dialing,
              selected: true,
            });
          }
        })();
        return;
      }
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
        // `dialing`, not `target.cluster`: the resolved config is the one that
        // was actually connected to, and it is the one carrying the domain
        // `canSignIn` and `signInToCluster` read to find the identity service.
        const offer = signInCanRecover(state.reason) && canSignIn(dialing);
        // Brief; the raw text is already in the Connection channel via the
        // state listener above (memql#4194, audit 45).
        const headline = `MemQL: ${briefMessage(state.message)}`;
        const choice = offer
          ? await offerDetails('error', connectionOutput, headline, 'Sign in')
          : await offerDetails('error', connectionOutput, headline);
        if (choice === 'Sign in') {
          await signInToCluster(dialing, {
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
      // WHICH OF THREE STATES IS THIS CLUSTER IN (memql#3885, memql#3906).
      //
      //   nobody owns it            -> /setup, which mints the first owner
      //   owner exists, nothing
      //     stored here             -> an enrolment link, the first passkey
      //   credential in hand        -> sign in
      //
      // THE ORDER THESE ARE ASKED IN IS THE FIX. #3885 probed `/setup` first
      // and mapped everything else to sign-in, which is right for a cluster
      // this machine knows nothing about and wrong for every cluster this
      // machine INSTALLED: `seedBootstrap` has already created the owner, so
      // `/setup` is sealed (verified: 404), and sign-in cannot work either
      // because that owner holds no human credential. Worse, the probe cannot
      // even reach a local cluster -- Node's `fetch` does not trust the mkcert
      // CA, so it throws `UNABLE_TO_VERIFY_LEAF_SIGNATURE` and every local
      // cluster answers `unknown`.
      //
      // So local evidence -- the receipt's recorded owner, and whether a
      // credential is stored -- is consulted first, and the network only for
      // what it cannot settle. `claim` is then reachable only on a real 200.
      //
      // AND IT RUNS INSIDE A PROGRESS NOTIFICATION (memql#4620). This used to be
      // awaited bare, before anything was drawn. The probe is a network GET, and
      // Node's fetch binds undici's 300-second headers timeout -- so a cluster
      // whose host does not route left `MemQL: Sign In` looking like a command
      // that did nothing at all, with no spinner to say otherwise and no way to
      // give up. There is now both, on top of the five-second deadline
      // `claimProbeSignal` imposes.
      const route = await window.withProgress(
        {
          location: ProgressLocation.Notification,
          title: `MemQL: checking what "${displayLabel(target.cluster)}" needs...`,
          cancellable: true,
        },
        async (_progress, token) => {
          const aborter = new AbortController();
          const subscription = token.onCancellationRequested(() => aborter.abort());
          try {
            const resolved = await ownershipRouteFor(target.cluster, aborter.signal);
            // Cancelling ends the COMMAND, not merely the probe. A cancelled
            // probe answers `unknown`, `unknown` routes to sign-in, and opening
            // a sign-in is not an honest answer to a person who just pressed
            // Cancel on the step before it.
            return token.isCancellationRequested ? undefined : resolved;
          } finally {
            subscription.dispose();
          }
        }
      );
      if (route === undefined) return;

      if (route === 'claim') {
        const wizard = setupUrlForCluster(target.cluster);
        // THE SENTENCE, CORRECTED NOW THAT IT IS REACHABLE (memql#4622). It said
        // "a cluster is claimed by its first sign-in", which is the bootstrap
        // model this product no longer has: an install writes the owner ROW at
        // identity boot, and what mints the first owner on a cluster that has
        // none is the WIZARD. That copy cost nothing while memql#4620 kept this
        // branch unreachable, and the moment the branch works it is the first
        // thing an operator reads. clusters/ownershipRoute.ts carries the model.
        const choice = await window.showInformationMessage(
          `MemQL: "${displayLabel(target.cluster)}" has no owner account, so there is nothing to sign in to. Its ownership wizard is open and mints the first owner -- claim it first.`,
          'Claim this cluster',
          'Sign in anyway'
        );
        if (choice === 'Claim this cluster') {
          // asExternalUri first, for the same reason the enrolment opener
          // does it: under Remote-SSH or Codespaces the wizard runs on the
          // REMOTE host, and the operator's browser is local.
          const external = (await env.asExternalUri(Uri.parse(wizard))).toString(true);
          await env.openExternal(Uri.parse(external));
          return;
        }
        if (choice !== 'Sign in anyway') {
          return;
        }
      }

      // THE STATE A LOCAL INSTALL ACTUALLY LEAVES BEHIND (znasllc-io#3905).
      //
      // The owner account exists and has no passkey and no magic-link identity,
      // so there is nothing to authenticate WITH. An operator reads that as
      // "the extension will not let me in". Both routes are offered rather than
      // one chosen, because this side cannot know whether the operator already
      // registered a passkey on another machine -- they may have -- so it asks.
      // The buttons speak the walk's vocabulary (memql#4078): the action is
      // creating the cluster's owner passkey, not "taking ownership" -- that
      // phrase survives only as the id of the command this button invokes.
      if (route === 'enrol') {
        const choice = await window.showInformationMessage(
          `MemQL: "${displayLabel(target.cluster)}" has an owner but no credential is stored here. ` +
            'Create the owner passkey if this is your first time on this machine; sign in if you already have one.',
          'Create the owner passkey',
          'Sign in'
        );
        if (choice === undefined) return;
        if (choice === 'Create the owner passkey') {
          await commands.executeCommand('memql.clusters.takeOwnership', target);
          return;
        }
      }
      await signInToCluster(target.cluster, {
        clustersPath,
        store: signInStore,
        clustersTree,
      });
    }),
    // Minting the FIRST credential for a cluster's owner, from the editor
    // (znasllc-io#3905).
    //
    // The owner account exists the moment `seedBootstrap` runs and holds nothing
    // a person can sign in with. The install mints an enrolment link, and an
    // operator who dismissed the notification, whose 15-minute link expired, or
    // who installed before that screen offered one had no way back to a link at
    // all -- the only route was a terminal, which is not a route a extension user
    // has.
    //
    // A FRESH LINK EVERY TIME, never a replay: the install's link is single-use
    // and short-lived, and re-opening a spent credential would fail in a way
    // that reads as the feature being broken. It is the same capability the
    // install graph runs, so the two cannot drift.
    commands.registerCommand('memql.clusters.takeOwnership', async (node?: ClusterNode) => {
      const target = node ?? (await pickCluster(clustersPath));
      if (target === undefined || target.cluster.name === '') {
        return;
      }
      // FIRST THING, before anything can fail or be abandoned: this walk owns
      // the cluster's enrolment story for the rest of the session (memql#4078).
      // The walk ends in a sign-in, and every sign-in runs the independent
      // passkey offer (offerPasskeyEnrolment) -- which used to stack a fresh
      // "Enrol a passkey" toast on top of the walk's own notifications, and
      // leave it in the bell to be clicked after the passkey already existed.
      // Suppressed rather than declined, because the operator said nothing.
      passkeyOfferMemory.suppress(target.cluster.name);
      const receipt = await readReceipt(defaultReceiptPath()).catch(() => null);
      const owner = recordedOwner(receipt);
      let url: string;
      try {
        url = await window.withProgress(
          { location: ProgressLocation.Notification, title: 'MemQL: minting an enrolment link...' },
          () =>
            mintOwnershipLink(
              {
                cluster: target.cluster,
                ownerEmail: owner.email,
                receiptDomain: recordedDomain(receipt),
                repoRoot: installRootFor(context),
              },
              runCapabilityScript
            )
        );
      } catch (err) {
        const detail = err instanceof OwnershipError ? err.message : String(err);
        noteDiagnostic(connectionOutput, 'minting an enrolment link failed', detail);
        void offerDetails('error', connectionOutput, `MemQL: ${briefMessage(detail)}`);
        return;
      }
      try {
        // The one place a minted link is validated (https, `/enroll?code=`)
        // before a browser is pointed at it. Every route into ownership -- this
        // command, the install's done screen, the Clusters tree -- arrives here.
        await openEnrolmentLink(url, {
          resolveExternalUri: async (u) => (await env.asExternalUri(Uri.parse(u))).toString(true),
          openExternal: async (u) => await env.openExternal(Uri.parse(u)),
        });
      } catch (err) {
        const detail = err instanceof EnrolmentError ? err.message : String(err);
        noteDiagnostic(connectionOutput, 'opening the enrolment link failed', detail);
        // `browserUnavailable` IS A DESIGNED-RECOVERABLE KIND, AND THIS IS THE
        // CALLER IT WAS DESIGNED FOR (memql#4618). install/enrolment.ts says the
        // kind exists as its own reason "because 'this machine has no browser'
        // is a real, recoverable state -- the caller can fall back to showing
        // the link". This caller did not: it raised an error toast and returned,
        // throwing away a live credential the mint had just produced. On a
        // headless host, a container with no desktop session, or an SSH session
        // with nothing to hand a URL to, that is the whole product of the step
        // discarded because of how it usually travels.
        //
        // ONLY ON THAT REASON. `malformed` means the value is not an https
        // /enroll?code= URL -- precisely the value not to put on somebody's
        // clipboard, since offering to copy it would hand them whatever the mint
        // actually printed and invite them to open it.
        const recoverable = err instanceof EnrolmentError && err.reason === 'browserUnavailable';
        const headline = `MemQL: ${briefMessage(detail)}`;
        // DETACHED, the shape this file uses wherever a toast carries a button
        // (memql#4079): a non-modal notification with an action does not time
        // out, so awaiting one holds the command open until somebody answers it.
        void (async () => {
          const choice = recoverable
            ? await offerDetails('error', connectionOutput, headline, COPY_ENROLMENT_LINK)
            : await offerDetails('error', connectionOutput, headline);
          if (choice === COPY_ENROLMENT_LINK) await copyEnrolmentLink(url);
        })();
        return;
      }
      // THE REST OF THE WALK, offered rather than assumed (memql#3906).
      //
      // Enrolling a passkey is a browser ceremony this side cannot observe --
      // an OS prompt, a security key, a fingerprint -- so there is no event to
      // wait on and no way to know when it finished. What the editor CAN do is
      // leave the next step one click away instead of leaving the operator on a
      // notification that congratulates them and stops.
      //
      // THE ORDER IS FORCED, not chosen. The console authenticates like every
      // other surface, so it cannot be the page that grants the first
      // credential; sign-in cannot work until the passkey exists. Each step is
      // therefore only offered once the one before it can succeed. The sign-in
      // straight after enrolment is WANTED, not ceremony: it is how the
      // operator verifies that the passkey they just approved actually works.
      //
      // ONE VOCABULARY (memql#4078). Three surfaces used to narrate this walk
      // as three different tasks -- "take ownership", "enrol a passkey", "sign
      // in" -- and, stacked, they read as three competing demands. Every step
      // now speaks as one task, finishing setup of the cluster the operator
      // owns; "take ownership" survives only as this command's id and palette
      // title, which are contributions, not copy.
      const enrolled = await window.showInformationMessage(
        `MemQL: approved the passkey prompt in your browser? Sign in to "${displayLabel(target.cluster)}" to finish.`,
        'Sign in'
      );
      if (enrolled !== 'Sign in') return;
      const signedIn = await signInToCluster(target.cluster, {
        clustersPath,
        store: signInStore,
        clustersTree,
      });
      if (!signedIn) return;
      const next = await window.showInformationMessage(
        `MemQL: setup is complete -- you own "${displayLabel(target.cluster)}". MemQL OS is its operations console.`,
        'Open console'
      );
      if (next === 'Open console') {
        await commands.executeCommand('memql.clusters.openConsole', target);
      }
    }),
    // The counterpart: forget this cluster's session. The store owns what that
    // means in each of the two places a credential lives (memql#3404).
    commands.registerCommand('memql.clusters.signOut', async (node?: ClusterNode) => {
      const target = node ?? (await pickCluster(clustersPath));
      if (target === undefined || target.cluster.name === '') {
        return;
      }
      // SIGNING OUT ENDS THE SESSION, NOT JUST THIS EDITOR'S COPY OF IT
      // (memql#4625). Until this, sign-out was purely local: the refresh
      // token stayed live on the cluster for its full thirty days while the
      // toast said "signed out", which is a claim about the session and was
      // only ever true here. The store attempts the revoke BEFORE it clears
      // (it needs the token) and forgets regardless of the answer, so a
      // sign-out on a plane still signs you out.
      const issuer = identityBaseUrlFor(target.cluster);
      let revocation: RevocationOutcome;
      try {
        revocation = await signOutCredentials(storeDeps, target.cluster.name, (refreshToken) =>
          issuer === undefined
            ? Promise.resolve({ attempted: false as const })
            : revokeRefreshToken(issuer, refreshToken, (url, init) => fetch(url, init))
        );
      } catch (err) {
        const detail = err instanceof Error ? err.message : String(err);
        noteDiagnostic(connectionOutput, `signing out of "${target.cluster.name}" failed`, detail);
        void offerDetails(
          'error',
          connectionOutput,
          `MemQL: signing out of "${target.cluster.name}" failed.`
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
      // The wording follows what actually happened. A session this could not
      // end is named as such, with identity's own Devices page as the way to
      // end it -- "signed out" over a live refresh token is the defect.
      if (revocation.attempted && !revocation.revoked) {
        noteDiagnostic(
          connectionOutput,
          `the session for "${target.cluster.name}" was not revoked on the cluster`,
          revocation.reason
        );
        void offerDetails(
          'warning',
          connectionOutput,
          signOutMessage(target.cluster.name, revocation)
        );
      } else {
        window.showInformationMessage(signOutMessage(target.cluster.name, revocation));
      }
    }),
    // The "+" (memql#3412). It used to mean exactly one thing -- register a
    // remote cluster -- for an operator who may have no cluster at all, or one
    // already installed and running. It now branches on EVIDENCE
    // (src/clusters/presence.ts), and registering a remote cluster is one card
    // among them: the "Connect to an existing cluster..." choice.
    commands.registerCommand('memql.clusters.add', () => {
      // The "+" opens a PAGE (memql#3472), not a quick pick. A palette entry is
      // the wrong shape for a decision that depends on the state of the machine
      // and is followed by ten minutes of work -- there is no room in a list of
      // three sentences to say what this machine actually is.
      //
      // THE QUICK PICK IS GONE, not kept beside this (memql#3478). It had one
      // caller, this one, and every branch it offered now belongs to the page:
      // the remote branch to the page's own form (memql#3475), install and
      // repair to the run screen over the callable seam (memql#3474), uninstall
      // to the dry-run preview (memql#3476). Leaving a second route into the
      // same four decisions would be a second wizard over one machine.
      AddClusterPanel.show(context, presence, addClusterDeps());
    }),
    // The irreversible half of the pair D1 keeps apart (memql#3476). It is
    // contributed on `memqlLocalCluster` rows only and never as an inline icon,
    // so it cannot be hit by aiming at the trash can next to it.
    //
    // THE TREE ROW IS NOT AN ARGUMENT. There is exactly one local cluster --
    // the receipt describes one install and presence finds one `local: true`
    // entry -- so the page uninstalls THE local cluster rather than the row
    // that was clicked. From the palette, where no row exists, the behaviour is
    // therefore identical; a machine with nothing installed gets the preview's
    // own refusal, which names the missing receipt.
    commands.registerCommand('memql.clusters.uninstall', () => {
      AddClusterPanel.show(context, presence, addClusterDeps(), 'uninstall');
    }),
    // Repair is the install graph re-run: every step verifies first and skips
    // when already satisfied, so there is no second graph and no second run
    // path -- only different wording. Registered as its own command because the
    // cluster panel's primary control has to have something to invoke
    // (memql#3476, design D5).
    commands.registerCommand('memql.clusters.repair', () => {
      AddClusterPanel.show(context, presence, addClusterDeps(), 'repair');
    }),
    commands.registerCommand('memql.clusters.remove', async (node?: ClusterNode) => {
      const target = node ?? (await pickCluster(clustersPath));
      if (target === undefined || target.cluster.name === '') {
        return;
      }
      const name = target.cluster.name;

      // Modal, and the detail says what is NOT happening. The whole risk in
      // this surface is an operator reading "remove" as "uninstall", so the
      // confirmation spends its second line ruling that out rather than asking
      // a generic "are you sure?".
      //
      // A LOCAL CLUSTER GETS ITS OWN SENTENCE (memql#3742). For a remote one
      // "nothing is uninstalled" is nearly tautological -- this editor could
      // not uninstall it if it tried. For the local one it is the whole point:
      // the k3d cluster, the hosts block and the CA are all still there, and
      // the sentence names where the operator goes to change that. It belongs
      // in the dialog rather than in documentation, because the dialog is
      // where the question is being asked.
      const confirmed = await window.showWarningMessage(
        `Remove "${name}" from the cluster list?`,
        {
          modal: true,
          detail:
            target.cluster.local === true
              ? 'This removes the connection only. The cluster keeps running -- uninstall it ' +
                'from Deployments. This editor forgets it and deletes the credential it stored, ' +
                'and you can connect to it again from the "+" menu without re-typing anything.'
              : 'This editor forgets the cluster and deletes the credential it stored for it. ' +
                'Nothing is uninstalled, and no data on the cluster is touched. ' +
                'You can add it back at any time.',
        },
        'Remove'
      );
      if (confirmed !== 'Remove') {
        return;
      }

      // Only a LIVE connection to this cluster counts. A disconnected state can
      // still name the cluster it was last dialled to, and disconnecting again
      // on the strength of that would be a no-op at best.
      const state = connections?.state;
      const connectedClusterName =
        state !== undefined && state.status !== 'disconnected' ? state.clusterName : undefined;

      try {
        await removeClusterCompletely(clustersPath, name, {
          secrets: context.secrets,
          connectedClusterName,
          disconnect: () => connections?.disconnect(),
        });
      } catch (err) {
        const detail = err instanceof Error ? err.message : String(err);
        noteDiagnostic(connectionOutput, `removing "${name}" failed`, detail);
        void offerDetails('error', connectionOutput, `MemQL: removing "${name}" failed.`);
        return;
      }

      // A removal changes what the "+" should offer -- deterministically, so it
      // must not wait out the memo window (see presence.ts invalidate).
      presence.invalidate();
      clustersTree.refresh();
      window.showInformationMessage(`MemQL: removed "${name}" from the cluster list.`);
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
      // saveClusterEdit rather than upsertCluster alone: the credentials are
      // keyed by cluster NAME, and half of them do not live in the file
      // (memql#3404 puts the refresh token and the expiry in SecretStorage), so
      // a rename that only rewrites the entry strands the thirty-day credential
      // under the old name. memql#3515's second half.
      await writeCluster(clustersTree, () =>
        saveClusterEdit(clustersPath, originalName, edited, { secrets: context.secrets })
      );
    }),
    // The CONNECTION page (memql#3742), which replaces the Cluster tab. What
    // went with that tab was cluster state -- a pod grid, orphan verdicts,
    // under-replica alarms -- which the console owns and already draws. What
    // arrives is the question nothing answered: what this editor dials, as
    // whom, and what happened.
    //
    // Reached from the Clusters tree's inline action, which supplies the node
    // -- and from the palette, which cannot, so it falls back to the selected
    // cluster rather than doing nothing. That fallback is why this command
    // carries the trust clause instead of "when": "false".
    commands.registerCommand('memql.clusters.connection', async (node?: ClusterNode) => {
      if (connections === undefined) {
        return;
      }
      const target = node ?? (await pickCluster(clustersPath));
      if (target === undefined || target.cluster.name === '') {
        return;
      }
      ConnectionPanel.open(
        context,
        {
          clustersPath,
          connections,
          // Straight off SecretStorage through the same store every other
          // credential path uses -- one place decides how a credential is
          // kept, so there is one place this can read it from.
          readExpiry: (name) =>
            new ClusterCredentialStore(context.secrets).readExpiry(name),
        },
        target.cluster.name,
      );
    }),
    // Open Console, as an inline action on the tree as well as a button on the
    // page. The cluster's OWN site row when there is a connection to read it
    // over, and the composed `os.<domain>/` when there is not -- reading the
    // row is what kept this correct through BOTH moves of the console's origin
    // (memql#3711 and epic memql#4984); the composed half needed an edit each
    // time.
    commands.registerCommand('memql.clusters.openConsole', async (node?: ClusterNode) => {
      const target = node ?? (await pickCluster(clustersPath));
      if (target === undefined || target.cluster.name === '') {
        return;
      }
      const url = await consoleUrlForCluster(target.cluster);
      if (url === '') {
        void window.showErrorMessage(
          'MemQL: no console address can be worked out for this cluster. Give it a domain, or connect to it so its site row can be read.'
        );
        return;
      }
      await env.openExternal(Uri.parse(url));
    })
  );

  // The DELIBERATE device-code sign-in (memql#3411). The fallback fires by
  // itself when the loopback flow proves this host cannot do it -- since
  // memql#3515 that is actually true of `MemQL: Sign In`, where it had been
  // documented but unreachable -- but it costs a callback deadline first, so a
  // user who already knows their environment (a container, a hardened network,
  // an SSH session with no browser) can ask for the device code straight away.
  //
  // Same shell as `MemQL: Sign In`, differing only in which grant runs, so this
  // command also refreshes the tree and reconnects the selected cluster. It used
  // to reach a second sign-in implementation that did neither.
  context.subscriptions.push(
    commands.registerCommand('memql.clusters.signInWithCode', async (node?: ClusterNode) => {
      const target = node ?? (await pickCluster(clustersPath));
      if (target === undefined || target.cluster.name === '') {
        return;
      }
      await signInToCluster(
        target.cluster,
        { clustersPath, store: signInStore, clustersTree },
        'deviceCode'
      );
    })
  );

  registerRunSurface(context, clustersPath, connections);

  // A handoff that survived a window reload (memql#4251). Opening the local
  // checkout can restart the extension host, so the request was parked in
  // globalState on the way out and is taken exactly once here -- expired ones
  // are dropped rather than replayed, which is what stops a link clicked an
  // hour ago from opening a file in a window nobody aimed it at.
  //
  // HERE rather than in registerRunSurface, which is scoped to the run
  // affordances and would be the wrong place for it to live even though it is
  // that function that runs last.
  //
  // Replayed as a URI so the ONE handler decides again: this window may have a
  // different workspace, a different connection and a different answer, and a
  // shortcut into the landing step would be a second copy of that decision.
  //
  // COMPOSED BY THE PARSER'S OWN MODULE (`openRequestUri`), not here. The link
  // shape now depends on which target the request is -- `name` or `id` -- and a
  // composer sitting at this call site would be free to fall out of step with
  // the parser it has to satisfy, which surfaces as a replay refusing its own
  // request.
  void takePending(context.globalState, Date.now())
    .then((req) => {
      if (req === undefined) return;
      return handleOpenUri(Uri.parse(openRequestUri(req))).then(() => undefined);
    })
    .catch(noteHandoffFailure);
}

/**
 * Redraws the training surfaces after the CLUSTER's catalog changed.
 *
 * MODULE-LEVEL because two functions of this file drive it. `registerRunSurface`
 * assigns it (it is the only place the publisher, the lens and the decorations
 * all exist) and `registerRuntimeSurface`'s `memql.constructs.refresh` calls it,
 * which is how a REBUILD reaches it (memql#4246): a rebuild replaces the DSL
 * tree a local cluster loaded, so every construct's training state was decided
 * against a tree that is no longer there -- including the `edited` state whose
 * lens offers the rebuild in the first place.
 *
 * No-op until the language client exists, which is the right degradation rather
 * than a gap: without a language server there are no training lenses, so there
 * is no surface a rebuild could leave stale.
 */
let refreshTrainingSurfaces: () => Promise<void> = async () => {};

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
  // Warm the name -> config cache the synchronous cluster() read depends on,
  // and the receipt read the checkout half of a cluster's identity comes from.
  void refreshClusterCache(clustersPath);
  void refreshRecordedCheckout();

  // ---------------------------------------------------------------------------
  // The four training actions (memql#3763)
  // ---------------------------------------------------------------------------

  // A DIAGNOSTIC COLLECTION OF ITS OWN, separate from `memql-run`. Both carry
  // the engine's answer about an assembled bundle, but they assemble DIFFERENT
  // bundles -- a run carries dependencies with unsaved edits, a training action
  // carries the ones the cluster does not have -- so neither can vouch for the
  // other's findings. Sharing one would let a green run clear a dry-run's errors
  // about constructs the run never submitted.
  const trainingDiagnostics = languages.createDiagnosticCollection('memql-training');
  // The structure goes HERE. A classified schema diff -- field, was, now, rows,
  // referencing constructs -- does not fit in a toast, and the toast is where a
  // developer learns something happened. So both: the headline notifies, the
  // channel keeps the record.
  const trainingOutput = window.createOutputChannel('MemQL Training');
  context.subscriptions.push(trainingDiagnostics, trainingOutput);

  // `refreshTrainingSurfaces` is MODULE-LEVEL (see its declaration): the
  // Deployments page has to reach it after a rebuild, and it lives in a
  // different function of this file. Assigned once the language client exists
  // (see the client block below).
  let refreshSessionLens: () => void = () => {};
  // The status bar's click-through. It is registered UNCONDITIONALLY below, so
  // it needs an answer for the window where there is no language server -- and
  // "nothing happens" is the wrong one for a command the palette offers. The
  // default explains instead.
  let showTrainingList: () => Promise<void> = async () => {
    window.showInformationMessage(
      'MemQL: training state needs the MemQL language server, which is not running. Set "memql.lsp.serverPath" or install memql-lsp on your PATH.'
    );
  };

  const training = new TrainingActions({
    cluster: () => currentRunCluster(clustersPath, conns),
    engine: () => buildTrainingEngine(conns),
    assemble: (request, scope) => assembleTraining(request, scope, workspaceRoot),
    confirm: (prompt) => showTrainingModal(prompt),
    // The same modal, and the separation that matters is not its icon: it is
    // that this is a SECOND question, asked after the diff exists, with its own
    // button naming its own consequence. TrainingActions keeps the two channels
    // apart so an ordinary yes can never answer this one.
    confirmOverride: (prompt) => showTrainingModal(prompt),
    publishDiagnostics: (mapped) => publishRunDiagnostics(trainingDiagnostics, mapped),
    display: (p) => displayPath(workspaceRoot, p),
    catalogChanged: () => refreshTrainingSurfaces(),
  });

  context.subscriptions.push(
    // All four are palette-hidden ("when": false): each takes the lens's
    // {uri, name} payload, which the palette cannot supply, and an invocation
    // without one returns rather than guessing at the active editor.
    commands.registerCommand(COMMAND_DRY_RUN, async (request?: TrainingRequest) => {
      if (request === undefined) return;
      reportTraining(await training.dryRun(request), trainingOutput);
    }),
    commands.registerCommand(COMMAND_TRY_IN_SESSION, async (request?: TrainingRequest) => {
      if (request === undefined) return;
      const outcome = await training.tryInSession(request);
      reportTraining(outcome, trainingOutput);
      // The lens that says "defined for this session only" is the half of the
      // temporariness message that survives the modal being dismissed, so it has
      // to redraw now rather than at the next keystroke.
      if (outcome.status === 'ok') refreshSessionLens();
    }),
    commands.registerCommand(COMMAND_STAGE, async (request?: TrainingRequest) => {
      if (request === undefined) return;
      reportTraining(await training.stage(request), trainingOutput);
    }),
    commands.registerCommand(COMMAND_PROMOTE, async (request?: TrainingRequest) => {
      if (request === undefined) return;
      const outcome = await training.promote(request);
      if (outcome.status !== 'breaking') {
        reportTraining(outcome, trainingOutput);
        return;
      }
      // THE OVERRIDE IS A SECOND ACT, and this is where its shape is enforced in
      // the UI. The refusal is written up and REVEALED first, so the classified
      // diff is on screen before anything offers to bypass it; only then does a
      // button appear, and that button opens a modal of its own. Two deliberate
      // clicks with the diff visible between them -- never a retry, never a
      // checkbox that was ticked before the diff existed.
      //
      // Written WITHOUT the usual toast: the channel is revealed outright here,
      // so a "Show details" notification beside the override one would be two
      // notifications competing for the same click.
      const refusal = writeTraining(outcome, trainingOutput);
      trainingOutput.show(true);
      const override = 'Override and promote...';
      const answer = await window.showWarningMessage(
        refusal?.headline ??
          'The engine refused this promote. The classified diff is in the MemQL Training output.',
        override
      );
      if (answer !== override) return;
      reportTraining(
        await training.promoteWithOverride(
          request,
          outcome.cluster,
          outcome.bundle,
          outcome.diffs
        ),
        trainingOutput
      );
    }),
    commands.registerCommand(COMMAND_DEMOTE, async (request?: TrainingRequest) => {
      if (request === undefined) return;
      reportTraining(await training.demote(request), trainingOutput);
    }),
    // The one training command that IS in the palette. It navigates and submits
    // nothing, and it takes no arguments -- the four above take the lens's
    // {uri, name} payload, which the palette cannot supply.
    commands.registerCommand(COMMAND_SHOW_LIST, () => showTrainingList())
  );

  // Every connection-state change ends the stream that held any session-define.
  // Bumping the epoch here is what makes the next run re-inject before
  // honouring itself -- without it a re-run after a reconnect silently
  // executes the DEPLOYED construct and returns a perfectly good wrong answer.
  context.subscriptions.push({
    dispose: conns.onDidChangeState((state) => {
      orchestrator.noteStreamReset();
      // The training half of the same fact: every session-defined construct
      // dies with the stream, silently, and a lens still claiming one after the
      // reconnect would be this editor asserting something false about the
      // cluster. Dropped here, then redrawn so the claim disappears from screen
      // rather than only from memory.
      training.noteStreamReset();
      refreshSessionLens();
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
    const lspBridge = {
      sendRequest: (method: string, params: unknown, token?: CancellationToken) =>
        token === undefined
          ? (client as LanguageClient).sendRequest(method, params)
          : (client as LanguageClient).sendRequest(method, params, token),
      sendNotification: (method: string, params: unknown) =>
        (client as LanguageClient).sendNotification(method, params),
      experimentalCapabilities: () =>
        (client as LanguageClient).initializeResult?.capabilities.experimental as
          | Record<string, unknown>
          | undefined,
    };
    const lensProvider = new RunnableCodeLensProvider(lspBridge);
    context.subscriptions.push(
      languages.registerCodeLensProvider({ language: 'memql' }, lensProvider)
    );

    // The training surface (memql#3761): state label above each signature, a
    // gutter mark, and the untrained/drifted count in the status bar.
    //
    // ALL THREE FEATURE-DETECT on `memqlTrainingState`, so a server that
    // predates memql#3759 answers "no" and this surface is simply ABSENT. That
    // is the correct behaviour rather than a degraded one -- a training UI that
    // rendered `unknown` for everything would say a cluster has none of these
    // constructs, which is the one wrong answer available here.
    //
    // THE THIRD IS THE ONE THAT MAKES THE OTHER TWO SAY ANYTHING. The server
    // holds the parser and no cluster connection, so what a cluster has loaded
    // has to be handed to it; without the publisher every construct answers
    // `unknown` and the gutter and the lens both render nothing at all.
    //
    // It carries ITS OWN catalog fetch, unlike the read-only marker, which
    // rides the Constructs tree's. That marker MUST share a fetch because it
    // writes `files.readonlyInclude` into workspace settings, where two answers
    // disagreeing leaves a developer's checkout locked on the authority of the
    // losing one. This is the opposite case on both counts: the training state
    // is an ephemeral rendering re-asked on every keystroke, and the tree's
    // fetch is LAZY -- it runs from getChildren, so a developer who never opens
    // the Constructs view triggers none.
    //
    // setClient pushes, which is what covers the ordinary startup order: the
    // connection is usually up before the language client finishes
    // initializing, so the state change that would otherwise drive the first
    // push has already happened by the time there is anywhere to push it.
    const clusterCatalog = new ClusterCatalogPublisher(async () => {
      const dispatcher = conns.dispatcher;
      // UNDEFINED, NOT AN EMPTY LIST. `[]` says the cluster has loaded nothing,
      // which decorates every construct in the file as untrained; `undefined`
      // says there is no cluster, which renders nothing. This is the single
      // most consequential line in the wiring.
      if (dispatcher === undefined) return undefined;
      return (await new ConstructsClient(dispatcher).listConstructs()).constructs;
    });
    clusterCatalog.setClient(lspBridge);
    // onDidChangeState hands back a bare unsubscribe closure, not a Disposable.
    const unsubscribeCatalog = conns.onDidChangeState(() => {
      void clusterCatalog.refresh();
    });
    context.subscriptions.push(clusterCatalog, { dispose: unsubscribeCatalog });

    const trainingLens = new TrainingCodeLensProvider();
    trainingLens.setClient(lspBridge);
    // The `edited` lens is locality-aware (memql#4244): Rebuild from checkout on
    // a local cluster, and on a remote one the sentence about rollouts and no
    // button. Pushed on every connection change for the same reason the catalog
    // is -- VS Code re-asks a lens provider when the DOCUMENT changes, and the
    // selected cluster is not part of the document.
    trainingLens.setCluster(currentRunCluster(clustersPath, conns));
    const unsubscribeLensCluster = conns.onDidChangeState(() => {
      trainingLens.setCluster(currentRunCluster(clustersPath, conns));
    });
    context.subscriptions.push({ dispose: unsubscribeLensCluster });
    const trainingDecorations = new TrainingDecorations();
    trainingDecorations.setClient(lspBridge);
    trainingDecorations.activate();
    // The status bar and its list have ONE OWNER, which is the whole reason the
    // handler delegates here rather than fetching for itself: the list has to be
    // the set the count was computed from, and a second reader is a second
    // answer.
    showTrainingList = () => trainingDecorations.showList();
    context.subscriptions.push(
      trainingLens,
      trainingDecorations,
      languages.registerCodeLensProvider({ language: 'memql' }, trainingLens)
    );

    // The lens that keeps "temporary" on screen (memql#3763).
    //
    // A SECOND PROVIDER rather than a fifth state on the first one, because it
    // is not a state: a session-defined construct is still `untrained` on that
    // cluster and still wants a Promote offered beside it. What this adds is a
    // fact about the CONNECTION -- a copy of this source is answering calls
    // right now and will stop without notice -- and the reason it needs saying
    // at all is that a session-define and a promote are indistinguishable from
    // the call site.
    //
    // It asks the server for itself rather than sharing the state lens's reply.
    // VS Code refreshes the two providers independently, so a shared cache would
    // be stale in whichever of them redrew second; and the cost is bounded to
    // the case where something IS defined, because an empty session returns
    // before making a request at all.
    const sessionLensChanged = new EventEmitter<void>();
    context.subscriptions.push(
      sessionLensChanged,
      languages.registerCodeLensProvider(
        { language: 'memql' },
        {
          onDidChangeCodeLenses: sessionLensChanged.event,
          provideCodeLenses: async (document) => {
            const cluster = currentRunCluster(clustersPath, conns);
            if (cluster === undefined) return [];
            if (training.sessionDefinitions.defined(cluster.name).length === 0) return [];
            const constructs = await requestTrainingStates(document.uri.fsPath);
            if (constructs === undefined) return [];
            return sessionLensPlans(constructs, (name) =>
              training.sessionDefinitions.isDefined(cluster.name, name)
            ).map(
              (plan) =>
                new CodeLens(lspRange(plan.signatureRange), {
                  // NO COMMAND. Clicking it would have to do something, and
                  // there is nothing to do -- undefining is not offered, and the
                  // way to make it permanent is the Promote lens on the same
                  // line.
                  title: plan.title,
                  command: '',
                  tooltip: plan.tooltip,
                })
            );
          },
        }
      )
    );
    refreshSessionLens = () => sessionLensChanged.fire();

    // What a promote or a demote has to redraw, in the order it has to redraw
    // it. THE CATALOG FIRST: training state is the server comparing a buffer
    // against the catalog this process pushed, so redrawing a lens before
    // replacing the catalog would repaint it from the answer that is about to
    // change -- which looks exactly like a promote that did nothing.
    refreshTrainingSurfaces = async () => {
      await clusterCatalog.refresh();
      trainingLens.setClient(lspBridge);
      void trainingDecorations.refresh(window.activeTextEditor);
      sessionLensChanged.fire();
    };
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
        window.showErrorMessage('MemQL: run configurations live in the workspace; open a folder first.');
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
      // THE RUNS EXCEPTION, enforced at the moment a cluster is actually needed
      // (memql#4425, design D2). Every other cluster-backed view empties itself
      // when nothing is selected so its welcome can render; this one keeps
      // listing, because `runs.json` is the developer's OWN file and hiding it
      // would present their saved work as lost. So the gate lives here instead,
      // on `memql.connected` rather than `memql.clusterSelected`: a run needs a
      // live session, not merely a chosen cluster, and a cluster that is
      // selected but not answering fails the same way.
      //
      // The sentence is the shared one, so the refusal an operator meets here
      // and the welcomes they read in the sidebar are recognisably one message.
      if (!connectionContextKeys(connections?.state ?? { status: 'disconnected' }).connected) {
        window.showWarningMessage(`MemQL: ${NOT_CONNECTED_REFUSAL}`);
        return;
      }
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
        window.showErrorMessage(
          `MemQL: ${briefMessage(redactForDisplay(err instanceof Error ? err.message : String(err), os.homedir()))}`
        );
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

// currentRunCluster resolves the selected cluster down to the few facts a run
// needs. Deliberately NOT the whole ClusterConfig: the PAT must never travel
// into the orchestrator, and from there into a webview or a log.
//
// The recorded version joined that set in memql#4000. It is safe to carry for
// the same reason the others are -- it is a release tag, not a credential --
// and it is what lets a severed session say the cluster is older than the
// extension instead of only "stream closed".
//
// The recorded CHECKOUT joined it in memql#4244, and it is a fact about THIS
// MACHINE rather than about the cluster: the install receipt records one
// directory, and a caller only learns something from it in combination with
// `local`. That pairing is the whole of the locality rule
// (`constructs/readonly.ts`), which is why the two travel together here rather
// than being resolved separately by each surface that needs them.
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
    // The recorded release, so a severed session can say whether this cluster
    // is older than the extension (memql#4000). Undefined stays undefined -- an
    // unlearned version produces no hint rather than a guess.
    version: cluster.version,
    // Where the installer put the checkout, from the cached receipt read. "" is
    // UNKNOWN, never "somewhere else": a surface that read it as a mismatch
    // would tell every developer their folder is the wrong one.
    checkout: recordedCheckoutDir,
  };
}

// The checkout directory the install receipt records, cached at module scope
// beside the cluster configs above and for the same reason: `currentRunCluster`
// is SYNCHRONOUS (RunDeps.cluster() is on the run path and must not do file IO)
// while reading a receipt is not.
//
// Refreshed where the answer is USED -- the Constructs tree's catalog load,
// which is the one place the read-only marking is recomputed -- and warmed once
// when the run surface comes up so nothing else that asks earlier sees a blank.
let recordedCheckoutDir = '';

async function refreshRecordedCheckout(): Promise<void> {
  const receipt = await readReceipt(defaultReceiptPath()).catch(() => null);
  recordedCheckoutDir = recordedStackDir(receipt);
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
    validateBundle: (sources, origin) => authoring.validateBundle(sources, { origin }),
    sessionDefineBundle: (sources, origin) => authoring.sessionDefineBundle(sources, { origin }),
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
  // A CATALOG TARGET HAS NOTHING TO ASSEMBLE (memql#3753). Its uri names the
  // catalog rather than a file, and for a promoted construct there is no file
  // anywhere on the machine. An empty bundle validates trivially, injects
  // nothing, and leaves the run invoking the definition the cluster already
  // has -- which is exactly what running from a catalog means. It is also what
  // makes `ranDeployedDefinition` true, so the Result view says so.
  if (isCatalogUri(target.uri)) {
    return { sources: '', files: [] };
  }
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

// buildTrainingEngine adapts the live connection to the four calls a training
// action makes. Undefined whenever anything is missing, which TrainingActions
// reports as "not connected" rather than failing mid-action.
//
// NO ROLE CHECK, here or anywhere on this path. Promote and demote are
// owner-only and this process has no trustworthy view of the caller's role; a
// client-side guess would either block a legitimate owner or promise a non-owner
// a call the engine refuses anyway. The engine enforces, the editor explains.
function buildTrainingEngine(conns: ConnectionManager): TrainingEngine | undefined {
  const authoring = conns.authoring;
  if (authoring === undefined) return undefined;
  return {
    validateBundle: (sources, origin) => authoring.validateBundle(sources, { origin }),
    sessionDefineBundle: (sources, origin) => authoring.sessionDefineBundle(sources, { origin }),
    stageBundle: (sources, origin) => authoring.stageBundle(sources, { origin }),
    durablePromoteBundle: (sources, options) => authoring.durablePromoteBundle(sources, options),
    durableDemoteBundle: (sources) => authoring.durableDemoteBundle(sources),
  };
}

// assembleTraining builds what a training action submits: the closure for
// dry-run / try-in-session / promote, the construct alone for demote.
//
// It OPENS every file it needs to classify, which is the one mechanical
// requirement the closure rule has. `memql/trainingState` answers out of the
// server's own copy of a document, so a dependency the editor has never opened
// is a dependency the server cannot describe -- and the closure would then leave
// it out on an assumption rather than on an answer. `openTextDocument` loads a
// document without showing it and the language client forwards the didOpen; it
// is the same mechanic `constructForConfig` already uses to ask about a file
// outside the active editor.
async function assembleTraining(
  request: TrainingRequest,
  scope: TrainingScope,
  workspaceRoot: string | undefined
): Promise<TrainingBundle | undefined> {
  const uri = Uri.parse(request.uri);
  const activePath = uri.fsPath;
  const document = await workspace.openTextDocument(uri);
  const activeText = document.getText();

  if (scope === 'construct') {
    const constructs = await requestTrainingStates(activePath);
    if (constructs === undefined) {
      // NOT the same as "no such construct". The server declining to describe
      // the file means the construct cannot be isolated from its neighbours, and
      // a demote built from a guess would withdraw the wrong thing.
      throw new Error(
        'The language server could not describe this file, so the construct to demote cannot be isolated from the rest of it.'
      );
    }
    return assembleConstruct(activePath, activeText, constructs, request.name);
  }

  const ws: TrainingWorkspace = {
    resolveImport: (dotted) => resolveImportPath(dotted, workspaceRoot, activePath),
    read: (p) => readTrainingSource(p),
    imports: (p, text) => requestImports(p, text),
    trainingStates: (p) => requestTrainingStates(p),
  };
  return assembleClosure(activePath, activeText, ws);
}

// requestTrainingStates asks the server what state each construct in a file is
// in. `undefined` is "could not say" and is kept distinct from `[]` all the way
// down -- see closure.ts, where the two get different decisions.
//
// FEATURE-DETECTED, like every other custom request: an older memql-lsp on the
// PATH is an ordinary situation. Note that a server without the capability also
// renders no training lens, so in practice this path is unreachable from a click
// -- the detection is here because the alternative is a MethodNotFound surfacing
// as a raw command-error toast.
async function requestTrainingStates(filePath: string): Promise<TrainingConstruct[] | undefined> {
  if (client === undefined) return undefined;
  const experimental = client.initializeResult?.capabilities.experimental as
    | Record<string, unknown>
    | undefined;
  if (experimental === undefined || experimental[TRAINING_STATE_CAPABILITY] !== true) {
    return undefined;
  }
  let document;
  try {
    document = await workspace.openTextDocument(Uri.file(filePath));
  } catch {
    return undefined;
  }
  try {
    const raw = await client.sendRequest(TRAINING_STATE_METHOD, {
      textDocument: { uri: document.uri.toString() },
    });
    return parseTrainingConstructs(raw);
  } catch {
    return undefined;
  }
}

// readTrainingSource is readSource with the DIRTY FLAG DROPPED, deliberately and
// visibly. The run bundle includes a dependency because it has unsaved edits;
// the training closure includes one because the cluster does not have it, and
// those two rules disagree for the ordinary case of a saved, never-promoted
// file. Handing the closure a field it must not consult would leave the
// separation to discipline; not handing it over at all settles it.
function readTrainingSource(p: string): { text: string } | undefined {
  const source = readSource(p);
  return source === undefined ? undefined : { text: source.text };
}

// displayPath renders an absolute path for a confirmation modal. Relative to the
// workspace where it can be -- a closure listing eight absolute paths is a
// closure nobody reads.
function displayPath(workspaceRoot: string | undefined, absolute: string): string {
  if (workspaceRoot === undefined) return absolute;
  const relative = path.relative(workspaceRoot, absolute);
  if (relative === '' || relative.startsWith('..') || path.isAbsolute(relative)) return absolute;
  return relative.split(path.sep).join('/');
}

// showTrainingModal asks a training confirmation.
//
// A MODAL, for the run path's reason and more so: a dismissable toast would let
// a promote proceed while the developer's attention is elsewhere, and a promote
// is persisted, shared and replayed on restart. The button carries the prompt's
// own label -- it names the act rather than saying "OK", which is what makes the
// dialog readable at a glance.
async function showTrainingModal(prompt: TrainingPrompt): Promise<boolean> {
  const answer = await window.showWarningMessage(
    prompt.message,
    { modal: true, detail: prompt.detail },
    prompt.confirmLabel
  );
  return answer === prompt.confirmLabel;
}

// reportTraining writes an outcome to both surfaces: the channel keeps the
// structure, the notification says something happened.
//
// `superseded` and `declined` report NOTHING and that is not an oversight --
// outcomeReport returns undefined for both. A superseded action was overtaken by
// a newer one whose report belongs on screen instead, and telling somebody their
// Cancel worked is noise.
function reportTraining(outcome: TrainingOutcome, output: OutputChannel): void {
  const report = writeTraining(outcome, output);
  if (report === undefined) return;

  const details = 'Show details';
  const shown =
    report.severity === 'error'
      ? window.showErrorMessage(report.headline, details)
      : report.severity === 'warning'
        ? window.showWarningMessage(report.headline, details)
        : window.showInformationMessage(report.headline, details);
  void Promise.resolve(shown).then((answer) => {
    // preserveFocus: revealing the record must not take the cursor out of the
    // file the developer is working in.
    if (answer === details) output.show(true);
  });
}

// writeTraining puts an outcome in the record and returns what it wrote, or
// undefined when there was nothing to say. Split out so the breaking-refusal
// path can reveal the channel itself instead of offering a second notification
// beside the one carrying the override.
function writeTraining(
  outcome: TrainingOutcome,
  output: OutputChannel
): ReturnType<typeof outcomeReport> {
  const report = outcomeReport(outcome);
  if (report === undefined) return undefined;
  output.appendLine(`[${new Date().toISOString()}] ${report.headline}`);
  output.appendLine(report.body);
  output.appendLine('');
  return report;
}

// lspRange converts the server's 0-based line/character range to the editor's.
function lspRange(range: {
  start: { line: number; character: number };
  end: { line: number; character: number };
}): Range {
  return new Range(
    new Position(range.start.line, range.start.character),
    new Position(range.end.line, range.end.character)
  );
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
      `cannot read ${maskHomePath(p, os.homedir())}: ${err instanceof Error ? err.message : String(err)} -- open the file and try again`
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
      `MemQL: the run configuration "${config.name}" names no file, so there is no buffer to run. Add a "file" pointing at the .memql file that declares ${config.construct}.`
    );
    return undefined;
  }
  const uri = Uri.file(path.join(workspaceRoot, config.file.split('/').join(path.sep)));
  let document;
  try {
    document = await workspace.openTextDocument(uri);
  } catch (err) {
    window.showErrorMessage(
      `MemQL: cannot open ${config.file}: ${briefMessage(redactForDisplay(err instanceof Error ? err.message : String(err), os.homedir()))}`
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
      `MemQL: the language server could not describe ${config.file}: ${err instanceof Error ? err.message : String(err)}`
    );
    return undefined;
  }
  const found = parseRunnableConstructs(raw).find(
    (c) => c.name === config.construct && c.kind === config.kind
  );
  if (found === undefined) {
    window.showErrorMessage(
      `MemQL: ${config.file} declares no ${config.kind} named ${config.construct}. The construct was renamed, or the file does not currently parse.`
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
    window.showErrorMessage(`MemQL: ${err instanceof Error ? err.message : String(err)}`);
    return;
  }
  const concept = list.find((c) => c.id === conceptId);
  if (concept === undefined) {
    window.showWarningMessage(
      `MemQL: ${conceptId} is not registered on the connected cluster, so row ${rowId} has no Concepts view.`
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
    window.showErrorMessage(`MemQL: ${err instanceof Error ? err.message : String(err)}`);
    return;
  }
  clustersTree.refresh();
}

// The console's URL for a cluster: the site row when there is a live
// connection to read it over, the composed address otherwise (see
// consoleTarget). It was factored out of memql.clusters.openPortal (memql#4252)
// so the construct page's "Browse rows" button could compute the same answer
// instead of growing a second copy of the connected-vs-composed choice; that
// button is gone (epic memql#4984) and this stays factored, because the one
// caller left is the command and a helper with one caller is not a cost.
async function consoleUrlForCluster(cluster: ClusterConfig): Promise<string> {
  const query = connections?.query;
  const state = connections?.state;
  const connected =
    query !== undefined && state?.status === 'connected' && state.clusterName === cluster.name;
  const page = connected
    ? await browseConceptPage(query, SITE_CONCEPT, { pageSize: 50 }).catch(() => null)
    : null;
  return consoleTarget(cluster, page?.rows ?? []).url;
}

async function pickCluster(clustersPath: string): Promise<ClusterNode | undefined> {
  // readClustersFileSafe, not readClustersFile: the Clusters TREE already
  // renders a malformed file as a readable row, and this path must agree with
  // it. The throwing variant turned "Select Cluster" from the palette into a
  // raw command-error toast for a file the tree was calmly explaining.
  const result = await readClustersFileSafe(clustersPath);
  if (!result.ok) {
    window.showErrorMessage(`MemQL: ${result.error}`);
    return undefined;
  }
  const file = result.file;
  const picked = await window.showQuickPick(
    file.clusters.map((cluster) => ({
      label: cluster.name,
      // The row policy (memql#4194, audit 42): state + version, never the
      // address. Same composer as the tree, so the two lists cannot disagree.
      description: clusterRowText(
        cluster,
        connections?.state ?? { status: 'disconnected' },
        releaseCache.peek()
      ).description,
      cluster,
    })),
    { placeHolder: 'Select a MemQL cluster' }
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

/**
 * What this cluster needs next: enrol a first credential, claim it, or sign in.
 *
 * ONE COMPUTATION, TWO CALLERS (memql#3909). `memql.clusters.signIn` asks it to
 * decide what to offer, and `memql.clusters.select` asks it to decide whether a
 * failed dial is a fault or an unfinished install. Two copies would answer the
 * same question differently on the day one of them was edited, and the
 * disagreement would be an operator told to set up a passkey by one surface and
 * to edit clusters.yaml by the other.
 *
 * The receipt is read at the moment of use, not cached: an install or repair
 * can rewrite it between two clicks, and the owner it names is the value the
 * mint will carry.
 *
 * `cancel` is the progress notification's cancellation token, bridged. It bounds
 * only the PROBE -- the receipt read is local and cannot hang -- and a cancelled
 * probe answers `unknown` like any other unreachable one, so cancelling costs
 * the claim branch and nothing else.
 */
async function ownershipRouteFor(
  cluster: ClusterConfig,
  cancel?: AbortSignal
): Promise<OwnershipRoute> {
  const receipt = await readReceipt(defaultReceiptPath()).catch(() => null);
  return resolveOwnershipRoute(
    {
      local: cluster.local === true,
      // The receipt is one file and the cluster list is many, so its owner is
      // evidence only about the cluster whose domain it names.
      ownerRecorded:
        recordedOwner(receipt).email !== '' &&
        !receiptNamesAnotherCluster(cluster.domain ?? '', recordedDomain(receipt)),
      credentialMissing: needsAuth(cluster),
    },
    // RE-DERIVED at the moment of use rather than read from a stored flag: the
    // operator may have claimed the cluster in a browser this extension never
    // saw, and acting on a stale "unclaimed" would send them to a wizard that
    // has since sealed.
    //
    // THE CLUSTER, NOT `cluster.issuer` (memql#4620). This passed the raw field,
    // and NOTHING in this extension ever writes it -- the connect form's
    // registration shape has no such key and the install path omits it on
    // purpose. So the probe was handed `undefined` on every cluster the editor
    // can produce, short-circuited to `unknown`, and `unknown` maps to sign-in:
    // the `claim` branch below was unreachable in the shipped product, and an
    // operator connecting to a genuinely unclaimed remote cluster was sent to
    // authenticate against an account that does not exist. Which field names the
    // identity service is `identityBaseUrlFor`'s decision, and
    // probeClaimStateForCluster is where it is now made -- somewhere a test can
    // reach, which this file is not.
    //
    // AND UNDER A DEADLINE. Node's fetch binds undici's 300-second headers
    // timeout, so an unroutable host used to hang this command for five minutes.
    () =>
      probeClaimStateForCluster(cluster, {
        fetch: globalThis.fetch,
        signal: claimProbeSignal(cancel),
      })
  );
}

// signInToCluster is the editor half of memql#3403's sign-in: progress,
// cancellation, and the two vscode.env capabilities the flow needs injected.
//
// THIS IS THE ONLY SIGN-IN SHELL (memql#3515). There used to be two functions
// with this name -- this one, and an exported one in auth/deviceCodeUi.ts that
// ran the loopback-to-device-code fallback. The exported one had ZERO importers:
// `MemQL: Sign In` reached this one, which ran loopback alone, so a host that
// genuinely could not do loopback (Remote-SSH onto a box whose browser is
// elsewhere, a hardened network) waited out the callback deadline and was then
// told it had failed, with the code to hand it a device code sitting unreachable
// two files away.
//
// The fallback is adopted here rather than deleted, because the capability is
// real and tested (auth/deviceCode.ts, signInWithDeviceCodeFallback). Merging in
// this direction rather than the other keeps the three things a sign-in owes the
// editor and the other shell never did: the tree refresh, the reconnect of the
// SELECTED cluster only, and the kind-based failure levels from auth/signin.ts.
// deviceCodeUi.ts keeps the part that is genuinely about a device code -- putting
// it on screen and keeping it there.
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
// One sign-in flow per cluster at a time (memql#4596). Four surfaces reach
// this -- the dial-failure toast's "Sign in" action, the palette command, the
// ownership walk, "Sign In With a Device Code" -- and nothing used to stop two of them
// running at once: two listeners, two browser tabs or code notifications, two
// progress notifications. A second request now JOINS the in-flight attempt
// and observes its outcome (joining, not cancel-and-restart: the person
// asking again is usually the person mid-way through the first flow's browser
// page, and a `flow` argument on a joined call is deliberately not honoured
// -- one flow at a time is the point). Keyed by cluster name; different
// clusters stay independent.
const signInFlights = new SingleFlight<boolean>();

async function signInToCluster(
  cluster: ClusterConfig,
  deps: SignInDeps,
  flow: SignInFlow = 'auto'
): Promise<boolean> {
  // Said out loud, not silently absorbed. The joiner most worth telling is
  // the one who ran "MemQL: Sign In With a Device Code" while a browser flow
  // waits on its (now ten-minute) callback: their request is deliberately
  // not honoured as a second flow, so without this line the documented
  // recovery gesture would read as a command that does nothing.
  if (signInFlights.has(cluster.name)) {
    void window.showInformationMessage(
      `MemQL: a sign-in to "${cluster.name}" is already in progress -- finish it, or cancel it from its progress notification first.`
    );
  }
  return signInFlights.run(cluster.name, () => runSignInToCluster(cluster, deps, flow));
}

async function runSignInToCluster(
  cluster: ClusterConfig,
  deps: SignInDeps,
  flow: SignInFlow = 'auto'
): Promise<boolean> {
  return window.withProgress(
    {
      location: ProgressLocation.Notification,
      title:
        flow === 'deviceCode'
          ? `MemQL: signing in to ${displayLabel(cluster)} with a device code`
          : `MemQL: signing in to ${displayLabel(cluster)}`,
      cancellable: true,
    },
    async (progress, token) => {
      const aborter = new AbortController();
      const cancelSubscription = token.onCancellationRequested(() => aborter.abort());
      // Read by the code notification at every step, so a flow that finished or
      // was cancelled while the notification sat open does not re-summon it.
      let settled = false;
      // True once a device code owns the progress line; the quiet-wait hint
      // below must never overwrite it.
      let deviceCodeShown = false;
      // True from the moment the auto-fallback fires. Declared here, above
      // the hint timer that reads it: after a bindFailed fallback the line
      // says "switching to a device code..." while POST /device/code runs,
      // and the hint overwriting THAT with browser-flow advice would claim a
      // flow is running that is not. Also read by onUserCode (the action
      // message explains the switch) and the failure path (so an error in the
      // fallback flow still says why a device code was being tried at all).
      let fallbackFired = false;
      // The browser wait can legitimately run for minutes -- the magic-link
      // round trip is enter-email, wait for the mail, click, approve -- and a
      // spinner that says nothing for that long reads as hung (memql#4594,
      // which replaced the old answer: silently switching to a device code
      // under the person's live tab). One update, after a quiet minute,
      // naming both exits.
      const quietHint =
        flow === 'deviceCode'
          ? undefined
          : setTimeout(() => {
              if (settled || deviceCodeShown || fallbackFired) return;
              progress.report({
                message:
                  'still waiting for the browser sign-in -- magic-link emails can take a minute. Cancel and run "MemQL: Sign In With a Device Code" if the page cannot reach this machine.',
              });
            }, 60_000);
      // Both grants report the user code the same way, and the device code is
      // the one thing a person has to READ off the screen and carry to another
      // one -- so it goes on the progress line (undismissable, lives exactly as
      // long as the polling) and into a message with the two actions a
      // progress line cannot render.
      const onUserCode = (authorization: DeviceAuthorization): void => {
        deviceCodeShown = true;
        progress.report({ message: deviceCodeProgressLine(authorization) });
        showDeviceCodeActions(
          authorization,
          () => settled,
          fallbackFired ? 'fallback' : 'deliberate',
          sinkFor(connectionOutput)
        );
      };
      // asExternalUri is not decoration: under Remote-SSH, Codespaces or a dev
      // container the browser runs on a different machine from this extension
      // host, and the loopback URL has to be rewritten into one that machine
      // can reach. toString(true) skips re-encoding -- the authorization URL's
      // query is already percent-encoded and encoding it twice corrupts the
      // PKCE challenge and the state.
      const resolveExternalUri = async (url: string): Promise<string> =>
        (await env.asExternalUri(Uri.parse(url))).toString(true);
      try {
        progress.report({
          message: flow === 'deviceCode' ? 'requesting a device code...' : 'opening your browser...',
        });
        await performSignIn(cluster, {
          signal: aborter.signal,
          store: deps.store,
          // The choice of grant is selectSignInRunner's, in auth/signin.ts,
          // where a test can reach it (memql#3515). This file binds the two
          // runners to vscode; it does not decide between them.
          runFlow: selectSignInRunner(flow, {
            loopbackWithDeviceFallback: (target, signal) =>
              signInWithDeviceCodeFallback(target, {
                signal,
                onUserCode,
                resolveExternalUri,
                openExternal: (url) => env.openExternal(Uri.parse(url)),
                // WHAT MAKES REMOTE-SSH WORK (memql#4623). Undefined locally.
                // A remote extension host cannot receive a loopback callback --
                // the port would be on the wrong machine -- so the flow refuses
                // before binding and this fallback takes the device path, which
                // needs no callback at all. Read here rather than in `flow.ts`,
                // which must stay free of `vscode` imports.
                remoteName: env.remoteName,
                onFallback: (reason) => {
                  fallbackFired = true;
                  announceDeviceCodeFallback(progress, reason, sinkFor(connectionOutput));
                },
              }),
            deviceCode: (target, signal) => runDeviceCodeFlow(target, { signal, onUserCode }),
          }),
        });
      } catch (err) {
        const report = describeSignInFailure(cluster.name, err);
        if (report.level !== 'silent') {
          noteDiagnostic(connectionOutput, `sign-in to "${cluster.name}" failed`, report.message);
        }
        // A failure INSIDE the fallback flow says why a device code was
        // being tried at all. The switch's own toast is gone (memql#4595 --
        // the explanation rides the code message), and the code message only
        // exists once /device/code succeeds; without this suffix, a person
        // who asked for a browser sign-in reads an error about a device flow
        // they never requested.
        const shown =
          fallbackFired && report.level !== 'silent'
            ? `${briefMessage(report.message)} (A browser sign-in was not possible on this host, so a device code was tried; the reason is in the MemQL Connection output.)`
            : briefMessage(report.message);
        // WHAT `retryable` WAS COMPUTED FOR (memql#4621). describeSignInFailure
        // returns it and documents it -- "A UI may offer a retry affordance on
        // true; it must not on false" -- and this call site read `level` and
        // `message` and discarded it, raising every failure toast with NO
        // actions. So after the browser flow ran out its ten-minute deadline the
        // operator read a sentence naming `MemQL: Sign In With a Device Code`
        // and had to open the palette and type it: a command name in prose,
        // where a button was already justified by a field two files away.
        //
        // WHICH buttons is signInRecoveryActions' decision, in a module a test
        // can reach. This binds each returned label to a command and branches on
        // nothing else -- the kinds are the contract, and a second opinion about
        // them here is how the two come to disagree.
        const actions = signInRecoveryActions({
          retryable: report.retryable,
          kind: isAuthFlowError(err) ? err.kind : undefined,
          flow,
        });
        if (report.level !== 'silent') {
          const severity = report.level;
          // STILL NOT AWAITED, for the reason this function's header gives: the
          // command settles now, and the handler runs afterwards. That ordering
          // is also what makes `Try again` work -- by the time a person clicks,
          // this flow has settled and the single-flight entry has cleared, so
          // the retry is a fresh flow rather than a joiner told one is already
          // in progress.
          void (async () => {
            const choice = await offerDetails(severity, connectionOutput, shown, ...actions);
            if (choice === SIGN_IN_RETRY) {
              await signInToCluster(cluster, deps, flow);
            } else if (choice === SIGN_IN_DEVICE_CODE) {
              await signInToCluster(cluster, deps, 'deviceCode');
            } else if (choice === SIGN_IN_EDIT_CLUSTER) {
              // The edit command reads only `cluster` off the node; `selected`
              // is the tree's own marker and nothing on this path knows it.
              await commands.executeCommand('memql.clusters.edit', { cluster, selected: false });
            }
          })();
        }
        return false;
      } finally {
        // Before the dispose, so a code notification still open when the flow
        // resolves stops re-summoning itself rather than outliving the sign-in.
        settled = true;
        if (quietHint !== undefined) clearTimeout(quietHint);
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
            void offerDetails('error', connectionOutput, `MemQL: ${briefMessage(state.message)}`);
            return true;
          }
        }
      }
      void window.showInformationMessage(`MemQL: signed in to "${cluster.name}".`);
      // The second half of memql#3885's three-state table (memql#3902). Runs
      // here and nowhere earlier because passkey state is only knowable to an
      // AUTHENTICATED caller -- there is deliberately no unauthenticated way to
      // ask whether an account has one, since that is an enumeration oracle.
      //
      // NOT AWAITED. The sign-in is finished and reported; whether the operator
      // also wants a passkey is a separate conversation, and holding the
      // progress notification open across it would make an optional offer look
      // like a step of the sign-in.
      void offerPasskeyEnrolment(cluster);
      return true;
    }
  );
}

/**
 * Offer to enrol a passkey when the operator who just signed in has none.
 *
 * AN OFFER, NOT A GATE, and silent whenever it cannot tell (see
 * decidePasskeyOffer): this fires on the heels of a sign-in the operator asked
 * for and got, so a diagnostic here reports a problem they do not have.
 *
 * NEVER STACKED ON THE OWNERSHIP WALK (memql#4078). This runs after EVERY
 * sign-in -- including the one that verifies the walk's own enrolment. That is
 * how the first fully-green install ended in three notifications at once, and
 * how this one, persisting in the notification bell because a toast with
 * buttons does, came to be clicked AFTER the operator had enrolled through the
 * walk -- minting a fresh link whose browser page could only say a passkey
 * already exists. Two answers: `memql.clusters.takeOwnership` suppresses its
 * cluster in passkeyOfferMemory before doing anything else, so the decision
 * below answers `suppressedByWalk` from memory; and a clicked offer re-reads
 * the passkey count before acting, so a stale click gets a one-line "all set"
 * in the editor instead of a refusal in the browser.
 *
 * The link is MINTED, never replayed. It is single-use and short-lived, so a
 * stored one would fail in a way that reads as the feature being broken -- the
 * same reasoning `memql.clusters.takeOwnership` records for the pre-sign-in
 * path, which mints through the capability script because at that point there
 * is no credential to mint with. Here there is one, so it goes over the
 * authenticated stream instead.
 */
async function offerPasskeyEnrolment(cluster: ClusterConfig): Promise<void> {
  const conns = connections;
  const query = conns?.query;
  const dispatcher = conns?.dispatcher;
  if (query === undefined || dispatcher === undefined) return;

  // No userId argument BY DESIGN: the row set comes from userId==actor.userId,
  // so this cannot be pointed at a stranger's authenticators (memql#3178).
  // Hoisted out of the deps object because the offer consults it TWICE: once
  // for the decision, and once more the moment a click acts on it (below).
  const countOwnPasskeys = async (): Promise<number> => {
    const result = await query.executeNamed('passkeysForSelf', 'passkeysForSelf()');
    return result.rows.length;
  };

  const decision = await decidePasskeyOffer(
    cluster.name,
    {
      whoAmI: async () => {
        const access = await query.getMyAccess();
        return access === null
          ? null
          : { userId: access.userId, clusterRole: String(access.clusterRole ?? '') };
      },
      countOwnPasskeys,
    },
    passkeyOfferMemory
  );
  if (!decision.offer) return;

  const choice = await window.showInformationMessage(
    passkeyOfferMessage(displayLabel(cluster)),
    'Enrol a passkey',
    'Not now'
  );
  if (choice !== 'Enrol a passkey') {
    // Remembered for the session on an explicit decline AND on a dismissal:
    // closing the notification is an answer, and re-asking on the next connect
    // is how a prompt teaches people to dismiss it without reading.
    passkeyOfferMemory.decline(cluster.name);
    return;
  }

  // THE COUNT IS RE-READ THE MOMENT THE CLICK ARRIVES (memql#4078). The offer
  // persists in the notification bell, so the click can be minutes stale --
  // late enough for the operator to have enrolled through the ownership walk
  // in between, which is exactly what happened. Only a CONFIRMED enrolment
  // cancels the mint; a count that cannot be read proceeds, because the
  // operator just asked for this one (the inverted default
  // enrolmentStillNeeded documents). Recorded as a decline so the rest of the
  // session does not re-ask about a credential that now exists.
  let passkeysNow: number;
  try {
    passkeysNow = await countOwnPasskeys();
  } catch {
    passkeysNow = Number.NaN;
  }
  if (!enrolmentStillNeeded(passkeysNow)) {
    passkeyOfferMemory.decline(cluster.name);
    void window.showInformationMessage(passkeyAlreadyEnrolledMessage());
    return;
  }

  try {
    const admin = new IdentityAdminClient(dispatcher);
    const minted = await window.withProgress(
      { location: ProgressLocation.Notification, title: 'MemQL: minting an enrolment link...' },
      () => admin.issueEnrolmentLink(decision.userId)
    );
    // asExternalUri first, for the reason every other opener in this file does
    // it: under Remote-SSH or Codespaces the extension host is a different
    // machine from the operator's browser.
    const external = (await env.asExternalUri(Uri.parse(minted.url))).toString(true);
    await env.openExternal(Uri.parse(external));
  } catch (err) {
    // LOUD here, unlike the decision above: the operator asked for this one, so
    // silence would leave them waiting for a browser tab that is not coming.
    const detail = err instanceof Error ? err.message : String(err);
    noteDiagnostic(connectionOutput, 'minting an enrolment link failed', detail);
    void offerDetails('error', connectionOutput, 'MemQL: could not mint an enrolment link.');
  }
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
    prompt: 'Domain (e.g. memql.localhost). The endpoint is composed as api.<domain>:443.',
    value: existing?.domain ?? '',
    ignoreFocusOut: true,
  });
  if (domain === undefined) {
    return undefined;
  }

  const endpoint = await window.showInputBox({
    prompt: 'gRPC endpoint (host:port)',
    // composeEndpointFromDomain, not a fourth copy of `api.<domain>:443`
    // (memql#3475). It answers "" for a blank domain, which is the same empty
    // box the ternary here used to construct by hand.
    value: existing?.endpoint ?? composeEndpointFromDomain(domain),
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
  // The field is OPTIONAL IN PRACTICE (memql#3403): the extension can mint its
  // own credential through the browser, so leaving this empty and running
  // "MemQL: Sign In" is the ordinary path rather than a dead end.
  //
  // NEVER PREFILLED (memql#4194, audit 7/8). The plan comes from
  // src/clusters/form.ts: the box is empty whatever is stored, the prompt says
  // what IS stored, and an empty answer KEEPS the stored value -- removing a
  // credential is sign-out's job, where the SecretStorage half goes too.
  const tokenPlan = tokenFieldPlan(existing?.token);
  const tokenEntered = await window.showInputBox({
    prompt: tokenPlan.prompt,
    value: tokenPlan.value,
    ignoreFocusOut: true,
    password: true,
  });
  const token = resolveCredentialInput(tokenEntered, existing?.token);
  if (token === undefined) {
    return undefined;
  }

  // The refresh token is collected here as the INGEST path only (memql#3385):
  // the credential resolver takes custody of it on the first exchange, moving
  // it into VS Code's SecretStorage and deleting it from the cockpit-shared
  // plaintext file. Same no-prefill plan as the access token.
  const refreshPlan = refreshTokenFieldPlan(existing?.refreshToken);
  const refreshEntered = await window.showInputBox({
    prompt: refreshPlan.prompt,
    value: refreshPlan.value,
    ignoreFocusOut: true,
    password: true,
  });
  const refreshToken = resolveCredentialInput(refreshEntered, existing?.refreshToken);
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

  // Every collected field is returned, INCLUDING the empty ones. For the
  // non-credential fields an empty string means "the user cleared this input",
  // which upsertCluster turns into a key delete. The two CREDENTIAL fields are
  // different since memql#4194: their boxes are never prefilled, so an empty
  // answer means "keep what is stored" and resolveCredentialInput has already
  // substituted the stored value -- revoking a credential is sign-out's job.
  //
  // `local` is returned as a real boolean for the same reason: false here means
  // "the user chose not-local", which upsertCluster writes as an ABSENT key
  // (the cockpit's `omitempty` would drop a `local: false` anyway).
  return {
    name: name.trim(),
    endpoint: endpoint.trim(),
    domain: domain.trim(),
    token,
    refreshToken,
    local: localChoice.value,
  };
}

export function deactivate(): Thenable<void> | undefined {
  // On a host deactivate/reload without a full process exit, an open
  // ConnectionManager WebSocket would otherwise outlive the extension.
  // Guard on the sources, not on the promises their calls return: comparing a
  // promise with undefined is the js/missing-await trap, and the host only
  // awaits what this function RETURNS.
  if (client === undefined && connections === undefined) {
    return undefined;
  }
  return Promise.all([client?.stop(), connections?.disconnect()]).then(() => undefined);
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

// installRootFor answers what SessionOptions.root must be on THIS installation:
// the staged copy of scripts/ inside a packaged extension, or the repository
// root when the extension is running out of a checkout (memql#3487).
//
// The same stage-at-package-time / resolve-at-run-time shape as the bundled
// memql-lsp binary above, and for the same reason: a .vsix contains only what is
// under the extension directory, so anything the extension needs from elsewhere
// in the repository has to be copied in at package time and found by its own
// path at run time. `context.asAbsolutePath(p)` is `path.join(extensionPath, p)`,
// which is why handing over the extension path loses nothing.
//
// The probe and the fallback live in src/install/root.ts rather than here
// because they are LOGIC: this file may import `vscode` and therefore cannot be
// unit-tested outside an editor, and "does a packaged extension find its scripts"
// is precisely the question that must be answerable by a test.
export function installRootFor(context: ExtensionContext): string {
  return resolveInstallRoot(context.extensionPath);
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

// -----------------------------------------------------------------------------
// Version learning: the two sources that need a live cluster (memql#3993)
// -----------------------------------------------------------------------------

/**
 * Whether this editor's live session is on THIS cluster.
 *
 * Both live sources below are gated on it, and the gate is not a nicety:
 * asking the open connection about a cluster it is not connected to would
 * record one cluster's version against another -- a confidently wrong answer,
 * written to disk, that nothing downstream could detect.
 */
function connectedTo(connections: ConnectionManager | undefined, name: string): boolean {
  const state = connections?.state;
  return state?.status === 'connected' && state.clusterName === name;
}

/**
 * `GetDeploymentStatus` for the cluster this editor is connected to.
 *
 * THE ONE ADAPTER OVER THE ENV-SHAPED DEPLOY SURFACE. Epic memql#3943 is
 * collapsing `deploycontrol` to a single target and deleting this parameter;
 * until it lands the RPC still takes one, and there is no environment to name
 * here -- the question is "what is the cluster I am talking to running", which
 * is exactly the question the post-collapse signature asks with no argument at
 * all. So the empty string is passed, deliberately, and this function is the
 * single line that changes when the parameter goes.
 *
 * No new env-shaped parameter is introduced anywhere for this epic, per #3989.
 *
 * A refusal here is an ORDINARY outcome, not a problem: the RPC is owner/admin
 * gated, so a reader legitimately cannot call it, and the collector treats the
 * failure as "this source had nothing to say".
 */
async function deploymentStatusForThisCluster(
  connections: ConnectionManager | undefined,
): Promise<{ version?: string; engineVersion?: string } | null> {
  const dispatcher = connections?.dispatcher;
  if (dispatcher === undefined) return null;
  // Rebuilt per call from the LIVE dispatcher rather than cached, matching the
  // deploy surface's own reasoning: the ConnectionManager drops it the moment
  // the socket dies, and a cached client would go on writing into a dead stream.
  return await new DeployControlClient(dispatcher).getDeploymentStatus();
}

/**
 * The version out of a `memqlVersion()` result.
 *
 * Defensive about the row shape rather than asserting one: this is a builtin
 * whose projection is not pinned by any contract this extension owns, and a
 * version refresh is opportunistic background work. An unrecognised shape is
 * "nothing learned", never a thrown error in front of an operator.
 */
function readReportedVersion(rows: readonly unknown[]): string {
  const first = rows[0];
  if (typeof first !== 'object' || first === null) return '';
  for (const key of ['version', 'serviceVersion', 'engineVersion', 'value']) {
    const found = (first as Record<string, unknown>)[key];
    if (typeof found === 'string' && found.trim() !== '') return found.trim();
  }
  return '';
}
