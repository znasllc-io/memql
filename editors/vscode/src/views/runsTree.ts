// The Runs tree: the workspace's saved runs.
//
// An ADAPTER over run/runConfig.ts, which owns the file format, the validation
// and the read-modify-write. This file turns that into tree rows.
//
// The tree is a LIST, never a trigger. Loading the file populates rows and
// nothing else; running is always an explicit click. That matters because the
// file lives in the workspace so a repository can ship one -- opening a folder
// must never be able to make the editor talk to a cluster.
//
// THE STATED EXCEPTION TO CONNECTION GATING (memql#4425, design D2). Every
// other cluster-backed view empties itself when no cluster is connected, so
// its welcome can say why; THIS ONE KEEPS LISTING, because `runs.json` entries
// are the DEVELOPER'S OWN FILE. Emptying the view because no cluster happens
// to be connected would present their saved work as gone. So the gate moves to
// the ACT: the inline Run appears only while connected (the manifest keys it on
// `memql.connected`), and the view's message says what running needs.
//
// EMPTY IS STILL SAID. With no folder open, or no saved runs yet, the tree is
// empty and the manifest's welcome for that case renders over it -- neither is
// keyed on the connection.

import * as vscode from "vscode";

import { readRunConfigs, runConfigPath, type RunConfig } from "../run/runConfig.js";
import { briefMessage } from "../state/diagnostics.js";
import { kindWord } from "../state/constructCatalog.js";
import { kindIcon } from "./constructsTree.js";

export type RunsTreeNode =
  | { kind: "run"; config: RunConfig }
  // The single synthetic row for a file that does not parse. Rendering an
  // empty list instead would look identical to "you have no saved runs", and
  // the developer would re-create the ones they have on top of a file the
  // writer is about to refuse anyway.
  | { kind: "error"; message: string }
  // Reported when entries were dropped, so a silently-shorter list is
  // explained rather than mysterious.
  | { kind: "dropped"; entries: string[] };

export interface RunsTreeOptions {
  /** Whether a run could go anywhere right now. Read only for the message, never to hide a row. */
  connected?: () => boolean;
  /** The line VS Code draws above the rows (`TreeView.message`). */
  setMessage?: (message: string | undefined) => void;
}

/** What the view says above saved runs while nothing is connected to run them on. */
export const RUNS_NEED_CLUSTER_MESSAGE = "Connect to a cluster to run these.";

export class RunsTreeProvider implements vscode.TreeDataProvider<RunsTreeNode> {
  private readonly changed = new vscode.EventEmitter<RunsTreeNode | undefined>();
  readonly onDidChangeTreeData = this.changed.event;

  constructor(
    private readonly workspaceRoot: string | undefined,
    private readonly options: RunsTreeOptions = {},
  ) {}

  refresh(): void {
    this.changed.fire(undefined);
  }

  async getChildren(element?: RunsTreeNode): Promise<RunsTreeNode[]> {
    if (element !== undefined) return [];
    const out = await this.rows();
    const hasRuns = out.some((row) => row.kind === "run");
    const connected = this.options.connected?.() ?? true;
    this.options.setMessage?.(hasRuns && !connected ? RUNS_NEED_CLUSTER_MESSAGE : undefined);
    return out;
  }

  private async rows(): Promise<RunsTreeNode[]> {
    if (this.workspaceRoot === undefined) return [];
    const file = runConfigPath(this.workspaceRoot);
    const result = await readRunConfigs(file);
    if (!result.ok) {
      // The file's own path is the one thing a row label must not carry: it is
      // long, it is absolute, and the row's click opens the file anyway.
      const prefix = `${file}: `;
      return [{ kind: "error", message: result.error.startsWith(prefix) ? result.error.slice(prefix.length) : result.error }];
    }
    const out: RunsTreeNode[] = result.file.runs.map((config) => ({ kind: "run", config }));
    if (result.dropped.length > 0) out.push({ kind: "dropped", entries: result.dropped });
    return out;
  }

  getTreeItem(node: RunsTreeNode): vscode.TreeItem {
    if (node.kind === "error") {
      const item = new vscode.TreeItem("Can't read runs.json", vscode.TreeItemCollapsibleState.None);
      item.description = briefMessage(node.message, 60);
      item.tooltip = `${briefMessage(node.message)}\nSelect to open the file.`;
      item.iconPath = new vscode.ThemeIcon("error", new vscode.ThemeColor("charts.red"));
      item.command = { command: "memql.runs.open", title: "Open runs.json" };
      return item;
    }
    if (node.kind === "dropped") {
      const n = node.entries.length;
      const item = new vscode.TreeItem(
        `${n} saved run${n === 1 ? "" : "s"} skipped`,
        vscode.TreeItemCollapsibleState.None,
      );
      item.tooltip = `Couldn't read ${node.entries.map(entryWords).join(", ")} in runs.json.\nSelect to open the file.`;
      item.iconPath = new vscode.ThemeIcon("warning", new vscode.ThemeColor("charts.yellow"));
      item.command = { command: "memql.runs.open", title: "Open runs.json" };
      return item;
    }

    const { config } = node;
    const item = new vscode.TreeItem(config.name, vscode.TreeItemCollapsibleState.None);
    item.description = config.construct;
    // The KIND as the icon, not a play glyph: Run is the row's inline ACT, and
    // an act drawn as the row's noun would be the same picture twice.
    item.iconPath = new vscode.ThemeIcon(kindIcon(config.kind));
    // contextValue is what the manifest's view/item/context `when` clause
    // matches, so Run and Delete appear on saved-run rows only.
    item.contextValue = "memqlRun";
    // NAMES AND COUNTS, NEVER VALUES (memql#4194). Saved arguments are whatever
    // a developer typed -- an address, a name, a pasted credential -- and a
    // hover republishes them to anyone looking at the screen. The values are
    // one click away in the file.
    const argCount = Object.keys(config.args).length;
    const facts = [
      `${kindWord(config.kind)} ${config.construct}`,
      argCount === 0 ? "no saved arguments" : `${argCount} saved argument${argCount === 1 ? "" : "s"}`,
    ];
    if (config.file !== undefined) facts.push(config.file);
    item.tooltip = `${config.name}\n${facts.join(" · ")}`;
    // NO `command` on the item. A tree item with a command runs it on a plain
    // SELECTION -- arrowing through the list with the keyboard would fire a
    // run per row. Running is an explicit inline act instead.
    return item;
  }
}

/** `runs[3]` as a person counts it: "entry 4". */
function entryWords(entry: string): string {
  return entry.replace(/^runs\[(\d+)\]/, (_all, index: string) => `entry ${Number(index) + 1}`);
}
