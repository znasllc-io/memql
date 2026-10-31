/** This restores a control only; the native decision gate validates authority
 * and the unchanged failed receipt before resuming any saved reporting stage. */
export function isReportingFailure(subject: Record<string, unknown> | null | undefined): boolean {
  const error = subject?.errorMessage;
  return typeof error === "string" && (error.includes("work_outcome_recovery_v1:")
    || error.startsWith('function "runAgentTurn" execution failed: runAgentTurn: invalid delegated work outcome:'));
}
