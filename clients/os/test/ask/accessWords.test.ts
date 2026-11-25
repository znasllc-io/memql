import { describe, expect, it } from "vitest";

import { LENDER_FALLBACK, unavailableSentence } from "../../src/ask/accessWords";
import { askFailure } from "../../src/ask/askFailure";
import type { AccessSourceRow } from "../../src/chrome/useDesktopAccess";

const names: Record<string, string> = { u1: "Priya" };
const nameOf = (id: string) => names[id] ?? LENDER_FALLBACK;

describe("unavailableSentence", () => {
  it("names the person's own machine that is asleep", () => {
    const sources: AccessSourceRow[] = [{ kind: "machine", state: "asleep", own: true, label: "Desk" }];
    expect(unavailableSentence(sources, nameOf)).toBe("Your machine Desk is offline. Ask works again when it is back.");
  });
  it("names a lent machine that is asleep and whom to ask", () => {
    const sources: AccessSourceRow[] = [{ kind: "machine", state: "asleep", own: false, lenderUserId: "u1", label: "Desk" }];
    expect(unavailableSentence(sources, nameOf)).toBe("The machine Priya lent you is offline. Ask Priya, or your administrator.");
  });
  it("names a lent key that is out of credit", () => {
    const sources: AccessSourceRow[] = [{ kind: "key", state: "outOfCredit", own: false, lenderUserId: "u1" }];
    expect(unavailableSentence(sources, nameOf)).toBe("The API key Priya lent you is out of credit. Ask Priya, or your administrator.");
  });
  it("says the person's own key is out of credit", () => {
    const sources: AccessSourceRow[] = [{ kind: "key", state: "outOfCredit", own: true }];
    expect(unavailableSentence(sources, nameOf)).toBe("Your API key is out of credit.");
  });
  it("names an app by its display name, not its id or machine label", () => {
    const sources: AccessSourceRow[] = [{ kind: "app", state: "outOfUsage", own: true, app: "codex", label: "codex on Desk" }];
    expect(unavailableSentence(sources, nameOf)).toBe("Codex has used up its allowance for now.");
    expect(unavailableSentence([{ kind: "app", state: "outOfUsage", own: true, app: "claude-code" }], nameOf))
      .toBe("Claude Code has used up its allowance for now.");
  });
  it("falls back to an administrator when nothing is named", () => {
    expect(unavailableSentence([], nameOf)).toBe("No model could take this request. Ask your administrator.");
    expect(unavailableSentence([{ kind: "machine", state: "ready", own: true, label: "Desk" }], nameOf))
      .toBe("No model could take this request. Ask your administrator.");
  });
  it("does not say 'the machine the person who lent it lent you' when the lender is not known", () => {
    const sources: AccessSourceRow[] = [{ kind: "machine", state: "asleep", own: false, lenderUserId: "zz" }];
    expect(unavailableSentence(sources, nameOf)).toBe("A machine lent to you is offline. Ask the person who lent it, or your administrator.");
  });
});

describe("askFailure", () => {
  const shut = "every_door_shut: nothing could answer";
  const sources: AccessSourceRow[] = [{ kind: "machine", state: "asleep", own: true, label: "Desk" }];
  it("offers Fleet only to a person who can open it", () => {
    expect(askFailure(shut).fleet).toBe(true);
    expect(askFailure(shut, { canOpenFleet: true, sources }).fleet).toBe(true);
    expect(askFailure(shut, { canOpenFleet: true, sources }).summary)
      .toBe("No eligible model could take this request. Check the available models in Fleet.");
  });
  it("names what is offline, and never Fleet, for a person who cannot open it", () => {
    const out = askFailure(shut, { canOpenFleet: false, sources, nameOf });
    expect(out.fleet).toBe(false);
    expect(out.summary).toBe("Your machine Desk is offline. Ask works again when it is back.");
    expect(askFailure("no eligible fleet machine for this model", { canOpenFleet: false }).summary)
      .toBe("No model could take this request. Ask your administrator.");
  });
  it("leaves other failures alone", () => {
    expect(askFailure("plain trouble", { canOpenFleet: false }).summary).toBe("plain trouble");
  });
});
