// The Data tree: every registered concept on the connected cluster, grouped by
// domain.
//
// The list comes from ConceptsListMsg via the SDK's listConcepts, which the
// engine answers from its own registry -- so a concept added to the DSL shows
// up here with no client change. That is the whole point of a generic browser.
//
// NEVER BLANK. The view draws rows only for a cluster this editor is talking
// to; every other state of the connection leaves the tree EMPTY on purpose, so
// the manifest's welcome for that state renders over it with the one act that
// fixes it (state/clusterViewState.ts) -- Select a cluster, Sign in, Retry. It
// used to go blank instead: with a cluster selected and no session, `load()`
// returned an empty list, which no welcome matched and which looked exactly
// like a cluster with no concepts. While the dial is in flight the view shows
// its own progress bar; a connected cluster with genuinely no concepts says so
// in the view's message; a failed read gets a row whose click tries again.
//
// A READ BELONGS TO THE GENERATION THAT STARTED IT, exactly as in the
// Constructs view: ConceptsCache drops a superseded answer, and the root asks
// again rather than painting one.

import * as vscode from "vscode";

import type { Concept } from "@znasllc-io/memql-sdk-core/client";
import type { ConnectionManager } from "../connection/manager.js";
import { clusterViewState } from "../state/clusterViewState.js";
import { ConceptsCache } from "../state/conceptsCache.js";
import { briefMessage } from "../state/diagnostics.js";

export type ConceptTreeNode =
  | { kind: "domain"; domain: string; count: number }
  | { kind: "concept"; concept: Concept }
  // The single synthetic row getChildren returns when listConcepts() rejects
  // -- the connection can drop between the state check and the call. A failed
  // read must never look like an empty cluster.
  | { kind: "error"; message: string };

export interface DataTreeDeps {
  /** A failed read, for the MemQL Connection output. */
  noteFailure?: (message: string) => void;
  /** The line VS Code draws above the rows (`TreeView.message`). */
  setMessage?: (message: string | undefined) => void;
  /** The view's id, for its own progress bar while dialing. */
  viewId?: string;
}

/** What the view says when a connected cluster has no concepts. */
export const NO_CONCEPTS_MESSAGE = "This cluster has no concepts.";

export class DataTreeProvider implements vscode.TreeDataProvider<ConceptTreeNode> {
  private readonly changed = new vscode.EventEmitter<ConceptTreeNode | undefined>();
  readonly onDidChangeTreeData = this.changed.event;

  // Cached (and race-guarded against an out-of-order settle) so expanding a
  // domain does not re-issue the list, and a cluster switch mid-request
  // never lets a stale response overwrite the fresh cluster's data. See
  // state/conceptsCache.ts.
  private readonly cache = new ConceptsCache<Concept>();
  /** Bumped by every refresh, so a read that settles late can tell it was overtaken. */
  private generation = 0;
  /** The generation whose failure was already recorded, so a repaint does not record it twice. */
  private notedGeneration = -1;
  private endProgress: (() => void) | undefined;

  constructor(
    private readonly connections: Pick<ConnectionManager, "state" | "query" | "onDidChangeState">,
    private readonly deps: DataTreeDeps = {},
  ) {
    this.connections.onDidChangeState(() => this.onConnectionChange());
    this.onConnectionChange();
  }

  refresh(): void {
    this.generation += 1;
    this.cache.invalidate();
    this.changed.fire(undefined);
  }

  private onConnectionChange(): void {
    this.endProgress?.();
    this.endProgress = undefined;
    const view = clusterViewState(this.connections.state);
    if (view !== "connected") this.deps.setMessage?.(undefined);
    if (view === "connecting" && this.deps.viewId !== undefined) {
      const done = new Promise<void>((resolve) => {
        this.endProgress = resolve;
      });
      void vscode.window.withProgress({ location: { viewId: this.deps.viewId } }, () => done);
    }
    this.refresh();
  }

  /** The concepts, or undefined when there is no live session to read them from. */
  private async load(): Promise<Concept[] | undefined> {
    for (;;) {
      const query = this.connections.query;
      if (clusterViewState(this.connections.state) !== "connected" || query === undefined) return undefined;
      const generation = this.generation;
      const concepts = await this.cache.load(() => query.listConcepts());
      if (generation === this.generation) return concepts;
    }
  }

  async getChildren(element?: ConceptTreeNode): Promise<ConceptTreeNode[]> {
    // BEFORE the read, so a view with no live session issues no listConcepts
    // and the welcome for its state renders over the empty tree.
    const concepts = await this.load();
    if (concepts === undefined) return [];

    if (element === undefined) {
      const error = this.cache.cachedError;
      if (error !== undefined) {
        this.deps.setMessage?.(undefined);
        if (this.notedGeneration !== this.generation) {
          this.notedGeneration = this.generation;
          this.deps.noteFailure?.(error);
        }
        return [{ kind: "error", message: error }];
      }
      this.deps.setMessage?.(concepts.length === 0 ? NO_CONCEPTS_MESSAGE : undefined);
      const counts = new Map<string, number>();
      for (const c of concepts) counts.set(c.domain, (counts.get(c.domain) ?? 0) + 1);
      return [...counts.keys()].sort().map((domain) => ({ kind: "domain", domain, count: counts.get(domain) ?? 0 }));
    }

    if (element.kind === "domain") {
      return concepts
        .filter((c) => c.domain === element.domain)
        .sort((a, b) => a.entity.localeCompare(b.entity))
        .map((concept) => ({ kind: "concept", concept }));
    }

    return [];
  }

  getTreeItem(node: ConceptTreeNode): vscode.TreeItem {
    if (node.kind === "error") {
      // Brief, per the information policy; the full text goes to the MemQL
      // Connection output. The click is the fix.
      const item = new vscode.TreeItem("Couldn't load concepts", vscode.TreeItemCollapsibleState.None);
      item.contextValue = "memqlDataError";
      item.description = briefMessage(node.message, 60);
      item.tooltip = `${briefMessage(node.message)}\nSelect to try again.`;
      item.iconPath = new vscode.ThemeIcon("error", new vscode.ThemeColor("charts.red"));
      item.command = { command: "memql.data.refresh", title: "Try Again" };
      return item;
    }

    if (node.kind === "domain") {
      const item = new vscode.TreeItem(node.domain, vscode.TreeItemCollapsibleState.Collapsed);
      item.contextValue = "memqlConceptDomain";
      item.description = `${node.count}`;
      item.iconPath = new vscode.ThemeIcon("folder");
      return item;
    }

    // The entity alone: the domain is the row above and the id says both
    // again, so the id lives in the tooltip with the description.
    const item = new vscode.TreeItem(node.concept.entity, vscode.TreeItemCollapsibleState.None);
    item.contextValue = "memqlConcept";
    item.tooltip =
      node.concept.description !== "" ? `${node.concept.id}\n\n${node.concept.description}` : node.concept.id;
    item.iconPath = new vscode.ThemeIcon("symbol-class");
    item.command = {
      command: "memql.data.open",
      title: "Open Concept",
      arguments: [node.concept],
    };
    return item;
  }
}
