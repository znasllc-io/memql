// The automation-run surface (memql#3310): the only invoke path an automation
// has on any surface. Synthesize a trigger event for a named automation,
// dispatch it through the normal automation path, and stream back a step
// trace.

export {
  AutomationClient,
  AutomationRunError,
  CODE_INVALID_ARGUMENT,
  CODE_DEADLINE_EXCEEDED,
  CODE_NOT_FOUND,
  CODE_PERMISSION_DENIED,
  CODE_RESOURCE_EXHAUSTED,
  CODE_FAILED_PRECONDITION,
  CODE_UNAVAILABLE,
  type AutomationRunAccepted,
  type AutomationRunComplete,
  type AutomationRunResult,
  type AutomationRunStep,
  type RunAutomationOptions,
} from "./automationRun.js";
