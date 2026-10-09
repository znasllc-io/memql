import * as vscode from "vscode";
import { reportProblem, type Problem } from "./problems.js";

let output: vscode.OutputChannel | undefined;
export function setProblemOutput(value: vscode.OutputChannel): void { output = value; }
export async function showProblem(error: unknown, operation: string): Promise<void> {
  const problem = reportProblem(error, operation);
  const choice = await vscode.window.showErrorMessage(problem.message, ...(problem.reference ? ["Details"] : []));
  if (choice === "Details") await showProblemDetails(problem);
}
export async function showProblemDetails(problem: Problem): Promise<void> {
  if (!problem.reference) return;
  const choice = await vscode.window.showInformationMessage("Troubleshooting reference", {
    modal: true,
    detail: `${problem.reference}\n\nSearch this reference in MemQL OS → Logs. If the cluster was unavailable, technical details remain in the MemQL Productivity Tools output here.`,
  }, "Copy reference", "Open local log");
  if (choice === "Copy reference") {
    try { await vscode.env.clipboard.writeText(problem.reference); }
    catch (error) { void showProblem(error, "copy the reference"); }
  }
  if (choice === "Open local log") output?.show(true);
}
