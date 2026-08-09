// Race-safe state for the Cluster tab.
//
// Four independent async round-trips feed this panel, and each races the world
// moving on beneath it:
//
//   DATA    the three concept-row reads (nodes, deployments, node specs), which
//           together are ONE consistent picture and must land or be discarded
//           together -- a topology built from fresh nodes and a stale
//           deployment list would mark live nodes orphaned on the strength of
//           a deployment that is no longer current.
//   STATUS  the deploy-control GetDeploymentStatus read, which is separately
//           gated (owner/admin, #728) and separately re-issued when the
//           operator flips the env toggle.
//   ACCESS  the caller's cluster role, which decides which actions are drawn.
//   ACTION  a running deploy action, whose outcome must not be painted after
//           the panel has been reset out from under it.
//
// Each gets its OWN Latest guard (src/async/latest.ts -- the shared one; #3304
// exists precisely so this is not a fifth hand-rolled generation counter), and
// the phantom kinds make handing one guard's token to another a compile error
// rather than a silent comparison of unrelated counters. A cluster switch or a
// reconnect calls reset(), which invalidates all four at once, so nothing in
// flight against the old connection can land on the new one's panel.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import type { Row } from "@znasllc-io/memql-sdk-core/client";
import type { Role } from "@znasllc-io/memql-sdk-core/client";
import type { DeploymentStatus, NextVersionSuggestion } from "@znasllc-io/memql-sdk-core/deploy";

import { Latest, type LatestToken } from "../async/latest.js";
import {
  composeDeployment,
  currentDeploymentId,
  projectDeployments,
  projectNodeSpecs,
  type CompositionTier,
  type DeploymentNodeSpec,
  type DeploymentRecord,
} from "../state/deploymentHistory.js";
import { buildTopology, liveNodeTypesFor, type Topology } from "../state/topology.js";
import { roleVisibility, type RoleVisibility } from "./actions.js";
import type { DeployOutcome } from "./controller.js";

/** The environments the deploy console scopes to. */
export type DeployEnv = "staging" | "prod";

/** The three concept-row reads that make one consistent picture. */
export interface ClusterRowSets {
  nodes: Row[];
  deployments: Row[];
  specs: Row[];
}

export type ActionToken = LatestToken<"deployAction">;

const EMPTY_TOPOLOGY: Topology = {
  nodes: [],
  tiers: [],
  liveByTier: new Map(),
  orphanCount: 0,
  underReplicaCount: 0,
};

export class ClusterPanelState {
  private readonly dataLatest = new Latest<"clusterData">();
  private readonly statusLatest = new Latest<"deployStatus">();
  private readonly accessLatest = new Latest<"access">();
  private readonly actionLatest = new Latest<"deployAction">();

  private topologyView: Topology = EMPTY_TOPOLOGY;
  private deploymentRecords: DeploymentRecord[] = [];
  private nodeSpecs: DeploymentNodeSpec[] = [];
  private current = "";
  private selection: string | undefined;

  private deploymentStatus: DeploymentStatus | null = null;
  private statusNotice = "";
  private versionSuggestion: NextVersionSuggestion | null = null;

  private visibilityState: RoleVisibility = roleVisibility(undefined);
  private environment: DeployEnv = "staging";

  private errorMessage = "";
  // Persistent, exactly like ConceptPanelState's: a successful data load must
  // NOT clear "live updates are off". A CDC subscription failing does not stop
  // ordinary queries succeeding on the same healthy connection, so routing
  // this through errorMessage would wipe it moments after showing it and leave
  // the panel looking live when it silently is not.
  private liveUpdatesDegradedMessage = "";
  private outcome: DeployOutcome | undefined;

  get topology(): Topology {
    return this.topologyView;
  }

  get deployments(): DeploymentRecord[] {
    return this.deploymentRecords;
  }

  get specs(): DeploymentNodeSpec[] {
    return this.nodeSpecs;
  }

  get currentDeployment(): string {
    return this.current;
  }

  get selectedDeploymentId(): string | undefined {
    return this.selection;
  }

  get status(): DeploymentStatus | null {
    return this.deploymentStatus;
  }

  get statusMessage(): string {
    return this.statusNotice;
  }

  get suggestion(): NextVersionSuggestion | null {
    return this.versionSuggestion;
  }

  get visibility(): RoleVisibility {
    return this.visibilityState;
  }

  get env(): DeployEnv {
    return this.environment;
  }

  get error(): string {
    return this.errorMessage;
  }

  get liveUpdatesError(): string {
    return this.liveUpdatesDegradedMessage;
  }

  get lastOutcome(): DeployOutcome | undefined {
    return this.outcome;
  }

  /** The selected deployment's record, or undefined when nothing is selected. */
  get selectedDeployment(): DeploymentRecord | undefined {
    if (this.selection === undefined) return undefined;
    return this.deploymentRecords.find((d) => d.deploymentId === this.selection);
  }

  /**
   * The selected deployment's per-tier composition, with leftovers flagged.
   *
   * Computed on read rather than cached: it is derived from three fields that
   * each change on their own schedule (the selection on a click, the specs and
   * the topology on a CDC event), and a cache would have to be invalidated
   * from all three. It is a handful of array passes over a node set bounded by
   * the cluster's pod count.
   */
  composition(): CompositionTier[] {
    const deployment = this.selectedDeployment;
    if (deployment === undefined) return [];
    return composeDeployment(
      deployment,
      this.nodeSpecs,
      liveNodeTypesFor(this.topologyView, deployment.deploymentId),
      deployment.deploymentId === this.current,
    );
  }

  /**
   * Clear everything derived from the connection and supersede every in-flight
   * call. Called on an explicit reload and on EVERY connection state change.
   *
   * Deliberately does NOT touch liveUpdatesDegradedMessage (it survives until a
   * subscribe attempt actually resolves) and does not touch `env` (the
   * operator's env choice is theirs, not the connection's -- silently
   * bouncing it back to staging on a reconnect would be a small betrayal at
   * the exact moment they are watching prod).
   */
  reset(): void {
    this.dataLatest.invalidate();
    this.statusLatest.invalidate();
    this.accessLatest.invalidate();
    this.actionLatest.invalidate();
    this.topologyView = EMPTY_TOPOLOGY;
    this.deploymentRecords = [];
    this.nodeSpecs = [];
    this.current = "";
    this.selection = undefined;
    this.deploymentStatus = null;
    this.statusNotice = "";
    this.versionSuggestion = null;
    this.visibilityState = roleVisibility(undefined);
    this.errorMessage = "";
    this.outcome = undefined;
  }

  setLiveUpdatesDegraded(message: string): void {
    this.liveUpdatesDegradedMessage = message;
  }

  clearLiveUpdatesDegraded(): void {
    this.liveUpdatesDegradedMessage = "";
  }

  /** A synchronous "not connected", with no round-trip to race. */
  setConnectionError(message: string): void {
    this.errorMessage = message;
  }

  /** Change the env the deploy-control reads scope to. Returns whether it moved. */
  setEnv(env: DeployEnv): boolean {
    if (this.environment === env) return false;
    this.environment = env;
    // The status and the suggestion are per-env, so the ones on screen are now
    // about the wrong environment. Invalidating rather than merely clearing is
    // what stops the OLD env's in-flight read from landing and looking like
    // the new one's.
    this.statusLatest.invalidate();
    this.deploymentStatus = null;
    this.statusNotice = "";
    this.versionSuggestion = null;
    return true;
  }

  /**
   * Load the three concept-row sets and rebuild the whole derived picture.
   *
   * `.current`, not `.begin()`: one refresh does not supersede another (the
   * caller has no "load more" here, and two refreshes landing in the same
   * generation are peers). reset() is the only thing that invalidates, which
   * is exactly the shape ConceptPanelState.loadPage uses.
   */
  async loadData(fetch: () => Promise<ClusterRowSets>): Promise<boolean> {
    const token = this.dataLatest.current;
    try {
      const rows = await fetch();
      if (!this.dataLatest.isCurrent(token)) return false;
      this.applyRows(rows);
      this.errorMessage = "";
      return true;
    } catch (err) {
      if (!this.dataLatest.isCurrent(token)) return false;
      this.errorMessage = err instanceof Error ? err.message : String(err);
      return true;
    }
  }

  /** Apply already-fetched rows. Split out so a CDC-driven refresh and the
   *  initial load share one derivation and cannot diverge. */
  private applyRows(rows: ClusterRowSets): void {
    this.deploymentRecords = projectDeployments(rows.deployments);
    this.nodeSpecs = projectNodeSpecs(rows.specs);
    this.current = currentDeploymentId(this.deploymentRecords);
    this.topologyView = buildTopology({
      nodeRows: rows.nodes,
      deployments: this.deploymentRecords,
      specs: this.nodeSpecs,
      currentDeploymentId: this.current,
    });
    // A selection that no longer names a real deployment is dropped rather
    // than kept: the composition pane would otherwise show an empty preview
    // with a row highlighted that is not in the list.
    if (
      this.selection !== undefined &&
      !this.deploymentRecords.some((d) => d.deploymentId === this.selection)
    ) {
      this.selection = undefined;
    }
  }

  /**
   * Record the deploy-control status read.
   *
   * `message` non-empty means the read did not produce a status -- refused by
   * the owner/admin gate, unavailable on this node, or failed. It is a
   * FIRST-CLASS outcome, not an error: topology and history are unaffected, so
   * the panel shows an explained section rather than treating the whole load
   * as broken.
   */
  async loadStatus(
    fetch: () => Promise<{ status: DeploymentStatus | null; message: string }>,
  ): Promise<boolean> {
    const token = this.statusLatest.current;
    const result = await fetch();
    if (!this.statusLatest.isCurrent(token)) return false;
    this.deploymentStatus = result.status;
    this.statusNotice = result.message;
    return true;
  }

  /** Record the next-version preview. Shares the status guard: both are per-env
   *  reads invalidated by the same events, and giving them separate guards
   *  would let an env flip clear one and not the other. */
  async loadSuggestion(
    fetch: () => Promise<{ suggestion: NextVersionSuggestion | null; message: string }>,
  ): Promise<boolean> {
    const token = this.statusLatest.current;
    const result = await fetch();
    if (!this.statusLatest.isCurrent(token)) return false;
    this.versionSuggestion = result.suggestion;
    return true;
  }

  /** Resolve which actions to draw. A failed read resolves to `indeterminate`,
   *  which OFFERS the actions with a notice -- see roleVisibility. */
  async loadAccess(fetch: () => Promise<Role | undefined>): Promise<boolean> {
    const token = this.accessLatest.current;
    try {
      const role = await fetch();
      if (!this.accessLatest.isCurrent(token)) return false;
      this.visibilityState = roleVisibility(role);
      return true;
    } catch (err) {
      if (!this.accessLatest.isCurrent(token)) return false;
      this.visibilityState = roleVisibility(
        undefined,
        `Your cluster role could not be read (${err instanceof Error ? err.message : String(err)}). Actions are offered, but the engine decides -- a refusal will name the role required.`,
      );
      return true;
    }
  }

  /**
   * Begin an action. Supersedes any earlier one -- hence begin(), not current():
   * starting a second action IS what makes the first one's outcome stale, and
   * two outcome lines racing to the same slot would leave whichever finished
   * last, not whichever was asked for last.
   */
  beginAction(): ActionToken {
    return this.actionLatest.begin();
  }

  /** Record an outcome, unless the action was superseded or the panel reset. */
  settleAction(token: ActionToken, outcome: DeployOutcome): boolean {
    if (!this.actionLatest.isCurrent(token)) return false;
    this.outcome = outcome;
    return true;
  }

  /** Select a deployment for the composition preview. Returns whether it moved. */
  selectDeployment(deploymentId: string): boolean {
    const next = deploymentId === "" ? undefined : deploymentId;
    if (this.selection === next) return false;
    this.selection = next;
    return true;
  }
}
