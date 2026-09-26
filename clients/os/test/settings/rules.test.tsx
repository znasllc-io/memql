import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { ReactNode } from "react";

// Settings -> Rules (epic memql#5153, D1).
//
// ===========================================================================
// WHAT THIS SUITE PROTECTS
// ===========================================================================
// Three properties, and each of them is a place where a correct-looking
// screen would be a lie.
//
// AN ACT THAT IS NOT LEGAL IS ABSENT (DESIGN.md rule 12). The engine refuses
// to retire a shipped rule BY NAME, so an Edit or a Remove on one could only
// ever produce a refusal at the end of a flow somebody had already invested
// in. A disabled control would still advertise that the act exists, so the
// assertions here are `queryBy` returning null and a COUNT of the Remove
// controls -- a greyed-out button passes "the click did nothing" and fails
// what this is actually about.
//
// ACTIVATE IS NOT REACHABLE UNTIL SOMETHING HAS BEEN CHECKED. The panel's
// three states exist so that a rule is compiled, then simulated against what
// the cluster has actually done, and only then armed. A simulation belongs to
// ONE exact rule, so an edit underneath it withdraws Activate again -- without
// that, somebody activates a rule on the strength of a check of a different
// one.
//
// THE FORM SENDS ONLY THE CONDITIONS THAT WERE TICKED. This is the same
// property `rulesFacts.test.ts` pins on `whenObjectFrom`, asserted here at the
// other end of the wire: the object that actually reaches
// `routingRuleActivate`. The pure test proves the function is right; this one
// proves the form is the only path to it.

const h = vi.hoisted(() => {
  const reply = (rows: unknown[]) => ({ rows: () => rows });
  const state = {
    rules: [] as unknown[],
    rulesError: null as Error | null,
    describeReply: {} as Record<string, unknown>,
    simulateReply: {} as Record<string, unknown>,
    activateError: null as Error | null,
    describeCalls: [] as Record<string, unknown>[],
    simulateCalls: [] as Record<string, unknown>[],
    activateCalls: [] as Record<string, unknown>[],
    retireCalls: [] as Record<string, unknown>[],
    /**
     * Builtins this cluster does NOT have.
     *
     * Every rules read and write lands with epic memql#5127, and on a cluster
     * without it the generated client simply has no such METHOD -- which is
     * what the surface probes for. So an unsupported cluster is modelled by
     * making the property absent rather than by making the call fail: a call
     * that rejects is a cluster that HAS the builtin and refused, which is a
     * different sentence on the page.
     */
    missing: new Set<string>(),
  };
  const builtins: Record<string, unknown> = {
    routingRules: vi.fn(async () => {
      if (state.rulesError !== null) throw state.rulesError;
      return reply(state.rules);
    }),
    routingRuleDescribe: vi.fn(async (args: Record<string, unknown>) => {
      state.describeCalls.push(args);
      return reply([state.describeReply]);
    }),
    routingRuleSimulate: vi.fn(async (args: Record<string, unknown>) => {
      state.simulateCalls.push(args);
      return reply([state.simulateReply]);
    }),
    routingRuleActivate: vi.fn(async (args: Record<string, unknown>) => {
      state.activateCalls.push(args);
      if (state.activateError !== null) throw state.activateError;
      return reply([{ message: "active" }]);
    }),
    routingRuleRetire: vi.fn(async (args: Record<string, unknown>) => {
      state.retireCalls.push(args);
      return reply([{ message: "retired" }]);
    }),
  };
  const query = new Proxy(builtins, {
    get: (target, prop) =>
      typeof prop === "string" && state.missing.has(prop) ? undefined : Reflect.get(target, prop),
  });
  const connection = {
    nodeId: "bff-test",
    engineVersion: "v9.9.9",
    engineCommit: "abcdef123456",
    subscriptions: null,
    dispatcher: null,
    query,
    onStatusChange: () => () => {},
  };
  return { connection, state };
});

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { SessionProvider } = await import("../../src/chrome/access");
const { OsProvider } = await import("../../src/chrome/state");
const { OS_REGISTRY } = await import("../../src/apps/registry");
const { SettingsApp } = await import("../../src/apps/settings/SettingsApp");
const { LocalDesktopStore } = await import("../../src/system/store");
const { UNKNOWN_RUNTIME_CONFIG } = await import("../../src/cluster/config");
const { RULES_SECTION_RESOURCE } = await import("../../src/apps/settings/RulesSection");
const { roleOpens } = await import("../seededAccess");

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

function wrap(children: ReactNode, role: string) {
  return (
    <SessionProvider
      value={{
        access: { userId: "u-1", primaryEmail: "owner@example.com", role: role, roleName: "", rank: 0 },
        config: {
          ...UNKNOWN_RUNTIME_CONFIG,
          domain: "example.com",
          identityUrl: "https://identity.example.com",
        },
      }}
    >
      <OsProvider
        registry={OS_REGISTRY}
        actorRole={role}
        grid={{ cols: 12, rows: 8 }}
        store={new LocalDesktopStore(memStorage())}
      >
        {children}
      </OsProvider>
    </SessionProvider>
  );
}

async function flush() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
}

async function renderRules(role = "owner") {
  const view = render(
    wrap(<SettingsApp sectionId="rules" navigate={vi.fn()} askContext={vi.fn()} />, role),
  );
  await flush();
  return view;
}

/** A rule row as the wire hands one over. */
function wireRule(over: Record<string, unknown>) {
  return {
    name: "aRule",
    when: {},
    level: "",
    policy: "localFirst",
    precedence: 10,
    onUnavailable: "degrade",
    excludes: [],
    locked: false,
    described: "",
    ...over,
  };
}

/** The list, which only exists once there is a rule in it. */
function ruleList(): HTMLElement {
  return screen.getByRole("list", { name: "Rules, in the order they are tried" });
}

function ruleRows(): HTMLElement[] {
  return within(ruleList()).getAllByRole("listitem");
}

function ruleRow(name: string): HTMLElement {
  const found = ruleRows().find(
    (li) => li.querySelector(".os-row-name")?.textContent === name,
  );
  if (found === undefined) throw new Error(`no rule row named ${name}`);
  return found;
}

/** Open the fields editor on an existing custom rule. */
function editAsFields(name: string): HTMLElement {
  fireEvent.click(within(ruleRow(name)).getByRole("button", { name: `Edit ${name} as fields` }));
  return screen.getByRole("region", { name: "Rule fields" });
}

beforeEach(() => {
  // PER TEST, because vitest isolates modules per FILE and not per test: the
  // spies are module state, so a click in one case is still recorded when the
  // next one asks whether anything was called.
  vi.clearAllMocks();
  h.state.missing = new Set<string>();
  h.state.rulesError = null;
  h.state.activateError = null;
  h.state.describeCalls = [];
  h.state.simulateCalls = [];
  h.state.activateCalls = [];
  h.state.retireCalls = [];
  h.state.simulateReply = { considered: 400, changed: 12 };
  h.state.describeReply = {
    name: "planningIsLocal",
    when: { level: "reasoning" },
    level: "",
    policy: "localOnly",
    precedence: 20,
    onUnavailable: "degrade",
    problem: "",
  };
  // A shipped set with a FLOOR in it, and two rules of the reader's own. The
  // shipped rule sits at precedence 5 and the custom ones at 30 and 10, so
  // "locked first" is what puts it on top rather than its number -- a plain
  // precedence sort gives the opposite answer, which is what makes the order
  // assertion discriminating.
  h.state.rules = [
    wireRule({ name: "nightlyIsCheap", precedence: 10, described: "keep the nightly run off a vendor" }),
    wireRule({
      name: "defaultRule",
      when: {},
      precedence: 0,
      locked: true,
      policy: "localFirst",
    }),
    wireRule({
      name: "planningStaysLocal",
      when: { level: "reasoning" },
      policy: "localOnly",
      precedence: 30,
      onUnavailable: "park",
    }),
    wireRule({
      name: "operatorReasoning",
      when: { prompt: "agentReply", role: "operator" },
      level: "reasoning",
      policy: "localFirst",
      precedence: 5,
      locked: true,
    }),
  ];
});

describe("Settings -> Rules: who may see it", () => {
  it("admits an owner and a developer, and refuses an admin", () => {
    // A SET rather than a floor, for the reason Doors gives: the ladder puts
    // admin (200) BELOW developer (300), so `{ min: "developer" }` would admit
    // admin -- whose concern is user administration, not what the cluster
    // talks to.
    expect(roleOpens("owner", RULES_SECTION_RESOURCE)).toBe(true);
    expect(roleOpens("developer", RULES_SECTION_RESOURCE)).toBe(true);
    expect(roleOpens("admin", RULES_SECTION_RESOURCE)).toBe(false);
    expect(roleOpens("writer", RULES_SECTION_RESOURCE)).toBe(false);
    expect(roleOpens("reader", RULES_SECTION_RESOURCE)).toBe(false);
  });

  it("declares the same requirement in the manifest as in the section", () => {
    // Two copies of a gate is how they drift.
    const settings = OS_REGISTRY.apps.find((a) => a.id === "settings");
    expect(settings?.sections?.find((s) => s.id === "rules")?.requires).toBe(RULES_SECTION_RESOURCE);
  });
});

describe("Settings -> Rules: a cluster that does not route by rules", () => {
  it("says the CLUSTER cannot, which is not 'you have no rules'", async () => {
    // The two read identically as an empty list and mean opposite things:
    // one is a cluster to upgrade, the other an invitation to write a rule.
    h.state.missing.add("routingRules");
    h.state.missing.add("routingRuleActivate");
    await renderRules();
    expect(screen.getByText(/does not route by rules yet/)).toBeTruthy();
    expect(screen.queryByText(/No rules yet/)).toBeNull();
    // And the act is absent, not offered and then refused at the last step.
    expect(screen.queryByRole("button", { name: "Describe a rule" })).toBeNull();
  });

  it("offers the act on a cluster that DOES -- the control on the line above", async () => {
    await renderRules();
    expect(screen.getByRole("button", { name: "Describe a rule" })).toBeTruthy();
    expect(screen.queryByText(/does not route by rules yet/)).toBeNull();
  });

  it("renders the cluster's own refusal when the read is declined", async () => {
    h.state.rulesError = new Error("routingRules is owner-only");
    await renderRules("developer");
    expect(screen.getByText(/declined this read for developer/)).toBeTruthy();
    expect(screen.getByText("routingRules is owner-only")).toBeTruthy();
  });
});

describe("Settings -> Rules: the list", () => {
  it("counts filtered records beside the title and uses the shared list rows", async () => {
    await renderRules();
    const heading = screen.getByRole("heading", { name: "Rules" }).parentElement!;
    expect(heading.querySelector(".os-head-meta")?.textContent).toBe("4");
    expect(ruleList().classList.contains("os-record-list")).toBe(true);
    expect(ruleList().querySelectorAll(".os-record-row")).toHaveLength(4);
    fireEvent.click(screen.getByRole("button", { name: /Refine/ }));
    fireEvent.change(screen.getByRole("textbox", { name: "Search" }), { target: { value: "planningStaysLocal" } });
    expect(heading.querySelector(".os-head-meta")?.textContent).toBe("1");
    expect(ruleList().querySelectorAll(".os-record-row")).toHaveLength(1);
    fireEvent.change(screen.getByRole("textbox", { name: "Search" }), { target: { value: "no such rule" } });
    expect(heading.querySelector(".os-head-meta")?.textContent).toBe("0");
  });

  it("renders the rules in the order the engine tries them, with the number that decides it", async () => {
    await renderRules();
    expect(ruleRows().map((li) => li.querySelector(".os-row-name")?.textContent)).toEqual([
      "operatorReasoning",
      "planningStaysLocal",
      "nightlyIsCheap",
      "defaultRule",
    ]);
    // The precedences are NOT in descending order, which is the point: locked
    // first and the floor last are what the order is actually made of.
    expect(ruleRows().map((li) => li.querySelector(".os-record-icon")?.textContent)).toEqual([
      "5",
      "30",
      "10",
      "0",
    ]);
  });

  it("writes each rule as a claim somebody can agree with, not a row of fields", async () => {
    await renderRules();
    expect(ruleRow("planningStaysLocal").querySelector(".os-record-secondary")?.textContent).toBe(
      "When the level asked for is reasoning, try localOnly. If nothing there is available, park and wait for a person.",
    );
  });

  it("marks a shipped rule and offers NO act on it at all", async () => {
    await renderRules();
    const shipped = ruleRow("operatorReasoning");
    expect(within(shipped).getByText("shipped")).toBeTruthy();
    // ABSENT, not disabled: the engine refuses to retire it by name, so a
    // greyed-out control would advertise an act that does not exist.
    expect(
      within(shipped).queryByRole("button", { name: "Edit operatorReasoning as fields" }),
    ).toBeNull();
    expect(within(shipped).queryByRole("button", { name: "Remove operatorReasoning" })).toBeNull();
    expect(within(shipped).queryAllByRole("button")).toHaveLength(0);

    // The count is what makes this a claim about the whole list rather than
    // about one row: exactly one Remove per rule the person actually owns.
    const unlocked = ["planningStaysLocal", "nightlyIsCheap"];
    expect(within(ruleList()).getAllByRole("button", { name: /^Remove / })).toHaveLength(
      unlocked.length,
    );
    for (const name of unlocked) {
      expect(within(ruleRow(name)).getByRole("button", { name: `Remove ${name}` })).toBeTruthy();
    }
  });

  it("says in the head how to get past a shipped rule", async () => {
    // A lock glyph says "you cannot edit this" and nothing else, so somebody
    // works the rest out by meeting a refusal. The affordance is said in words
    // instead, before anybody clicks anything.
    await renderRules();
    expect(screen.getByText(/matching shipped rule takes priority/)).toBeTruthy();
    expect(screen.getByText(/come back on every restart/)).toBeTruthy();
  });

  it("says what the FLOOR is, so its place at the bottom does not read as a sorting bug", async () => {
    await renderRules();
    const floor = ruleRow("defaultRule");
    expect(within(floor).getByText("the floor")).toBeTruthy();
    expect(floor.textContent).toMatch(/never fall through/);
    // Locked, so still no acts -- the exemption is about ORDER, not authority.
    expect(within(floor).queryAllByRole("button")).toHaveLength(0);
  });
});

describe("Settings -> Rules: describing one", () => {
  async function openDescribe() {
    await renderRules();
    fireEvent.click(screen.getByRole("button", { name: "Describe a rule" }));
    return screen.getByLabelText("Describe a rule in your own words") as HTMLTextAreaElement;
  }

  it("offers nothing at all while the box is empty", async () => {
    await openDescribe();
    expect(screen.queryByRole("button", { name: "Compile it" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Check it" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Activate" })).toBeNull();
  });

  it("offers Check after compiling, and Activate only after a simulation", async () => {
    const box = await openDescribe();

    fireEvent.change(box, { target: { value: "send planning work to my own machines" } });
    expect(screen.getByRole("button", { name: "Compile it" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Activate" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Compile it" }));
    await flush();
    // The compiled rule comes back as a SENTENCE in the same words the list
    // uses -- the question a person can answer is "is this what I meant?",
    // not "is this the right value in field four". Read off the panel rather
    // than the document, because the list underneath is written in exactly
    // the same form, which is the property being asserted.
    const panel = screen.getByRole("region", { name: "Describe a rule" });
    expect(panel.querySelector(".os-rule-compiled-said")?.textContent).toBe(
      "When the level asked for is reasoning, try localOnly. If nothing there is available, step down a level.",
    );
    expect(screen.getByRole("button", { name: "Check it" })).toBeTruthy();
    // ABSENT, never disabled. A greyed-out Activate advertises that activating
    // without checking is a thing you can do from here.
    expect(screen.queryByRole("button", { name: "Activate" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Check it" }));
    await flush();
    expect(screen.getByText("Over the last 400 decisions this would have changed 12.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Activate" })).toBeTruthy();
    expect(h.state.activateCalls).toEqual([]);
  });

  it("withdraws Activate the moment the sentence changes underneath it", async () => {
    const box = await openDescribe();
    fireEvent.change(box, { target: { value: "send planning work to my own machines" } });
    fireEvent.click(screen.getByRole("button", { name: "Compile it" }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Check it" }));
    await flush();
    expect(screen.getByRole("button", { name: "Activate" })).toBeTruthy();

    // A simulation belongs to ONE exact rule. Left standing, it would arm an
    // Activate for a rule nothing was ever checked against.
    fireEvent.change(box, { target: { value: "send planning work anywhere at all" } });
    expect(screen.queryByRole("button", { name: "Activate" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Check it" })).toBeNull();
    expect(screen.getByRole("button", { name: "Compile it" })).toBeTruthy();
    expect(h.state.activateCalls).toEqual([]);
  });

  it("renders a compiler refusal verbatim and leaves the person's words in the box", async () => {
    // The compiler knows which clause it could not read; this panel does not.
    // And clearing the field throws away the thing they are in the middle of
    // fixing.
    const refusal = "I could not read \"never to a paid vendor\" as a policy name.";
    h.state.describeReply = { problem: refusal };
    const box = await openDescribe();
    const words = "send planning work to my own machines, never to a paid vendor";
    fireEvent.change(box, { target: { value: words } });
    fireEvent.click(screen.getByRole("button", { name: "Compile it" }));
    await flush();

    expect(screen.getByText(refusal)).toBeTruthy();
    expect((screen.getByLabelText("Describe a rule in your own words") as HTMLTextAreaElement).value).toBe(
      words,
    );
    expect(screen.queryByRole("button", { name: "Check it" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Activate" })).toBeNull();
  });

  it("sends the compiled rule, with the person's own words attached to it", async () => {
    const box = await openDescribe();
    const words = "send planning work to my own machines";
    fireEvent.change(box, { target: { value: words } });
    fireEvent.click(screen.getByRole("button", { name: "Compile it" }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Check it" }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Activate" }));
    await flush();

    expect(h.state.activateCalls).toHaveLength(1);
    expect(h.state.activateCalls[0]).toEqual({
      name: "planningIsLocal",
      when: { level: "reasoning" },
      level: "",
      policy: "localOnly",
      precedence: 20,
      onUnavailable: "degrade",
      description: words,
      excludes: [],
    });
  });
});

describe("Settings -> Rules: the fields editor", () => {
  it("sends the ONE condition that was ticked, and no key for the six that were not", async () => {
    // The defect this refuses: seven boxes, all seven sent, so every field the
    // person left alone arrives as a condition matching only "" -- and the
    // rule silently never fires while reading correctly in the list.
    await renderRules();
    const panel = editAsFields("nightlyIsCheap");

    fireEvent.click(within(panel).getByRole("checkbox", { name: "the level asked for" }));
    fireEvent.change(within(panel).getByLabelText("Value for the level asked for"), {
      target: { value: "reasoning" },
    });
    fireEvent.click(within(panel).getByRole("button", { name: "Check it" }));
    await flush();
    fireEvent.click(within(panel).getByRole("button", { name: "Activate" }));
    await flush();

    expect(h.state.activateCalls).toHaveLength(1);
    const when = h.state.activateCalls[0]?.["when"] as Record<string, string>;
    expect(Object.keys(when)).toEqual(["level"]);
    expect(when).toEqual({ level: "reasoning" });
    for (const key of ["modality", "prompt", "role", "actorRole", "tag", "touches"]) {
      expect(key in when).toBe(false);
    }
  });

  it("offers a value box only for a condition that is ticked", async () => {
    await renderRules();
    const panel = editAsFields("nightlyIsCheap");
    expect(within(panel).queryByLabelText("Value for the call's tag")).toBeNull();
    fireEvent.click(within(panel).getByRole("checkbox", { name: "the call's tag" }));
    expect(within(panel).getByLabelText("Value for the call's tag")).toBeTruthy();
    // Unticking takes the condition away again, not just its value.
    fireEvent.click(within(panel).getByRole("checkbox", { name: "the call's tag" }));
    expect(within(panel).queryByLabelText("Value for the call's tag")).toBeNull();
  });

  it("opens on the rule's OWN conditions, so editing one shows the rule that is running", async () => {
    await renderRules();
    const panel = editAsFields("planningStaysLocal");
    expect(
      (within(panel).getByRole("checkbox", { name: "the level asked for" }) as HTMLInputElement)
        .checked,
    ).toBe(true);
    expect(
      (within(panel).getByRole("checkbox", { name: "the call's tag" }) as HTMLInputElement).checked,
    ).toBe(false);
    expect(
      (within(panel).getByLabelText("Value for the level asked for") as HTMLInputElement).value,
    ).toBe("reasoning");
  });

  it("warns on a precedence another rule already holds, and withholds the act", async () => {
    await renderRules();
    const panel = editAsFields("nightlyIsCheap");
    fireEvent.change(within(panel).getByLabelText("Precedence"), { target: { value: "30" } });

    expect(within(panel).getByText("planningStaysLocal already sits at precedence 30.")).toBeTruthy();
    // Two rules at one precedence are resolved by nothing, so replicas could
    // disagree about which wins. There is nothing to check and nothing to arm.
    expect(within(panel).queryByRole("button", { name: "Check it" })).toBeNull();
    expect(within(panel).queryByRole("button", { name: "Activate" })).toBeNull();
    expect(h.state.simulateCalls).toEqual([]);

    // A SHIPPED rule's number is not a collision: shipped rules run in their
    // own partition and do not compete for a slot. Without this control the
    // assertion above would pass on a check that refuses every number.
    fireEvent.change(within(panel).getByLabelText("Precedence"), { target: { value: "5" } });
    expect(within(panel).queryByText(/already sits at precedence/)).toBeNull();
    expect(within(panel).getByRole("button", { name: "Check it" })).toBeTruthy();
  });
});
