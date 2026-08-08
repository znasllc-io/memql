// The Runs tree: the workspace's saved run configurations.
//
// An ADAPTER over run/runConfig.ts, which owns the file format, the validation
// and the read-modify-write. This file turns that into tree rows.
//
// The tree is a LIST, never a trigger. Loading the file populates rows and
// nothing else; running is always an explicit click. That matters because the
// file lives in the workspace so a repository can ship one -- opening a folder
// must never be able to make the editor talk to a cluster.

import * as vscode from "vscode";

import { readRunConfigs, runConfigPath, type RunConfig } from "../run/runConfig.js";

export type RunsTreeNode =
  | { kind: "run"; config: RunConfig }
  // The single synthetic row for a file that does not parse. Rendering an
  // empty list instead would look identical to "you have no run
  // configurations", and the developer would re-create the ones they have on
  // top of a file the writer is about to refuse anyway.
  | { kind: "error"; message: string }
  // Reported when entries were dropped, so a silently-shorter list is
  // explained rather than mysterious.
  | { kind: "dropped"; message: string };

export class RunsTreeProvider implements vscode.TreeDataProvider<RunsTreeNode> {
  private readonly changed = new vscode.EventEmitter<RunsTreeNode | undefined>();
  readonly onDidChangeTreeData = this.changed.event;

  constructor(private readonly workspaceRoot: string | undefined) {}

  refresh(): void {
    this.changed.fire(undefined);
  }

  async getChildren(element?: RunsTreeNode): Promise<RunsTreeNode[]> {
    if (element !== undefined) return [];
    if (this.workspaceRoot === undefined) return [];

    const result = await readRunConfigs(runConfigPath(this.workspaceRoot));
    if (!result.ok) return [{ kind: "error", message: result.error }];

    const out: RunsTreeNode[] = result.file.runs.map((config) => ({ kind: "run", config }));
    if (result.dropped.length > 0) {
      out.push({
        kind: "dropped",
        message: `${result.dropped.length} entr${result.dropped.length === 1 ? "y was" : "ies were"} ignored: ${result.dropped.join(", ")}`,
      });
    }
    return out;
  }

  getTreeItem(node: RunsTreeNode): vscode.TreeItem {
    if (node.kind === "error") {
      const item = new vscode.TreeItem(node.message, vscode.TreeItemCollapsibleState.None);
      item.iconPath = new vscode.ThemeIcon("error");
      item.tooltip = "Open .memql/runs.json and fix it by hand. Nothing will be written until it parses.";
      return item;
    }
    if (node.kind === "dropped") {
      const item = new vscode.TreeItem(node.message, vscode.TreeItemCollapsibleState.None);
      item.iconPath = new vscode.ThemeIcon("warning");
      return item;
    }

    const item = new vscode.TreeItem(node.config.name, vscode.TreeItemCollapsibleState.None);
    item.description = `${node.config.kind} ${node.config.construct}`;
    item.iconPath = new vscode.ThemeIcon("play");
    // contextValue is what the manifest's view/item/context `when` clause
    // matches, so the run and delete actions appear on configuration rows and
    // not on the error/dropped rows.
    item.contextValue = "memqlRun";
    item.tooltip = new vscode.MarkdownString(
      [
        `**${node.config.name}**`,
        "",
        `\`${node.config.kind} ${node.config.construct}\``,
        node.config.file === undefined ? "" : `File: \`${node.config.file}\``,
        "",
        "```json",
        JSON.stringify(node.config.args, null, 2),
        "```",
      ].join("\n"),
    );
    // NO `command` on the item. A tree item with a command runs it on a plain
    // SELECTION -- arrowing through the list with the keyboard would fire a
    // run per row. Running is an explicit inline action instead.
    return item;
  }
}
