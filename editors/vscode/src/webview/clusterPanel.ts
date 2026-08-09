// The `Cluster: <name>` editor tab -- topology, deployment history, and the
// deploy actions (memql#3312).
//
// The split #3301 established holds: the sidebar navigates, the editor area
// holds content. This is the content half of the Clusters tree.
//
// WHAT READS THROUGH WHAT. Topology (`v1:cluster:node`), deployment history
// (`v1:cluster:deployment`) and per-tier specs (`v1:cluster:deploymentNodeSpec`)
// are ORDINARY CONCEPT ROWS and are read through the same browseConceptPage
// the concept browser uses -- no bridge involved, and deliberately so
// (memql#3311 records the decision not to duplicate history through the deploy
// service). Only the ACTIONS, and the per-env deployment status, go through
// the bridged DeployControlService.
//
// WHAT THIS FILE IS NOT ALLOWED TO DECIDE. Every rule the surface renders --
// what an orphan is, which deployment is current, which tier is under-
// replicated, which actions a role may use, whether a confirmation matched --
// lives in src/state/ and src/deploy/, unit-tested under bare `node --test`.
// This file is adapter wiring: the webview lifecycle, the CDC subscription,
// the postMessage boundary, and the native input prompts. That division is
// enforced mechanically (cmd/memql-lsp/vscodeimportrule_test.go); it is also
// what makes the role matrix and the type-to-confirm testable at all.
//
// AND THE GATE IS NEVER THIS FILE. Hiding an action a role cannot use is a
// courtesy. The engine refuses independently, through the same gate the unary
// path runs, and a refusal is surfaced naming the role required -- never
// swallowed into a silent no-op.

import * as vscode from "vscode";
import { randomBytes } from "node:crypto";

import type { Role } from "@znasllc-io/memql-sdk-core/client";
import { browseConceptPage } from "@znasllc-io/memql-sdk-core/client";
import { DeployControlClient } from "@znasllc-io/memql-sdk-core/deploy";
import {
  escapeHtml,
  renderDeploymentHistory,
  renderDetail,
  renderTally,
  renderToHtml,
  renderTopologyGrid,
  viewKitStyles,
} from "@znasllc-io/memql-view-kit";

import type { ConnectionManager } from "../connection/manager.js";
import {
  actionById,
  confirmationMatches,
  confirmationPhrase,
  rolloutRequiresConfirmation,
  tierDescription,
  visibleActions,
  type DeployActionId,
} from "../deploy/actions.js";
import { ClusterPanelState, type DeployEnv } from "../deploy/clusterPanelState.js";
import {
  compositionViews,
  historyViews,
  replicaTallyViews,
  topologySummary,
  topologyViews,
} from "../deploy/clusterView.js";
import {
  previewNextVersion,
  readDeploymentStatus,
  runDeployAction,
  type DeployActionRequest,
  type DeployControlPort,
} from "../deploy/controller.js";
import {
  DEPLOYMENT_CONCEPT,
  DEPLOYMENT_NODE_SPEC_CONCEPT,
} from "../state/deploymentHistory.js";
import { NODE_CONCEPT } from "../state/topology.js";

// A cluster's node count is bounded by its pod count and its deployment
// history by how often it ships; one page of each is the whole picture in
// every realistic case, and paging a topology grid would be stranger than
// capping it.
const PAGE_SIZE = 500;
const NOT_CONNECTED_MESSAGE = "Not connected. Select a cluster in the Clusters view.";
const LIVE_UPDATES_OFFLINE_MESSAGE = "live updates unavailable: not connected";

export class ClusterPanel {
  private static readonly open_ = new Map<string, ClusterPanel>();

  private readonly panel: vscode.WebviewPanel;
  private readonly state = new ClusterPanelState();
  private readonly disposeConnectionListener: () => void;
  private readonly clusterName: string;
  private unsubscribeChanges: (() => void) | undefined;

  // Same two-bag disposal arrangement as ConceptPanel: this panel's own
  // listeners live here and die with the tab, while ONE entry goes onto
  // context.subscriptions so a tab still open at deactivation is torn down.
  // Registering everything on context.subscriptions leaks -- that array is
  // drained once, at deactivation, so every listener from every tab ever
  // opened accumulates for the life of the window.
  private readonly disposables: vscode.Disposable[] = [];
  private readonly contextSubscriptions: { dispose(): unknown }[];
  private readonly contextEntry: vscode.Disposable;
  private disposed = false;

  static open(
    context: vscode.ExtensionContext,
    connections: ConnectionManager,
    clusterName: string,
  ): void {
    const existing = ClusterPanel.open_.get(clusterName);
    if (existing !== undefined) {
      existing.panel.reveal();
      return;
    }
    ClusterPanel.open_.set(clusterName, new ClusterPanel(context, connections, clusterName));
  }

  private constructor(
    context: vscode.ExtensionContext,
    private readonly connections: ConnectionManager,
    clusterName: string,
  ) {
    this.clusterName = clusterName;
    this.contextSubscriptions = context.subscriptions;
    this.panel = vscode.window.createWebviewPanel(
      "memqlCluster",
      `Cluster: ${clusterName}`,
      vscode.ViewColumn.Active,
      // No retainContextWhenHidden, for the reason ConceptPanel gives: every
      // render() replaces webview.html wholesale, so there is no DOM state to
      // preserve -- a revealed tab repaints from ClusterPanelState, which
      // lives in the extension host and survives regardless.
      { enableScripts: true },
    );

    this.contextEntry = new vscode.Disposable(() => this.dispose(false));
    context.subscriptions.push(this.contextEntry);

    // A cluster switch (or a reconnect to the same cluster) must not let a
    // response already in flight against the OLD connection land here.
    // reset() invalidates all four of ClusterPanelState's guards at once.
    //
    // The CDC subscription is tied to the old socket and is torn down on EVERY
    // state change, not just on a transition away from "connected" -- otherwise
    // a cluster switch leaves a subscription registered against a socket this
    // panel no longer reads from while ALSO leaving it silently unsubscribed
    // from the new one.
    this.disposeConnectionListener = this.connections.onDidChangeState((connState) => {
      this.unsubscribeChanges?.();
      this.unsubscribeChanges = undefined;
      if (connState.status === "connected") {
        this.subscribeToChanges();
      } else {
        this.state.setLiveUpdatesDegraded(LIVE_UPDATES_OFFLINE_MESSAGE);
      }
      this.state.reset();
      this.render();
      void this.refresh();
    });

    this.disposables.push(
      this.panel.onDidDispose(() => this.dispose(true)),
      this.panel.webview.onDidReceiveMessage((msg: unknown) => this.onMessage(msg)),
    );

    this.subscribeToChanges();
    this.render();
    void this.refresh();
  }

  // The webview posts plain JSON. It is treated as untrusted input rather than
  // trusting the compile-time annotation: `msg.type` on a null message throws,
  // and an unnarrowed id would reach actionById() as any non-undefined value.
  private onMessage(msg: unknown): void {
    if (msg === null || typeof msg !== "object") return;
    const { type, value } = msg as { type?: unknown; value?: unknown };
    if (type === "reload") {
      this.state.reset();
      this.render();
      void this.refresh();
    } else if (type === "selectDeployment" && typeof value === "string") {
      if (this.state.selectDeployment(value)) this.render();
    } else if (type === "setEnv" && (value === "staging" || value === "prod")) {
      if (this.state.setEnv(value as DeployEnv)) {
        this.render();
        void this.refreshDeployControl();
      }
    } else if (type === "action" && typeof value === "string") {
      void this.runAction(value);
    }
  }

  private dispose(fromPanel: boolean): void {
    if (this.disposed) return;
    this.disposed = true;
    if (fromPanel) {
      // Only on the path that is NOT already inside VS Code's own walk of
      // context.subscriptions -- mutating that array mid-iteration would skip
      // whatever entry slid into the vacated index.
      const at = this.contextSubscriptions.indexOf(this.contextEntry);
      if (at >= 0) this.contextSubscriptions.splice(at, 1);
    }
    this.unsubscribeChanges?.();
    this.unsubscribeChanges = undefined;
    this.disposeConnectionListener();
    for (const d of this.disposables.splice(0)) d.dispose();
    ClusterPanel.open_.delete(this.clusterName);
    this.panel.dispose();
  }

  // Live topology is the point of the grid: a node going degraded, or a deploy
  // landing, must appear without a manual reload. One subscription covers all
  // three concepts because they are read and re-derived together anyway -- a
  // node event changes the orphan set, and a deployment event changes which
  // deployment is current, which changes the orphan set too.
  private subscribeToChanges(): void {
    const subs = this.connections.subscriptions;
    if (subs === undefined) {
      this.state.setLiveUpdatesDegraded(LIVE_UPDATES_OFFLINE_MESSAGE);
      return;
    }
    const unsubscribes: (() => void)[] = [];
    try {
      for (const concept of [DEPLOYMENT_CONCEPT, DEPLOYMENT_NODE_SPEC_CONCEPT, NODE_CONCEPT]) {
        unsubscribes.push(
          subs.subscribeGraph(() => void this.refresh(), {
            concept,
            actions: ["created", "updated", "deleted"],
          }),
        );
      }
      this.unsubscribeChanges = () => {
        for (const u of unsubscribes) u();
      };
      this.state.clearLiveUpdatesDegraded();
    } catch (err) {
      // Roll back whatever DID subscribe before the throw. Leaving one or two
      // of the three registered would mean a panel that reports "live updates
      // unavailable" while still receiving some of them, and a leak on the
      // next teardown that has no handle to release.
      for (const u of unsubscribes) {
        try {
          u();
        } catch {
          // A failed unsubscribe on an already-broken subscription is not
          // worth surfacing; the socket is going away regardless.
        }
      }
      // A PERSISTENT notice, deliberately not routed through state.error:
      // ordinary queries succeeding on the same connection is the common case,
      // and that would clear an error-based notice within moments, leaving the
      // panel looking live when it silently is not.
      this.state.setLiveUpdatesDegraded(
        `live updates unavailable: ${err instanceof Error ? err.message : String(err)}`,
      );
    }
  }

  /** Reload everything: the concept rows, the role, and the deploy-control reads. */
  private async refresh(): Promise<void> {
    const query = this.connections.query;
    if (query === undefined) {
      this.state.setConnectionError(NOT_CONNECTED_MESSAGE);
      this.render();
      return;
    }

    const changed = await this.state.loadData(async () => {
      // Issued together so the three sets describe one instant as closely as
      // the wire allows. Sequential reads would let a deploy land between the
      // node read and the deployment read, and the topology built from that
      // pair would mark every live node an orphan.
      const [nodes, deployments, specs] = await Promise.all([
        browseConceptPage(query, NODE_CONCEPT, { pageSize: PAGE_SIZE }),
        browseConceptPage(query, DEPLOYMENT_CONCEPT, { pageSize: PAGE_SIZE }),
        browseConceptPage(query, DEPLOYMENT_NODE_SPEC_CONCEPT, { pageSize: PAGE_SIZE }),
      ]);
      return { nodes: nodes.rows, deployments: deployments.rows, specs: specs.rows };
    });
    if (changed) this.render();

    // Not awaited together with the row read: the role decides which BUTTONS
    // draw, and a slow or refused deploy-control read must not hold the whole
    // topology off the screen.
    if (await this.state.loadAccess(() => this.readRole())) this.render();
    await this.refreshDeployControl();
  }

  /** The two per-env deploy-control reads (status + next-version preview). */
  private async refreshDeployControl(): Promise<void> {
    const port = this.port();
    if (port === undefined) return;
    const env = this.state.env;
    if (await this.state.loadStatus(() => readDeploymentStatus(port, env))) this.render();
    if (await this.state.loadSuggestion(() => previewNextVersion(port, env))) this.render();
  }

  // The caller's cluster role, or undefined when there is nothing to ask.
  //
  // `getMyAccess()` reports "" for a role the SDK's wire enum cannot name --
  // USER_ROLE_DEVELOPER has no TS mapping (roleFromWire), so a developer is
  // indistinguishable from an unknown here. roleVisibility() treats both as
  // indeterminate for exactly that reason.
  private async readRole(): Promise<Role | undefined> {
    const query = this.connections.query;
    if (query === undefined) return undefined;
    const access = await query.getMyAccess();
    return access === null ? undefined : access.clusterRole;
  }

  // The bridged deploy-control client, rebuilt per call from the LIVE
  // dispatcher rather than cached: ConnectionManager drops `conn` the moment
  // the socket dies, so a cached client would go on writing into a dead
  // stream. Constructing one is free -- it holds nothing but the dispatcher.
  private port(): DeployControlPort | undefined {
    const dispatcher = this.connections.dispatcher;
    if (dispatcher === undefined) return undefined;
    return new DeployControlClient(dispatcher);
  }

  // ---------------------------------------------------------------------------
  // Actions
  // ---------------------------------------------------------------------------

  private async runAction(rawId: string): Promise<void> {
    // Narrowed against a literal list rather than cast: the postMessage
    // channel is untrusted, and an unrecognised id must be dropped here rather
    // than reaching actionById() and throwing.
    const id = DEPLOY_ACTION_IDS.find((known) => known === rawId);
    if (id === undefined) return;
    const port = this.port();
    if (port === undefined) {
      void vscode.window.showErrorMessage(`memQL: ${NOT_CONNECTED_MESSAGE}`);
      return;
    }

    const request = await this.collect(id);
    if (request === undefined) return; // Cancelled, or a precondition told the user why.

    // begin() BEFORE the round-trip: a second action started while this one is
    // in flight supersedes it, so the outcome line shows what was asked for
    // last rather than whatever happened to finish last.
    const token = this.state.beginAction();
    const outcome = await runDeployAction(port, request);
    if (!this.state.settleAction(token, outcome)) return;
    this.render();

    // Also surfaced as a notification. The outcome line in the panel is the
    // record; the toast is what reaches an operator who has already switched
    // tabs -- and a deploy is exactly the thing people start and walk away
    // from.
    if (outcome.kind === "success") {
      void vscode.window.showInformationMessage(`memQL: ${outcome.line}`);
    } else {
      void vscode.window.showErrorMessage(`memQL: ${outcome.line}`);
    }

    // An action that landed changes the deployment records and, shortly, the
    // topology. Re-read rather than waiting on CDC: the writes are async
    // ("accepted and kicked off", not "deployed"), so the first thing to
    // change is the record this panel is already showing.
    await this.refresh();
  }

  /**
   * Gather an action's parameters through native prompts, including the
   * type-to-confirm.
   *
   * Native inputs rather than webview form fields: a QuickPick/InputBox
   * sequence is modal, cannot be missed behind a scrolled pane, and -- for the
   * confirmation specifically -- cannot be pre-filled or auto-submitted by the
   * page. Returns undefined when the operator cancels or a precondition is
   * unmet, having already said which.
   */
  private async collect(id: DeployActionId): Promise<DeployActionRequest | undefined> {
    switch (id) {
      case "cutVersion":
        return this.collectCutVersion();
      case "deploy":
        return this.collectDeploy();
      case "promote":
        return this.collectPromote();
      case "rollback":
        return this.collectRollback();
      case "rolloutAction":
        return this.collectRolloutAction();
      default:
        return undefined;
    }
  }

  private async collectCutVersion(): Promise<DeployActionRequest | undefined> {
    const suggestion = this.state.suggestion;
    // The preview is a convenience, not a precondition: SuggestNextVersion is
    // developer-gated too, so a caller who can cut can normally see it -- but a
    // failed preview must degrade to "type the version", never to a dead button.
    const choices: { label: string; description: string; bump: string; version: string }[] = [];
    if (suggestion !== null) {
      choices.push(
        { label: suggestion.nextPatch, description: "patch", bump: "patch", version: "" },
        { label: suggestion.nextMinor, description: "minor", bump: "minor", version: "" },
        { label: suggestion.nextMajor, description: "major", bump: "major", version: "" },
      );
    }
    choices.push({ label: "Enter a version...", description: "explicit override", bump: "", version: "" });

    const picked = await vscode.window.showQuickPick(choices, {
      placeHolder:
        suggestion === null
          ? `Cut a version for ${this.state.env} (no suggestion available)`
          : `Cut a version for ${this.state.env} (current ${suggestion.currentVersion || "unknown"})`,
      ignoreFocusOut: true,
    });
    if (picked === undefined) return undefined;

    if (picked.bump !== "") {
      return { id: "cutVersion", env: this.state.env, bump: picked.bump, version: "" };
    }
    const typed = await vscode.window.showInputBox({
      prompt: `Version to cut for ${this.state.env}`,
      ignoreFocusOut: true,
      validateInput: (v) => (v.trim() === "" ? "A version is required" : undefined),
    });
    if (typed === undefined) return undefined;
    return { id: "cutVersion", env: this.state.env, bump: "", version: typed.trim() };
  }

  private async collectDeploy(): Promise<DeployActionRequest | undefined> {
    const selected = this.state.selectedDeployment;
    if (selected === undefined) {
      void vscode.window.showErrorMessage(
        "memQL: select the deployment record to ship in the Deployments list first. Cut a version if there is no pending record.",
      );
      return undefined;
    }
    return { id: "deploy", deploymentId: selected.deploymentId };
  }

  private async collectPromote(): Promise<DeployActionRequest | undefined> {
    const version = await vscode.window.showInputBox({
      prompt: "Version to promote from staging to prod (digest copy, no rebuild)",
      value: this.state.status?.version ?? "",
      ignoreFocusOut: true,
      validateInput: (v) => (v.trim() === "" ? "A version is required" : undefined),
    });
    if (version === undefined) return undefined;
    const trimmed = version.trim();
    if (!(await this.confirm("promote", trimmed, `Promoting ${trimmed} to PRODUCTION.`))) {
      return undefined;
    }
    return { id: "promote", version: trimmed };
  }

  private async collectRollback(): Promise<DeployActionRequest | undefined> {
    const selected = this.state.selectedDeployment;
    if (selected === undefined) {
      void vscode.window.showErrorMessage(
        "memQL: select the deployment to roll back to in the Deployments list first.",
      );
      return undefined;
    }
    if (selected.status !== "succeeded") {
      // The RPC redeploys a prior SUCCEEDED deployment's stored digest. A
      // pending or failed record has no digest that ever ran, so this is a
      // precondition worth stating rather than a refusal to decode from the
      // engine's reply.
      void vscode.window.showErrorMessage(
        `memQL: roll back targets a succeeded deployment; ${selected.deploymentId} is ${selected.status || "in an unknown state"}.`,
      );
      return undefined;
    }
    if (
      !(await this.confirm(
        "rollback",
        selected.deploymentId,
        `Rolling back to deployment ${selected.deploymentId} (${selected.version || "no version"}). Owner role required.`,
      ))
    ) {
      return undefined;
    }
    return { id: "rollback", toDeploymentId: selected.deploymentId };
  }

  private async collectRolloutAction(): Promise<DeployActionRequest | undefined> {
    const rollouts = this.state.status?.rollouts ?? [];
    let rollout: string | undefined;
    if (rollouts.length > 0) {
      const picked = await vscode.window.showQuickPick(
        rollouts.map((r) => ({ label: r.name, description: `${r.kind} -- ${r.phase}` })),
        { placeHolder: `Rollout in ${this.state.env}`, ignoreFocusOut: true },
      );
      rollout = picked?.label;
    } else {
      // The rollout list comes from the owner/admin-gated status read, so a
      // caller who cannot read status can still act on a rollout they know the
      // name of. Falling back to a text box rather than to a dead menu keeps
      // the two gates independent, which is what they are.
      rollout = await vscode.window.showInputBox({
        prompt: `Rollout name in ${this.state.env}`,
        ignoreFocusOut: true,
        validateInput: (v) => (v.trim() === "" ? "A rollout name is required" : undefined),
      });
      rollout = rollout?.trim();
    }
    if (rollout === undefined || rollout === "") return undefined;

    const sub = await vscode.window.showQuickPick(
      [
        { label: "promote", description: "advance the rollout to the next step" },
        { label: "abort", description: "abort the rollout (type-to-confirm)" },
      ],
      { placeHolder: `Action for ${rollout}`, ignoreFocusOut: true },
    );
    if (sub === undefined) return undefined;

    if (rolloutRequiresConfirmation(sub.label)) {
      if (!(await this.confirm("rolloutAction", rollout, `Aborting rollout ${rollout} in ${this.state.env}.`))) {
        return undefined;
      }
    }
    return { id: "rolloutAction", env: this.state.env, rollout, subAction: sub.label };
  }

  /**
   * The type-to-confirm step.
   *
   * The operator re-types the action's TARGET -- the version, the deployment
   * id, the rollout name -- not "yes" and not the action's name. A
   * confirmation that can be satisfied without reading the target confirms
   * nothing, which is the whole difference between this and a modal Yes
   * button. The comparison lives in src/deploy/actions.ts so it is testable;
   * this method is the prompt.
   */
  private async confirm(id: DeployActionId, target: string, what: string): Promise<boolean> {
    const expected = confirmationPhrase(id, target);
    if (expected === "") {
      // An action with nothing identifiable to re-type must not proceed
      // unchallenged. Refusing here rather than falling through to "no
      // confirmation needed" is the safe direction.
      void vscode.window.showErrorMessage(
        `memQL: ${actionById(id).label} needs a target to confirm against, and none was resolved.`,
      );
      return false;
    }
    const typed = await vscode.window.showInputBox({
      title: actionById(id).label,
      prompt: `${what} Type "${expected}" to confirm.`,
      ignoreFocusOut: true,
      validateInput: (v) =>
        v.trim() === "" || confirmationMatches(expected, v) ? undefined : `Type "${expected}" exactly.`,
    });
    // An empty submission passes validateInput (so the box does not shout at
    // an operator who has not typed yet) and is rejected here.
    return typed !== undefined && confirmationMatches(expected, typed);
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  private render(): void {
    const nonce = nonceValue();
    const state = this.state;
    const topology = state.topology;

    const gridHtml = renderToHtml(renderTopologyGrid(topologyViews(topology)));
    const tallyHtml = renderToHtml(
      renderTally(replicaTallyViews(topology), "No node types to account for."),
    );
    const historyHtml = renderToHtml(
      renderDeploymentHistory(historyViews(state.deployments, state.currentDeployment), state.selectedDeploymentId),
    );
    const compositionHtml =
      state.selectedDeploymentId === undefined
        ? '<div class="placeholder">Select a deployment to preview its per-tier composition.</div>'
        : renderToHtml(
            renderTally(
              compositionViews(state.composition()),
              "This deployment declares no per-tier spec.",
            ),
          );

    const statusHtml =
      state.status !== null
        ? renderToHtml(renderDetail(state.status as unknown as Record<string, unknown>))
        : state.statusMessage === ""
          ? '<div class="placeholder">Loading deployment status...</div>'
          : `<div class="notice">${escapeHtml(state.statusMessage)}</div>`;

    this.panel.webview.html = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy"
      content="default-src 'none'; style-src 'nonce-${nonce}'; script-src 'nonce-${nonce}';">
<title>${escapeHtml(this.clusterName)}</title>
<style nonce="${nonce}">
  /* Theme view-kit by mapping ITS tokens onto VS Code's -- the entire coupling
     between the two, exactly as the concept panel does it. */
  :root {
    --vk-fg: var(--vscode-foreground);
    --vk-muted-fg: var(--vscode-descriptionForeground);
    --vk-border: var(--vscode-panel-border);
    --vk-hover-bg: var(--vscode-list-hoverBackground);
    --vk-selected-bg: var(--vscode-list-activeSelectionBackground);
    --vk-selected-fg: var(--vscode-list-activeSelectionForeground);
  }

${viewKitStyles}

  /* Health and orphan colouring. view-kit stays value-agnostic and emits
     data-health / data-orphan; deciding that degraded is amber is the HOST's
     job, and this block is where this host decides it. */
  .vk-tile-health[data-health="healthy"] { color: var(--vscode-charts-green); }
  .vk-tile-health[data-health="degraded"] { color: var(--vscode-charts-yellow); }
  .vk-tile-health[data-health="connecting"] { color: var(--vscode-charts-blue); }
  .vk-tile-health[data-health="draining"] { color: var(--vscode-charts-yellow); }
  .vk-tile-health[data-health="offline"] { color: var(--vscode-charts-red); }
  .vk-tile-health[data-health="stopped"] { color: var(--vscode-charts-red); }
  .vk-tile[data-orphan="true"] { border-color: var(--vscode-editorWarning-foreground); }
  .vk-flag { color: var(--vscode-editorWarning-foreground); }
  .vk-row-status[data-status="failed"] { color: var(--vscode-charts-red); }
  .vk-row-status[data-status="succeeded"] { color: var(--vscode-charts-green); }

  /* Page chrome -- the panel's, not view-kit's. */
  body { font-family: var(--vscode-font-family); color: var(--vscode-foreground);
         background: var(--vscode-editor-background); margin: 0; padding: 0; }
  .toolbar { padding: 8px 12px; border-bottom: 1px solid var(--vscode-panel-border);
             display: flex; gap: 12px; align-items: center; flex-wrap: wrap; }
  .layout { display: grid; grid-template-columns: minmax(280px, 55%) 1fr;
            height: calc(100vh - 42px); }
  .pane { overflow: auto; padding: 8px 12px; }
  .pane + .pane { border-left: 1px solid var(--vscode-panel-border); }
  h2 { font-size: 1em; margin: 12px 0 6px; text-transform: uppercase;
       letter-spacing: 0.06em; color: var(--vscode-descriptionForeground); }
  h2:first-child { margin-top: 0; }
  .summary { color: var(--vscode-descriptionForeground); padding-bottom: 6px; }
  .placeholder { color: var(--vscode-descriptionForeground); opacity: 0.6; padding: 8px 0; }
  .error { color: var(--vscode-errorForeground); padding: 8px 12px; }
  .warning { color: var(--vscode-editorWarning-foreground); padding: 8px 12px; }
  .notice { color: var(--vscode-descriptionForeground); padding: 8px 0; }
  .outcome { padding: 8px 12px; border-bottom: 1px solid var(--vscode-panel-border);
             overflow-wrap: anywhere; }
  .outcome-error { color: var(--vscode-errorForeground); }
  .actions { display: flex; gap: 8px; flex-wrap: wrap; padding: 4px 0; }
  button { background: var(--vscode-button-background); color: var(--vscode-button-foreground);
           border: none; padding: 4px 10px; cursor: pointer; border-radius: 2px; }
  button[data-selected="true"] { outline: 1px solid var(--vscode-focusBorder); }
</style>
</head>
<body>
<div class="toolbar">
  <strong>${escapeHtml(this.clusterName)}</strong>
  <span class="summary">${escapeHtml(topologySummary(topology))}</span>
  <button id="env-staging" type="button" data-selected="${state.env === "staging"}">staging</button>
  <button id="env-prod" type="button" data-selected="${state.env === "prod"}">prod</button>
  <button id="reload" type="button">Reload</button>
</div>
${this.outcomeHtml()}
${state.liveUpdatesError === "" ? "" : `<div class="warning">WARNING: ${escapeHtml(state.liveUpdatesError)}</div>`}
${state.error === "" ? "" : `<div class="error">ERROR: ${escapeHtml(state.error)}</div>`}
<div class="layout">
  <div class="pane">
    <h2>Topology</h2>
    ${gridHtml}
    <h2>Replicas</h2>
    ${tallyHtml}
  </div>
  <div class="pane">
    <h2>Deployments</h2>
    <div id="history">${historyHtml}</div>
    <h2>Composition</h2>
    ${compositionHtml}
    <h2>Deploy status (${escapeHtml(state.env)})</h2>
    ${statusHtml}
    <h2>Actions</h2>
    ${this.actionsHtml()}
  </div>
</div>
<script nonce="${nonce}">
  const vscode = acquireVsCodeApi();
  // One delegated listener per region: view-kit emits data attributes, never
  // inline handlers, which is what lets the CSP forbid them outright.
  document.getElementById('history').addEventListener('click', (e) => {
    const row = e.target.closest('[data-row-id]');
    if (row) vscode.postMessage({ type: 'selectDeployment', value: row.dataset.rowId });
  });
  document.getElementById('reload').addEventListener('click', () =>
    vscode.postMessage({ type: 'reload' }));
  document.getElementById('env-staging').addEventListener('click', () =>
    vscode.postMessage({ type: 'setEnv', value: 'staging' }));
  document.getElementById('env-prod').addEventListener('click', () =>
    vscode.postMessage({ type: 'setEnv', value: 'prod' }));
  const actions = document.getElementById('actions');
  if (actions) actions.addEventListener('click', (e) => {
    const button = e.target.closest('[data-action]');
    if (button) vscode.postMessage({ type: 'action', value: button.dataset.action });
  });
</script>
</body>
</html>`;
  }

  private outcomeHtml(): string {
    const outcome = this.state.lastOutcome;
    if (outcome === undefined) return "";
    const cls = outcome.kind === "success" ? "outcome" : "outcome outcome-error";
    return `<div class="${cls}">${escapeHtml(outcome.line)}</div>`;
  }

  /**
   * The action buttons for the caller's role.
   *
   * A resolved writer or reader gets NO buttons and a line saying so -- the
   * acceptance criterion. An INDETERMINATE role gets the buttons plus a
   * notice, because the TS SDK's role enum cannot name `developer` (see
   * roleVisibility), and hiding on that basis would lock a genuine developer
   * out of cut and deploy that the engine would have allowed. Either way the
   * engine is the gate; this is presentation.
   */
  private actionsHtml(): string {
    const visibility = this.state.visibility;
    const actions = visibleActions(visibility);
    if (actions.length === 0) {
      return `<div class="notice">${escapeHtml(
        `Deploy actions require the developer, admin, or owner cluster role. Your role is ${
          visibility.kind === "resolved" && visibility.role !== "" ? visibility.role : "unknown"
        }; topology and deployment history above are unaffected.`,
      )}</div>`;
    }
    const buttons = actions
      .map(
        (action) =>
          `<button type="button" data-action="${escapeHtml(action.id)}" title="${escapeHtml(
            `${action.description} Requires ${tierDescription(action.tier)}.`,
          )}">${escapeHtml(action.label)}</button>`,
      )
      .join("");
    const notice =
      visibility.kind === "indeterminate"
        ? `<div class="notice">${escapeHtml(visibility.reason)}</div>`
        : "";
    return `<div class="actions" id="actions">${buttons}</div>${notice}`;
  }
}

// The ids runAction will accept off the untrusted postMessage channel. A
// literal list rather than a map over DEPLOY_ACTIONS so the narrowing is a
// real type guard rather than a cast.
const DEPLOY_ACTION_IDS: readonly DeployActionId[] = [
  "cutVersion",
  "deploy",
  "promote",
  "rollback",
  "rolloutAction",
];

// A CSP nonce is a security control, so it comes from a CSPRNG. Math.random()
// is not one.
function nonceValue(): string {
  return randomBytes(16).toString("base64");
}
