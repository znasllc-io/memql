/** Legacy errors are strings. Only recognized routing refusals offer Fleet;
 * keep the original diagnostic in Details instead of guessing its cause. */
export function askFailure(message: string): { summary: string; fleet: boolean } {
  if (message.includes("every_door_shut") || message.includes("no eligible fleet machine for this model")) {
    return { summary: "No eligible model could take this request. Check the available models in Fleet.", fleet: true };
  }
  if (message.includes('compose: unknown format')) {
    return { summary: "The selected file creator could not handle the requested format. Your request needs a compatible creation capability.", fleet: false };
  }
  if (message.length > 240 || message.includes("\n") || /Spine refused:|work compile:|automation .*execution failed:/.test(message)) {
    return { summary: "MemQL could not complete this request. You can edit the request or open Details to inspect the failure.", fleet: false };
  }
  return { summary: message, fleet: false };
}
