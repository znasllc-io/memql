import { type ApprovalRow } from "./rows";
import { approvalKindWord } from "./words";

const KINDS = ["sideEffect", "scopeElevation", "budget", "skillMint", "feedback", "planReview", "inferenceUnavailable", "routingReview", "procedurePromotion"];

// Keep the names a person sees searchable before the server pages the queue.
export function isFailureQuestion(approval: ApprovalRow): boolean {
  return approval.subject?.progressLimit !== true && approval.kind !== "budget"
    && approval.options.some(option => option.value === "retry")
    && approval.options.some(option => option.value === "abandon");
}
export function isRepositoryReview(approval: ApprovalRow): boolean {
  return approval.kind === "planReview" && approval.subject?.reviewType === "repository-change" && !isFailureQuestion(approval);
}
export function approvalLabel(approval: ApprovalRow): string {
  if (isRepositoryReview(approval)) return "Repository change";
  return isFailureQuestion(approval) ? "Work stopped" : approvalKindWord(approval.kind);
}
export function approvalMatches(approval: ApprovalRow, search: string): boolean {
  const text = search.trim().toLowerCase();
  return !text || [approval.kind, approvalLabel(approval), approval.question, approval.evidenceReason, approval.stepKey,
    ...["statement", "summary", "title", "description", "command", "message", "name"].map(key => approval.subject?.[key])]
    .some(value => typeof value === "string" && value.toLowerCase().includes(text));
}
export function approvalSearchArgs(search: string) {
  const text = search.trim().toLowerCase();
  return {
    search: search.trim() || undefined,
    kindLabels: text ? KINDS.filter(kind => approvalKindWord(kind).toLowerCase().includes(text)) : [],
    repositoryLabel: !!text && "repository change".includes(text),
    failureLabel: !!text && "work stopped".includes(text),
  };
}
