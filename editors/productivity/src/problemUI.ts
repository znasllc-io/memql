import * as vscode from "vscode";
import { reportProblem } from "./problems.js";

let output: vscode.OutputChannel | undefined;
export function setProblemOutput(value: vscode.OutputChannel): void { output = value; }
/**
 * One toast per problem. When the problem has a reference, the toast's one button reveals the output channel,
 * which holds the reference line with the technical details. There is no dialog behind it.
 */
export async function showProblem(error: unknown, operation: string): Promise<void> {
  const problem = reportProblem(error, operation);
  const choice = await vscode.window.showErrorMessage(problem.message, ...(problem.reference ? ["Show details"] : []));
  if (choice === "Show details") output?.show(true);
}
