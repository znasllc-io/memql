// Cluster-topology and deployment rendering.
//
// The DevOps surface (memql#3312) draws three things: a grid of live cluster
// nodes, a newest-first list of deployments, and a couple of small
// name/detail/count tables (the replica tally, and a selected deployment's
// per-tier composition). All three are wanted by BOTH consumers -- the VS Code
// cluster tab and, later, the portal -- so they live here rather than as
// VS Code-specific DOM the portal would have to rebuild.
//
// The same rule rowList.ts enforces applies: NO judgement lives in the
// renderer. It does not decide what an orphan is, which deployment is current,
// or whether a tier is under-replicated -- it is handed booleans and strings
// that a caller already computed (see editors/vscode/src/state/topology.ts and
// state/deploymentHistory.ts) and draws them. That keeps the two consumers
// agreeing on the RULES by sharing the module that implements them, rather
// than by each re-deriving them from a renderer's markup.
//
// Values are surfaced as `data-*` attributes and never as per-value classes,
// matching `.vk-row-status[data-status]`: view-kit stays value-agnostic, and a
// host that wants "healthy is green, degraded is amber" writes those three
// rules against the data attribute in its own sheet.

import { h, text, type VNode } from "./vnode.js";

// One live cluster node, already projected. `orphanReason` is rendered as the
// flag's tooltip -- an operator seeing `[orphan]` needs to know WHICH rule
// caught it (a stopped node and a node from a superseded deployment are very
// different problems), and a tooltip carries that without widening the tile.
export interface TopologyNodeView {
  /** The node row id. Emitted as data-node-id for the host's click delegation. */
  id: string;
  /** What this node IS -- the node type, plus its flavor when it has one. */
  label: string;
  /** Secondary identity: the short node id, or the advertised address. */
  detail: string;
  /** The memQL version this node is running. Empty renders as "version unknown". */
  version: string;
  /** The short deployment id. Empty renders as "no deployment id". */
  deployment: string;
  /** connecting | healthy | degraded | draining | offline | stopped. */
  health: string;
  orphan: boolean;
  /** Why this node is an orphan. Ignored unless `orphan` is true. */
  orphanReason: string;
}

// One row of a small name/detail/count table. Deliberately generic: the
// replica tally ("cognition -- 1 of 2 running") and a deployment's per-tier
// composition ("cognition -- 2026.6.21 -- 2 replicas") are the same shape, and
// giving them one primitive is what keeps them looking like one surface.
export interface TallyRowView {
  name: string;
  detail: string;
  /** The trailing figure. Right-aligned and tabular so a column scans. */
  count: string;
  /** Non-empty renders a `[flag]` marker and sets data-flagged on the row. */
  flag: string;
}

// One deployment in the history list.
export interface DeploymentHistoryView {
  /** The deploymentId. Emitted as data-row-id -- the host's existing row-click
   *  delegation reads that attribute, so selection needs no new wiring. */
  id: string;
  version: string;
  environment: string;
  provider: string;
  /** pending | in_progress | succeeded | failed | superseded | rolled_back. */
  status: string;
  /** True for the deployment the cluster is currently running. */
  current: boolean;
}

/**
 * The topology grid.
 *
 * One tile per registered node. Every tile carries its health and orphan state
 * as data attributes, so a host colours them without this module knowing what
 * "degraded" should look like.
 */
export function renderTopologyGrid(nodes: TopologyNodeView[]): VNode {
  if (nodes.length === 0) {
    return h("div", { class: "vk-empty" }, [
      text("No cluster nodes registered."),
    ]);
  }

  return h(
    "ul",
    { class: "vk-grid" },
    nodes.map((node) => {
      const attrs: Record<string, string> = {
        class: "vk-tile",
        "data-node-id": node.id,
        "data-health": node.health,
      };
      if (node.orphan) attrs["data-orphan"] = "true";

      const children: VNode[] = [
        h("span", { class: "vk-tile-name" }, [text(node.label)]),
        h("span", { class: "vk-tile-health", "data-health": node.health }, [
          text(node.health === "" ? "unknown" : node.health),
        ]),
      ];

      if (node.orphan) {
        // The tooltip is the only place the REASON fits, and the reason is the
        // difference between "this pod is stopped" and "this pod is serving
        // traffic from a superseded release".
        children.push(
          h("span", { class: "vk-flag", title: node.orphanReason }, [text("[orphan]")]),
        );
      }

      if (node.detail !== "") {
        children.push(h("span", { class: "vk-tile-detail" }, [text(node.detail)]));
      }

      // An absent version or deployment id is rendered EXPLICITLY rather than
      // omitted. Both are load-bearing on this surface -- "which release is
      // this node running" is the question the grid exists to answer -- and a
      // silently missing line reads as "nothing to report" instead of "this
      // could not be resolved".
      children.push(
        node.version === ""
          ? h("span", { class: "vk-tile-meta vk-null" }, [text("version unknown")])
          : h("span", { class: "vk-tile-meta" }, [text(node.version)]),
      );
      children.push(
        node.deployment === ""
          ? h("span", { class: "vk-tile-meta vk-null" }, [text("no deployment id")])
          : h("span", { class: "vk-tile-meta" }, [text(`deploy ${node.deployment}`)]),
      );

      return h("li", attrs, children);
    }),
  );
}

/**
 * A small name / detail / count table.
 *
 * `emptyMessage` is the caller's, not this module's: "no node types are
 * declared for this deployment" and "this deployment declares no per-tier
 * spec" are different facts, and a shared primitive cannot know which one it
 * is being used for.
 */
export function renderTally(rows: TallyRowView[], emptyMessage: string): VNode {
  if (rows.length === 0) {
    return h("div", { class: "vk-empty" }, [text(emptyMessage)]);
  }

  return h(
    "ul",
    { class: "vk-tally" },
    rows.map((row) => {
      const attrs: Record<string, string> = { class: "vk-tally-row" };
      if (row.flag !== "") attrs["data-flagged"] = "true";

      const children: VNode[] = [h("span", { class: "vk-tally-name" }, [text(row.name)])];
      if (row.detail !== "") {
        children.push(h("span", { class: "vk-tally-detail" }, [text(row.detail)]));
      }
      if (row.flag !== "") {
        children.push(h("span", { class: "vk-flag" }, [text(`[${row.flag}]`)]));
      }
      children.push(h("span", { class: "vk-tally-count" }, [text(row.count)]));
      return h("li", attrs, children);
    }),
  );
}

/**
 * The deployment history list, newest-first (the CALLER orders it -- this
 * function renders whatever order it is handed).
 *
 * Deliberately reuses the `vk-row` contract rather than inventing a second
 * selectable-list markup: the row classes, the selected-row styling, and the
 * `data-row-id` click delegation every consumer already wired for the concept
 * browser all apply unchanged. The only addition is `vk-row-flag`, which marks
 * the current deployment.
 */
export function renderDeploymentHistory(
  entries: DeploymentHistoryView[],
  selectedId?: string,
): VNode {
  if (entries.length === 0) {
    return h("div", { class: "vk-empty" }, [
      text("No deployments recorded for this cluster."),
    ]);
  }

  return h(
    "ul",
    { class: "vk-rows" },
    entries.map((entry) => {
      const attrs: Record<string, string> = {
        class: "vk-row",
        "data-row-id": entry.id,
      };
      if (selectedId !== undefined && entry.id === selectedId) {
        attrs["data-selected"] = "true";
      }
      if (entry.current) attrs["data-current"] = "true";

      const children: VNode[] = [
        // Falls back to the deployment id, exactly as rowList's primary slot
        // falls back to the row id: a record with no version yet (a freshly
        // cut, still-pending deployment) must stay identifiable and clickable.
        h("span", { class: "vk-row-primary" }, [
          text(entry.version === "" ? entry.id : entry.version),
        ]),
      ];
      if (entry.environment !== "") {
        children.push(h("span", { class: "vk-row-secondary" }, [text(entry.environment)]));
      }
      if (entry.provider !== "") {
        children.push(h("span", { class: "vk-row-tertiary" }, [text(entry.provider)]));
      }
      if (entry.current) {
        children.push(h("span", { class: "vk-row-flag" }, [text("[current]")]));
      }
      if (entry.status !== "") {
        children.push(
          h("span", { class: "vk-row-status", "data-status": entry.status }, [
            text(entry.status),
          ]),
        );
      }
      return h("li", attrs, children);
    }),
  );
}
