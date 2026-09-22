import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { ReactNode } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { chooseOption } from "../selectControl";

// Settings -> Decisions (epic memql#5153, D2).
//
// ===========================================================================
// THE HEADLINE ASSERTION: A FREE CALL NEVER RENDERS A MONEY FIGURE
// ===========================================================================
// A call served by a machine the person owns, or by a subscription they
// already pay for, cost this cluster nothing. `$0.00` says something else --
// that a cost was MEASURED and came out zero -- and it makes a free call and
// a mis-priced metered one identical on the page. That is the whole product
// argument rendered wrong in the one column somebody reads it from.
//
// So the row says "your own machine, no charge" and there is no `$` in it at
// all, and `billing: "unknown"` reads as "not reported" rather than being
// rounded to either side. Zero, free and unmeasured are three answers.
//
// ===========================================================================
// AND THE HALF THAT MAKES A RULE FALSIFIABLE
// ===========================================================================
// `considered` is kept on SUCCESS as well as on a refusal, because "this
// policy had no alternative" and "its alternatives were all shut" look
// identical from the outcome and are the two situations an operator is
// actually trying to tell apart. A chain of one says there was nowhere else
// to look; a longer one says how many doors were tried first.
//
// ===========================================================================
// THE FACETS NARROW THE QUESTION, NOT THE PAGE
// ===========================================================================
// The query is keyset-paged, so filtering a page in the browser would narrow
// the PAGE rather than the search -- a facet that finds nothing the moment
// the cluster gets busy, which is the classic "works on my quiet cluster"
// defect. The assertion is therefore on the argument object the query
// received, and an unset facet must contribute NO KEY: the query's
// `when(args.x)` guards drop an absent argument and its connective as if
// never written, while an empty string is a value and would filter on "".

const h = vi.hoisted(() => {
  const reply = (rows: unknown[]) => ({ rows: () => rows });
  const state = {
    decisions: [] as unknown[],
    decisionsError: null as Error | null,
    calls: [] as Record<string, unknown>[],
    /** Reads this cluster does NOT have. See rules.test.tsx for the argument. */
    missing: new Set<string>(),
  };
  const builtins: Record<string, unknown> = {
    routerDecisionsRecent: vi.fn(async (args: Record<string, unknown>) => {
      state.calls.push(args);
      if (state.decisionsError !== null) throw state.decisionsError;
      return reply(state.decisions);
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
const { DECISIONS_SECTION_RESOURCE } = await import("../../src/apps/settings/DecisionsSection");
const { roleOpens } = await import("../seededAccess");
const { decisionFromRow, levelWords } = await import("../../src/apps/settings/decisionFacts");

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
  });
}

async function renderDecisions(role = "owner") {
  const view = render(
    wrap(<SettingsApp sectionId="decisions" navigate={vi.fn()} askContext={vi.fn()} />, role),
  );
  await flush();
  return view;
}

/** A `v1:router:call` row as the wire hands one over. */
function wireDecision(over: Record<string, unknown> = {}) {
  return {
    id: "v1:router:call:1",
    createdAt: "2026-09-07T10:00:00Z",
    requestId: "req-1",
    promptName: "agentReply",
    level: "strong",
    requestedLevel: "strong",
    servedLevel: "strong",
    degraded: false,
    rule: "operatorReasoning",
    policy: "localFirst",
    door: "local",
    providerName: "fleetQwen",
    vendor: "",
    model: "qwen3-coder",
    considered: [{ entry: "fleet:strongest", door: "local", why: "", served: true }],
    touches: [],
    // A MEASURED ZERO on the wire, deliberately: the row must still not print
    // a money figure. A fixture with the field absent would pass on a surface
    // that renders `$0.0000` for every zero it is handed.
    totalCost: 0,
    totalDurationMs: 1200,
    outcome: "ok",
    billing: "local",
    executionSurface: "",
    ...over,
  };
}

/** The row whose model column reads `model`. */
function rowFor(model: string): HTMLElement {
  const found = [...document.querySelectorAll("li.os-decision-item")].find(
    (li) => li.querySelector(".os-decision-model")?.textContent === model,
  );
  if (!(found instanceof HTMLElement)) throw new Error(`no decision row for ${model}`);
  return found;
}

function costIn(model: string): string {
  return rowFor(model).querySelector(".os-decision-cost")?.textContent ?? "";
}

/** The cost cell's own title -- the long reading, kept for the hover. */
function titleOfCostIn(model: string): string {
  return rowFor(model).querySelector(".os-decision-free")?.getAttribute("title") ?? "";
}

/** The arguments the most recent read was made with. */
function lastArgs(): Record<string, unknown> {
  const last = h.state.calls[h.state.calls.length - 1];
  if (last === undefined) throw new Error("the decisions read was never made");
  return last;
}

beforeEach(() => {
  vi.clearAllMocks();
  h.state.missing = new Set<string>();
  h.state.decisionsError = null;
  h.state.calls = [];
  h.state.decisions = [wireDecision()];
});

describe("Settings -> Decisions: who may see it", () => {
  it("admits an admin, a developer and an owner, and refuses everyone below", () => {
    // `{ min: "admin" }` on this ladder admits admin (200), developer (300)
    // and owner (400) -- developer OUTRANKS admin here, which is the ordering
    // the engine uses and the one the shell reads from the cluster.
    //
    // The floor is admin rather than developer because a decision record
    // carries neither the prompt nor the error message: this list answers
    // "why did that go to a vendor" while holding nothing a reader of it
    // should not see.
    expect(roleOpens("admin", DECISIONS_SECTION_RESOURCE)).toBe(true);
    expect(roleOpens("developer", DECISIONS_SECTION_RESOURCE)).toBe(true);
    expect(roleOpens("owner", DECISIONS_SECTION_RESOURCE)).toBe(true);
    expect(roleOpens("writer", DECISIONS_SECTION_RESOURCE)).toBe(false);
    expect(roleOpens("reader", DECISIONS_SECTION_RESOURCE)).toBe(false);
  });

  it("declares the same requirement in the manifest as in the section", () => {
    const settings = OS_REGISTRY.apps.find((a) => a.id === "settings");
    expect(settings?.sections?.find((s) => s.id === "decisions")?.requires).toBe(
      DECISIONS_SECTION_RESOURCE,
    );
  });
});

describe("Settings -> Decisions: a cluster that records none", () => {
  it("says the CLUSTER does not record them, which is not 'nothing has been routed'", async () => {
    h.state.missing.add("routerDecisionsRecent");
    await renderDecisions();
    expect(screen.getByText(/does not record routing decisions yet/)).toBeTruthy();
    expect(screen.queryByText(/No calls have been routed yet/)).toBeNull();
  });

  it("says nothing has been routed on a cluster that DOES record them", async () => {
    // The control for the line above: the two empty states are different
    // sentences and only one of them is an invitation to wait.
    h.state.decisions = [];
    await renderDecisions();
    expect(screen.getByText(/No calls have been routed yet/)).toBeTruthy();
    expect(screen.queryByText(/does not record routing decisions yet/)).toBeNull();
  });

  it("renders the cluster's own refusal when the read is declined", async () => {
    h.state.decisionsError = new Error("routerDecisionsRecent is admin-only");
    await renderDecisions("developer");
    expect(screen.getByText(/declined this read for developer/)).toBeTruthy();
    expect(screen.getByText("routerDecisionsRecent is admin-only")).toBeTruthy();
  });
});

describe("Settings -> Decisions: what a call cost", () => {
  it("NEVER prints a money figure for a call this cluster was not billed for", async () => {
    h.state.decisions = [
      wireDecision({ id: "d-local", billing: "local", door: "local", model: "qwen3-coder" }),
      wireDecision({
        id: "d-sub",
        billing: "subscription",
        door: "app",
        model: "claude-code",
        executionSurface: "cockpit-app:claude-code",
      }),
    ];
    await renderDecisions();

    // THE CELL SAYS "no charge"; the long form is the title. The door column
    // beside it already says WHICH free door, so repeating "your own machine"
    // here spent the column restating its neighbour and then truncated -- the
    // cost column's job is the money, and for a free call the whole answer is
    // that there is none.
    expect(costIn("qwen3-coder")).toBe("no charge");
    expect(costIn("claude-code")).toBe("no charge");
    // The full reading survives, where there is room for it.
    expect(titleOfCostIn("qwen3-coder")).toBe("your own machine, no charge");
    expect(titleOfCostIn("claude-code")).toBe("your subscription, no charge here");
    // Not anywhere in the row, not just not in the cost cell. `$0.0000` here
    // would claim a measurement was taken, and it would make a free call
    // indistinguishable from a metered one that was priced wrong.
    expect(rowFor("qwen3-coder").textContent).not.toContain("$");
    expect(rowFor("claude-code").textContent).not.toContain("$");
  });

  it("says a cache hit cost nothing, and never prints a money figure for one", async () => {
    // A CACHE ROW'S `billing` IS THE CONCEPT'S DEFAULT, "metered" -- which is
    // not wrong, it says which bucket the call WOULD have fallen into -- so
    // without the cacheKind guard this row printed `$0.0000` and claimed a
    // metered call that cost nothing rather than a call that never happened
    // (memql#5581).
    h.state.decisions = [
      wireDecision({
        id: "d-cache",
        billing: "metered",
        cacheKind: "exact",
        door: "federation",
        vendor: "openai",
        model: "gpt-5.4-mini",
        totalCost: 0,
        inputTokens: 0,
        outputTokens: 0,
      }),
    ];
    await renderDecisions();
    expect(costIn("gpt-5.4-mini")).toBe("from cache");
    expect(titleOfCostIn("gpt-5.4-mini")).toBe("answered from the cache, nothing was sent");
    expect(rowFor("gpt-5.4-mini").textContent).not.toContain("$");
  });

  it("prints the figure for a call that WAS billed", async () => {
    // The control. Without it, "no `$` anywhere" would pass on a column that
    // never renders money at all.
    h.state.decisions = [
      wireDecision({
        billing: "metered",
        door: "federation",
        vendor: "anthropic",
        model: "claude-sonnet-5",
        totalCost: 0.0123,
      }),
    ];
    await renderDecisions();
    expect(costIn("claude-sonnet-5")).toBe("$0.0123");
  });

  it("draws an ABSENCE for a metered call whose cost nobody reported", async () => {
    // The other direction of the same rule: a billed call with no figure is
    // an em dash, not `$0.0000`. Rounding an unmeasured cost to zero is how
    // a metered call comes to look free.
    h.state.decisions = [
      wireDecision({
        billing: "metered",
        door: "federation",
        model: "claude-sonnet-5",
        totalCost: null,
      }),
    ];
    await renderDecisions();
    expect(costIn("claude-sonnet-5")).toBe("—");
    expect(rowFor("claude-sonnet-5").textContent).not.toContain("$");
  });

  it("reads `unknown` as not reported, and rounds it to neither side", async () => {
    // `billing` falls to `unknown` when an app's usage report or a machine's
    // subscription signal is silent, and it is never inferred. Reading it as
    // free understates the bill; reading it as metered invents one.
    h.state.decisions = [
      wireDecision({ billing: "unknown", door: "app", model: "codex", totalCost: 0 }),
    ];
    await renderDecisions();
    expect(costIn("codex")).toBe("not reported");
    expect(rowFor("codex").textContent).not.toContain("$");
    expect(rowFor("codex").textContent).not.toContain("0.00");
  });
});

describe("Settings -> Decisions: what else the chain looked at", () => {
  it("expands in place to the walk, rather than paging away from the list", async () => {
    // DESIGN.md rule 11 is about a DETAIL PAGE. This is four lines under the
    // row -- paging to a route to read three chain entries would lose the
    // reader's place in the list they are scanning.
    h.state.decisions = [
      wireDecision({
        billing: "metered",
        door: "federation",
        model: "claude-sonnet-5",
        rule: "operatorReasoning",
        policy: "localFirst",
        touches: ["library"],
        considered: [
          { entry: "fleet:strongest", door: "local", why: "no model clears the floor", served: false },
          { entry: "app:*", door: "app", why: "nobody signed in", served: false },
          { entry: "federation:anthropic", door: "federation", why: "", served: true },
        ],
      }),
      wireDecision({ id: "d-2", model: "qwen3-coder" }),
    ];
    await renderDecisions();
    const row = rowFor("claude-sonnet-5");
    expect(row.querySelector(".os-decision-detail")).toBeNull();

    fireEvent.click(within(row).getByRole("button"));
    expect(row.querySelector(".os-decision-detail")).not.toBeNull();
    expect(within(row).getByText("Decided by operatorReasoning, which chose localFirst.")).toBeTruthy();
    expect(within(row).getByText("2 earlier doors were looked at first.")).toBeTruthy();

    const walk = within(row).getByRole("list", { name: "What the chain looked at" });
    const entries = within(walk).getAllByRole("listitem");
    expect(entries.map((li) => li.querySelector(".os-mono")?.textContent)).toEqual([
      "fleet:strongest",
      "app:*",
      "federation:anthropic",
    ]);
    // Why each was passed over is the half that makes the row evidence: the
    // outcome alone cannot tell "no alternative" from "every alternative shut".
    expect(entries.map((li) => li.querySelector(".os-decision-walk-why")?.textContent)).toEqual([
      "no model clears the floor",
      "nobody signed in",
      "served this call",
    ]);
    expect(entries[2]?.getAttribute("data-os-served")).toBe("true");

    // Still a list: the other row is where it was, and unexpanded.
    expect(rowFor("qwen3-coder").querySelector(".os-decision-detail")).toBeNull();
  });

  it("says a chain of ONE had nowhere else to look", async () => {
    // "There was nothing to try" and "everything else was shut" are the two
    // stories a vendor call can have, and they are the question this list
    // exists to answer.
    await renderDecisions();
    const row = rowFor("qwen3-coder");
    fireEvent.click(within(row).getByRole("button"));
    expect(
      within(row).getByText("There was nothing else in the chain, so this was the only place to look."),
    ).toBeTruthy();
    expect(within(row).queryByText(/earlier door/)).toBeNull();
  });

  it("says so when the cluster recorded no walk at all", async () => {
    // An empty `considered` is not "nothing was tried" -- it is "nobody wrote
    // it down", and inventing a chain of one from it would be a claim.
    h.state.decisions = [wireDecision({ model: "qwen3-coder", considered: [] })];
    await renderDecisions();
    const row = rowFor("qwen3-coder");
    fireEvent.click(within(row).getByRole("button"));
    expect(within(row).getByText("The cluster did not record what else it looked at.")).toBeTruthy();
    expect(within(row).queryByRole("list", { name: "What the chain looked at" })).toBeNull();
  });
});

describe("Settings -> Decisions: the facets narrow the QUESTION", () => {
  it("sends no key for a facet nobody set, and sends the one that was", async () => {
    await renderDecisions();
    // An empty facet is not a filter on "": the query's `when(args.x)` guards
    // drop an absent argument as if never written, and an empty string is a
    // value that would match nothing.
    expect(h.state.calls).toHaveLength(1);
    expect(lastArgs()).toEqual({});

    fireEvent.click(screen.getByRole("button", { name: "Refine decisions" }));
    chooseOption(screen.getByLabelText("Door"), "A paid vendor");
    await flush();

    expect(Object.keys(lastArgs())).toEqual(["door"]);
    expect(lastArgs()).toEqual({ door: "federation" });
    // The narrowed question went to the CLUSTER, so a busy cluster's page 1
    // is not what got filtered.
    expect(h.state.calls.length).toBeGreaterThan(1);
  });

  it("sends every facet that is set, and drops one that is cleared again", async () => {
    await renderDecisions();
    fireEvent.click(screen.getByRole("button", { name: "Refine decisions" }));
    chooseOption(screen.getByLabelText("Level"), "reasoning");
    await flush();
    chooseOption(screen.getByLabelText("Outcome"), "Failed");
    await flush();
    expect(lastArgs()).toEqual({ level: "reasoning", outcome: "error" });

    fireEvent.click(screen.getByRole("button", { name: "Remove reasoning" }));
    await flush();
    expect(Object.keys(lastArgs())).toEqual(["outcome"]);
    expect("level" in lastArgs()).toBe(false);
  });
});

describe("what a decision says about its level", () => {
  // Pure, so the distinction is pinned without a render: a row that was not
  // degraded says ONE level, because "strong -> strong" on every row is a
  // column of noise that hides the handful where the two differ -- and those
  // are the only rows the field exists for.
  const read = (over: Record<string, unknown> = {}) =>
    decisionFromRow(wireDecision(over) as unknown as Row);

  it("says one level on an ordinary row", () => {
    expect(levelWords(read())).toBe("strong");
  });

  it("says BOTH when the call was degraded", () => {
    expect(levelWords(read({ degraded: true, requestedLevel: "reasoning", servedLevel: "strong" }))).toBe(
      "reasoning served as strong",
    );
  });

  it("says one level when the flag is set but nothing actually stepped down", () => {
    expect(levelWords(read({ degraded: true, requestedLevel: "strong", servedLevel: "strong" }))).toBe(
      "strong",
    );
  });

  it("marks the degraded row on the page too", async () => {
    h.state.decisions = [
      wireDecision({
        model: "qwen3-coder",
        degraded: true,
        requestedLevel: "reasoning",
        servedLevel: "strong",
      }),
    ];
    await renderDecisions();
    const level = rowFor("qwen3-coder").querySelector(".os-decision-level");
    expect(level?.textContent).toContain("reasoning served as strong");
    expect(level?.textContent).toContain("degraded");
  });
});
