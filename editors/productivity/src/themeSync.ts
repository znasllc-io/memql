import * as vscode from "vscode";
import { appearanceSettings, readEditorAppearance } from "./editorAppearance.js";

/** Desktop choice persists in Settings; closing the offer also means keep it. */
export async function syncDesktopAppearance(raw: string | null): Promise<void> {
  const appearance = readEditorAppearance(raw);
  if (!appearance) return;
  const kind = vscode.window.activeColorTheme.kind;
  if (kind !== vscode.ColorThemeKind.Light && kind !== vscode.ColorThemeKind.Dark) return;
  const settings = vscode.workspace.getConfiguration("memql.productivity");
  let choice = settings.get<string>("osTheme", "ask");
  if (choice === "ask") {
    const answer = await vscode.window.showInformationMessage(
      "Match this editor to MemQL OS when opening its files?", "Match MemQL OS", "Keep editor theme");
    choice = answer === "Match MemQL OS" ? "match" : "keep";
    await settings.update("osTheme", choice, vscode.ConfigurationTarget.Global);
  }
  if (choice !== "match") return;
  const workbench = vscode.workspace.getConfiguration("workbench");
  const next = appearanceSettings(appearance, workbench.inspect<Record<string, unknown>>("colorCustomizations")?.globalValue);
  await workbench.update("colorCustomizations", next.colors, vscode.ConfigurationTarget.Global);
  await workbench.update("colorTheme", next.name, vscode.ConfigurationTarget.Global);
}
