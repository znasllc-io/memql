import { cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// Fleet > Routing > Rules (was Settings > Rules; epic memql#5153, D1, and the
// routing redesign of 2026-09-28).
//
// ===========================================================================
// WHAT THIS SUITE PROTECTS
// ===========================================================================
// AN ACT THAT IS NOT LEGAL IS ABSENT (DESIGN.md rule 12). The engine refuses
// to change or remove a shipped rule BY NAME, so a shipped row carries no
// control at all -- asserted as a COUNT of buttons, because a greyed-out
// control passes "the click did nothing" and fails what this is about.
//
// ACTIVATE IS NOT REACHABLE UNTIL SOMETHING HAS BEEN CHECKED, and an edit
// underneath a check withdraws it.
//
// THE FORM SENDS ONLY THE CONDITIONS THAT WERE TICKED -- the object that
// reaches the save, not just the pure function that builds it.
//
// A REORDER WRITES PRECEDENCES THAT NEVER TIE, one save at a time, each naming
// the revision the last one produced -- driven by the keyboard as well as by
// a drag.

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { renderRouting, routingConnection, ruleRow, settle, shippedRules } = await import("./routingHarness");
const { RULES_SECTION_RESOURCE } = await import("../../src/apps/fleet/routing/access");
const { OS_REGISTRY } = await import("../../src/apps/registry");
const { roleOpens } = await import("../seededAccess");
const { chooseOption } = await import("../selectControl");

afterEach(cleanup);

let calls: ReturnType<typeof routingConnection>["calls"];
let state: ReturnType<typeof routingConnection>["state"];
function use(rules = [
  ...shippedRules(),
  ruleRow({ name: "nightlyIsCheap", when: { tag: "nightly" }, policy: "localOnly", precedence: 10, described: "keep the nightly run off a vendor" }),
  ruleRow({ name: "planningStaysLocal", when: { level: "reasoning" }, policy: "localOnly", precedence: 30, onUnavailable: "park" }),
]) {
  const made = routingConnection({ rules });
  h.connection = made.connection;
  calls = made.calls;
  state = made.state;
}
beforeEach(() => use());

async function openRules(role = "owner") {
  await renderRouting(role, { intent: { id: "to-rules", payload: { routingTab: "rules" } } });
  return screen.getByRole("list", { name: "Rules, in the order they are tried" });
}

function lines(): HTMLElement[] {
  return within(screen.getByRole("list", { name: "Rules, in the order they are tried" })).getAllByRole("listitem");
}
function sentences(): string[] {
  return lines().map((li) => {
    const s = li.querySelector(".fleet-rule-sentence");
    return [s?.querySelector(".fleet-rule-when")?.textContent, s?.querySelector(".fleet-rule-route")?.textContent].join(" -> ");
  });
}
function customLine(when: RegExp): HTMLElement {
  const found = lines().map((li) => li.querySelector<HTMLElement>("button.fleet-rule-line")).find((b) => b !== null && when.test(b.textContent ?? ""));
  if (!found) throw new Error(`no custom rule line matching ${when}`);
  return found;
}
function floorBar(): HTMLElement {
  return screen.getByRole("group", { name: "What you can do with this" });
}

describe("who may see the rules", () => {
  it("admits an owner and a developer, and refuses an admin", () => {
    // A SET rather than a floor: the ladder puts admin (200) BELOW developer
    // (300), so `{ min: "developer" }` would admit admin -- whose concern is
    // user administration, not what the cluster talks to.
    expect(roleOpens("owner", RULES_SECTION_RESOURCE)).toBe(true);
    expect(roleOpens("developer", RULES_SECTION_RESOURCE)).toBe(true);
    expect(roleOpens("admin", RULES_SECTION_RESOURCE)).toBe(false);
    expect(roleOpens("writer", RULES_SECTION_RESOURCE)).toBe(false);
  });

  it("is the same resource Settings still declares for its signpost", () => {
    const settings = OS_REGISTRY.apps.find((a) => a.id === "settings");
    const section = settings?.sections?.find((s) => s.id === "rules");
    expect(section?.requires).toBe(RULES_SECTION_RESOURCE);
    // Off the Settings rail: a drill-down, reached by a link.
    expect(section?.parent).toBe("levels");
  });
});

describe("the rules, as sentences", () => {
  it("reads each rule as When -> Route, in the order the engine tries them, the floor last", async () => {
    await openRules();
    expect(sentences()).toEqual([
      "Embeddings -> Embeddings",
      "Reasoning work -> Local, then best vendor",
      "Fast work -> Fast local first",
      "Background work -> Local first",
      "Reasoning work -> Local only",
      "Tagged nightly -> Local only",
      "Everything else -> Local first",
    ]);
  });

  it("gives a shipped rule a lock and no control at all", async () => {
    await openRules();
    const shipped = lines().filter((li) => li.hasAttribute("data-os-locked"));
    expect(shipped).toHaveLength(5);
    for (const li of shipped) {
      expect(li.querySelector(".fleet-rule-lock")).not.toBeNull();
      expect(within(li).queryAllByRole("button")).toHaveLength(0);
    }
    // One control per rule the person wrote -- the rule itself.
    expect(within(screen.getByRole("list", { name: "Rules, in the order they are tried" })).getAllByRole("button")).toHaveLength(2);
  });

  it("says what else a rule does in a few words", async () => {
    await openRules();
    expect(customLine(/Reasoning work/).textContent).toContain("waits when nothing is ready");
  });

  it("says the cluster does not route by rules, which is not 'no rules', and offers no Add", async () => {
    state.missing.add("routingRules");
    state.missing.add("routingRuleSave");
    state.missing.add("routingRuleActivate");
    await renderRouting("owner", { intent: { id: "r", payload: { routingTab: "rules" } } });
    expect(screen.getByText(/does not route by rules yet/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add a rule" })).toBeNull();
  });

  it("renders the cluster's own refusal of the read", async () => {
    calls.routingRules.mockRejectedValue(new Error("routingRules is owner-only"));
    await renderRouting("developer", { intent: { id: "r", payload: { routingTab: "rules" } } });
    expect(screen.getByText("routingRules is owner-only")).toBeTruthy();
  });
});

describe("reordering your rules", () => {
  it("moves a rule down with Alt+ArrowDown: one save, a precedence between its new neighbours", async () => {
    // Yours, highest first: planningStaysLocal (30), nightlyIsCheap (10).
    // Moving the first below the second puts it 10 below: 0.
    await openRules();
    const line = customLine(/Reasoning work/);
    line.focus();
    fireEvent.keyDown(line, { key: "ArrowDown", altKey: true });
    await settle();
    expect(calls.routingRuleSave).toHaveBeenCalledTimes(1);
    expect(calls.routingRuleSave).toHaveBeenCalledWith(expect.objectContaining({ name: "planningStaysLocal", precedence: 0, expectedRevision: 7, conditions: { level: "reasoning" }, policy: "localOnly", onUnavailable: "park" }));
    expect(sentences().slice(4, 6)).toEqual(["Tagged nightly -> Local only", "Reasoning work -> Local only"]);
    expect(document.activeElement).toBe(customLine(/Reasoning work/));
  });

  it("ignores the arrow keys without Alt, and a move off the end", async () => {
    await openRules();
    const line = customLine(/Reasoning work/);
    fireEvent.keyDown(line, { key: "ArrowDown" });
    fireEvent.keyDown(line, { key: "ArrowUp", altKey: true });
    await settle();
    expect(calls.routingRuleSave).not.toHaveBeenCalled();
  });

  it("renumbers with chained revisions when there is no room, and no write ties", async () => {
    use([
      ...shippedRules(),
      ruleRow({ name: "a", when: { tag: "a" }, precedence: 11 }),
      ruleRow({ name: "b", when: { tag: "b" }, precedence: 10 }),
      ruleRow({ name: "c", when: { tag: "c" }, precedence: 9 }),
    ]);
    await openRules();
    fireEvent.keyDown(customLine(/Tagged c/), { key: "ArrowUp", altKey: true });
    await settle();
    const writes = calls.routingRuleSave.mock.calls.map(([args]) => [args.name, args.precedence, args.expectedRevision]);
    expect(writes).toEqual([
      ["a", 41, 7],
      ["c", 31, 8],
      ["b", 21, 9],
    ]);
    expect(sentences().slice(4, 7)).toEqual(["Tagged a -> Local first", "Tagged c -> Local first", "Tagged b -> Local first"]);
  });

  it("moves a rule by dragging it onto another", async () => {
    await openRules();
    fireEvent.dragStart(customLine(/Tagged nightly/));
    fireEvent.dragOver(customLine(/Reasoning work/));
    fireEvent.drop(customLine(/Reasoning work/));
    await settle();
    expect(calls.routingRuleSave).toHaveBeenCalledWith(expect.objectContaining({ name: "nightlyIsCheap", precedence: 40 }));
  });

  it("puts the order back and says why when a save is refused", async () => {
    state.ruleSaveError = new Error("routing configuration changed; refresh and validate again");
    await openRules();
    fireEvent.keyDown(customLine(/Reasoning work/), { key: "ArrowDown", altKey: true });
    await settle();
    expect(screen.getByText(/routing configuration changed/)).toBeTruthy();
    expect(sentences().slice(4, 6)).toEqual(["Reasoning work -> Local only", "Tagged nightly -> Local only"]);
  });
});

describe("adding a rule", () => {
  it("When, then Route, then Review: a choice answers its step, and Add rule checks then saves", async () => {
    await openRules();
    fireEvent.click(screen.getByRole("button", { name: "Add a rule" }));
    fireEvent.click(screen.getByRole("radio", { name: "Fast work" }));
    // Chosen is answered: the wizard is on Route now.
    fireEvent.click(screen.getByRole("radio", { name: /Local only/ }));
    expect(screen.getByRole("group", { name: "What you can do with this" }).textContent).toContain("Ready to add");
    const review = document.querySelector(".fleet-rule-review")!;
    expect(review.querySelector(".fleet-rule-when")?.textContent).toBe("Fast work");
    expect(review.querySelector(".fleet-rule-route")?.textContent).toBe("Local only");
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Add rule" }));
    await settle();
    expect(calls.routingRuleValidate).toHaveBeenCalled();
    expect(calls.routingRuleSave).toHaveBeenCalledWith(expect.objectContaining({ name: "fastWorkLocalOnly", conditions: { level: "fast" }, policy: "localOnly", precedence: 40, expectedRevision: 7 }));
    expect(screen.getByRole("list", { name: "Rules, in the order they are tried" })).toBeTruthy();
  });

  it("asks one more thing for a prompt, and only then offers Next", async () => {
    await openRules();
    fireEvent.click(screen.getByRole("button", { name: "Add a rule" }));
    fireEvent.click(screen.getByRole("radio", { name: "A prompt" }));
    expect(within(floorBar()).queryByRole("button", { name: "Next" })).toBeNull();
    fireEvent.change(screen.getByLabelText("Prompt name"), { target: { value: "agentReply" } });
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Next" }));
    fireEvent.click(screen.getByRole("radio", { name: /Local first/ }));
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Add rule" }));
    await settle();
    expect(calls.routingRuleSave).toHaveBeenCalledWith(expect.objectContaining({ conditions: { prompt: "agentReply" }, policy: "localFirst" }));
  });

  it("offers Describe it instead, and the described rule goes compile -> check -> activate", async () => {
    await openRules();
    fireEvent.click(screen.getByRole("button", { name: "Add a rule" }));
    fireEvent.click(screen.getByRole("button", { name: "Describe it instead" }));
    const box = screen.getByLabelText("Describe a rule in your own words");
    // Nothing to do yet: no forward act at all.
    expect(within(floorBar()).queryByRole("button", { name: "Compile it" })).toBeNull();
    fireEvent.change(box, { target: { value: "send planning work to my own machines" } });
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Compile it" }));
    await settle();
    const said = screen.getByRole("group", { name: "The rule it becomes" });
    expect(said.querySelector(".fleet-rule-when")?.textContent).toBe("Reasoning work");
    expect(said.querySelector(".fleet-rule-route")?.textContent).toBe("Local only");
    expect(within(floorBar()).queryByRole("button", { name: "Activate" })).toBeNull();
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Check it" }));
    await settle();
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Activate" }));
    await settle();
    expect(calls.routingRuleSave).toHaveBeenCalledWith(expect.objectContaining({ name: "planningIsLocal", conditions: { level: "reasoning" }, policy: "localOnly", description: "send planning work to my own machines" }));
  });

  it("withdraws Check and Activate the moment the words change", async () => {
    await openRules();
    fireEvent.click(screen.getByRole("button", { name: "Add a rule" }));
    fireEvent.click(screen.getByRole("button", { name: "Describe it instead" }));
    const box = screen.getByLabelText("Describe a rule in your own words");
    fireEvent.change(box, { target: { value: "send planning work to my own machines" } });
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Compile it" }));
    await settle();
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Check it" }));
    await settle();
    expect(within(floorBar()).getByRole("button", { name: "Activate" })).toBeTruthy();
    fireEvent.change(box, { target: { value: "send planning work anywhere" } });
    expect(within(floorBar()).queryByRole("button", { name: "Activate" })).toBeNull();
    expect(within(floorBar()).queryByRole("button", { name: "Check it" })).toBeNull();
    expect(within(floorBar()).getByRole("button", { name: "Compile it" })).toBeTruthy();
  });

  it("renders a compiler refusal verbatim and keeps the words", async () => {
    const refusal = "I could not read \"never to a paid vendor\" as a route.";
    calls.routingRuleDescribe.mockResolvedValue({ rows: () => [{ problem: refusal }] });
    await openRules();
    fireEvent.click(screen.getByRole("button", { name: "Add a rule" }));
    fireEvent.click(screen.getByRole("button", { name: "Describe it instead" }));
    const words = "send planning work to my own machines, never to a paid vendor";
    fireEvent.change(screen.getByLabelText("Describe a rule in your own words"), { target: { value: words } });
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Compile it" }));
    await settle();
    expect(screen.getByText(refusal)).toBeTruthy();
    expect((screen.getByLabelText("Describe a rule in your own words") as HTMLTextAreaElement).value).toBe(words);
    expect(within(floorBar()).queryByRole("button", { name: "Check it" })).toBeNull();
  });
});

describe("a rule's own page", () => {
  it("sends the ONE condition that was ticked, and no key for the six that were not", async () => {
    await openRules();
    fireEvent.click(customLine(/Tagged nightly/));
    fireEvent.click(screen.getByRole("checkbox", { name: "the call's tag" })); // untick the tag
    fireEvent.click(screen.getByRole("checkbox", { name: "the level asked for" }));
    fireEvent.change(screen.getByLabelText("Value for the level asked for"), { target: { value: "reasoning" } });
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Check it" }));
    await settle();
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Activate" }));
    await settle();
    const conditions = calls.routingRuleSave.mock.calls[0]![0].conditions as Record<string, string>;
    expect(conditions).toEqual({ level: "reasoning" });
  });

  it("opens on the rule's OWN conditions and route", async () => {
    await openRules();
    fireEvent.click(customLine(/Reasoning work/));
    expect((screen.getByRole("checkbox", { name: "the level asked for" }) as HTMLInputElement).checked).toBe(true);
    expect((screen.getByRole("checkbox", { name: "the call's tag" }) as HTMLInputElement).checked).toBe(false);
    expect((screen.getByLabelText("Value for the level asked for") as HTMLInputElement).value).toBe("reasoning");
    expect(screen.getByLabelText("Route").textContent).toContain("Local only");
  });

  it("changes the route through the Route field", async () => {
    await openRules();
    fireEvent.click(customLine(/Tagged nightly/));
    chooseOption(screen.getByLabelText("Route"), "Fast local first");
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Check it" }));
    await settle();
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Activate" }));
    await settle();
    expect(calls.routingRuleSave).toHaveBeenCalledWith(expect.objectContaining({ name: "nightlyIsCheap", policy: "fastLocalFirst" }));
  });

  it("warns on a precedence another of your rules holds, and withholds Check", async () => {
    await openRules();
    fireEvent.click(customLine(/Tagged nightly/));
    fireEvent.change(screen.getByLabelText("Precedence"), { target: { value: "30" } });
    expect(screen.getByText("planningStaysLocal already sits at precedence 30.")).toBeTruthy();
    expect(within(floorBar()).queryByRole("button", { name: "Check it" })).toBeNull();
    // A SHIPPED rule's number is not a collision.
    fireEvent.change(screen.getByLabelText("Precedence"), { target: { value: "45" } });
    expect(screen.queryByText(/already sits at precedence/)).toBeNull();
    expect(within(floorBar()).getByRole("button", { name: "Check it" })).toBeTruthy();
  });

  it("removes a rule after asking, in the bar", async () => {
    await openRules();
    fireEvent.click(customLine(/Tagged nightly/));
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Remove" }));
    expect(floorBar().textContent).toContain("Remove this rule?");
    expect(calls.routingRuleRemove).not.toHaveBeenCalled();
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Remove rule" }));
    await settle();
    expect(calls.routingRuleRemove).toHaveBeenCalledWith({ name: "nightlyIsCheap", expectedRevision: 7 });
  });
});
