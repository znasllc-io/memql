import type { AccessSourceRow } from "../chrome/useDesktopAccess";
import { LENDER_FALLBACK, unavailableSentence } from "./accessWords";

/** Who is asking, as far as recovery goes: whether Fleet can be opened by them
 * and what their model access is made of (the desktop access check's sources). */
export interface AskAccess {
  canOpenFleet: boolean;
  sources?: AccessSourceRow[];
  nameOf?: (id: string) => string;
}

/** Legacy errors are strings. Only recognized routing refusals offer Fleet;
 * keep the original diagnostic in Details instead of guessing its cause.
 *
 * `opts` absent means a person who can open Fleet (today's messages). A person
 * who cannot gets the same refusal said as what is offline and whom to ask,
 * and never a Fleet button (design record 2026-11-24-desktop-access, section 5).
 */
export function askFailure(message: string, opts?: AskAccess): { summary: string; fleet: boolean } {
  if (message.includes("every_door_shut") || message.includes("no eligible fleet machine for this model")) {
    if (opts && !opts.canOpenFleet) {
      return { summary: unavailableSentence(opts.sources ?? [], opts.nameOf ?? (() => LENDER_FALLBACK)), fleet: false };
    }
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
