import { appList } from "../apps/settings/routingFacts";
import type { AccessSourceRow } from "../chrome/useDesktopAccess";

// WHAT IS OFFLINE, AND WHOM TO ASK (design record 2026-11-24-desktop-access,
// section 5). A person who is neither a cluster owner nor a developer cannot
// open Fleet, so Ask's failure messages never send them there: they name the
// source that is asleep or out of credit and the person who can fix it. The
// sources are the ones the access check listed for that person.
//
// The first match wins, in this order: the person's own machine, a lent
// machine, a lent key, their own key, an app. Nothing matching (including the
// check having failed, which has no sources) says the plain "otherwise"
// sentence, which is true whatever the cause.
//
// Regression tests: test/ask/accessWords.test.ts.

/** What a lender is called when no people read named them. */
export const LENDER_FALLBACK = "the person who lent it";

const NONE = "No model could take this request. Ask your administrator.";

export function unavailableSentence(sources: AccessSourceRow[], nameOf: (userId: string) => string): string {
  const lent = (s: AccessSourceRow) => !s.own && s.lenderUserId !== undefined;
  const lender = (s: AccessSourceRow) => nameOf(s.lenderUserId ?? "");

  const own = sources.find((s) => s.kind === "machine" && s.state === "asleep" && s.own);
  if (own) return own.label ? `Your machine ${own.label} is offline. Ask works again when it is back.` : "Your machine is offline. Ask works again when it is back.";

  const machine = sources.find((s) => s.kind === "machine" && s.state === "asleep" && lent(s));
  if (machine) {
    const name = lender(machine);
    // "The machine the person who lent it lent you" reads as nonsense.
    if (name === LENDER_FALLBACK) return `A machine lent to you is offline. Ask ${name}, or your administrator.`;
    return `The machine ${name} lent you is offline. Ask ${name}, or your administrator.`;
  }

  const key = sources.find((s) => s.kind === "key" && s.state === "outOfCredit" && lent(s));
  if (key) {
    const name = lender(key);
    if (name === LENDER_FALLBACK) return `An API key lent to you is out of credit. Ask ${name}, or your administrator.`;
    return `The API key ${name} lent you is out of credit. Ask ${name}, or your administrator.`;
  }

  if (sources.some((s) => s.kind === "key" && s.state === "outOfCredit" && s.own)) return "Your API key is out of credit.";

  const app = sources.find((s) => s.kind === "app" && s.state === "outOfUsage");
  if (app) return `${app.app ? appList([app.app]) : "An app"} has used up its allowance for now.`;

  return NONE;
}
