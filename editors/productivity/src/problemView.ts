import type { Problem } from "./problems.js";

/** Render only the host's safe message and correlation reference, never HTML. */
export function renderProblem(target: HTMLElement, problem: Problem, post: (value: unknown) => void): void {
  target.replaceChildren(document.createTextNode(problem.message));
  if (!problem.reference) return;
  const details = document.createElement("details"); details.className = "problem-details";
  details.style.marginTop = "8px"; details.style.color = "var(--vscode-descriptionForeground)";
  const summary = document.createElement("summary"); summary.textContent = "Details";
  const reference = document.createElement("code"); reference.textContent = problem.reference;
  reference.style.overflowWrap = "anywhere";
  const hint = document.createElement("p"); hint.textContent = "Search this reference in MemQL OS → Logs. If offline, open the MemQL Productivity Tools output in VS Code.";
  const copy = document.createElement("button"); copy.textContent = "Copy reference"; copy.className = "secondary";
  copy.addEventListener("click", () => post({type:"copyProblemReference",reference:problem.reference}));
  details.append(summary, reference, hint, copy); target.append(details);
}
