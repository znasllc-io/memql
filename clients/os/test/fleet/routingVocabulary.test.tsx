import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ReactNode } from "react";

// ONE VOCABULARY (routing design brief, section 1): Source, Route, Rule,
// Level. The engine's words -- policy, door, task rule, federation, lane --
// stay in ids, query names and the DSL, and must not reach a person on a
// routing surface.
//
// The trap is that they arrive through DATA as much as through copy: the
// shipped route `federationStrongest`, the shipped rules `fastLane` and
// `backgroundLane`. So this sweep walks every routing page -- each tab, a
// route's page, both wizards at every step, the describe page, a rule's page,
// an opened history row -- over the SHIPPED routes and rules, and reads
// everything a person can perceive: the text, and every accessible name and
// hover title.

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { renderRouting, routingConnection, routeRow, ruleRow, settle, shippedRoutes, shippedRules } = await import("./routingHarness");
const { retiredWordsIn } = await import("../../src/apps/fleet/routing/vocabulary");
const { SessionProvider } = await import("../../src/chrome/access");
const { OsProvider } = await import("../../src/chrome/state");
const { OS_REGISTRY } = await import("../../src/apps/registry");
const { SettingsApp } = await import("../../src/apps/settings/SettingsApp");
const { UNKNOWN_RUNTIME_CONFIG } = await import("../../src/cluster/config");
const { installSeededAccess } = await import("../seededAccess");
const { shippedRulesFromDsl } = await import("./shippedRulesDsl");

afterEach(cleanup);

/**
 * Everything a person can perceive on the page, one string per source.
 *
 * PER TEXT NODE, not the page's textContent: that glues neighbouring elements
 * together ("Save policy" beside "x" reads "Save policyx"), and a retired word
 * glued to the next element's text would slip past a word boundary.
 */
function perceived(root: ParentNode = document.body): string[] {
  const out: string[] = [];
  const walker = document.createTreeWalker(root instanceof Node ? root : document.body, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    if (node.textContent) out.push(node.textContent);
  }
  for (const el of root.querySelectorAll("[aria-label], [title], [placeholder], [aria-description]")) {
    for (const attr of ["aria-label", "title", "placeholder", "aria-description"]) {
      const value = el.getAttribute(attr);
      if (value) out.push(value);
    }
  }
  return out;
}

function retired(root?: ParentNode): string[] {
  const found = new Map<string, string>();
  for (const text of perceived(root)) {
    for (const word of retiredWordsIn(text)) {
      const at = text.toLowerCase().indexOf(word.split(" ")[0]!);
      found.set(word, text.slice(Math.max(0, at - 40), at + 40));
    }
  }
  return [...found.entries()].map(([word, where]) => `${word}: ...${where}...`);
}

function floorBar(): HTMLElement {
  return screen.getByRole("group", { name: "What you can do with this" });
}

// The SHIPPED rules as the DSL declares them -- all of them, read from
// dsl/rules/rules.memql rather than from a literal that could leave out the
// very rule whose words leak (it did: `policyCompilerLocalOnly` matches the
// prompt `composeRoutingPolicy`).
function use() {
  const made = routingConnection({
    routes: [...shippedRoutes().map((r) => (r.name === "localFirst" ? routeRow("localFirst", "fleet:strongest", ["app:*", "federation:cheapest", "policy:federationStrongest"], { customized: true }) : r))],
    rules: [...shippedRulesFromDsl(), ruleRow({ name: "nightlyIsCheap", when: { tag: "nightly" }, policy: "federationStrongest", precedence: 10, onUnavailable: "park" })],
    decisions: [
      {
        id: "d-0",
        createdAt: "2026-09-28T10:01:00Z",
        promptName: "composeRoutingPolicy",
        level: "strong",
        rule: "policyCompilerLocalOnly",
        policy: "localOnly",
        door: "local",
        model: "qwen3.8:27b",
        outcome: "ok",
        billing: "local",
        considered: [{ entry: "fleet:strongest", door: "local", why: "", served: true }],
      },
      {
        id: "d-1",
        createdAt: "2026-09-28T10:00:00Z",
        promptName: "agentReply",
        level: "fast",
        rule: "fastLane",
        policy: "fastLocalFirst",
        door: "federation",
        model: "claude-sonnet-5",
        outcome: "ok",
        billing: "metered",
        totalCost: 0.01,
        considered: [
          { entry: "fleet:fastest", door: "local", why: "no model", served: false },
          { entry: "federation:cheapest", door: "federation", why: "", served: true },
        ],
      },
    ],
  });
  h.connection = made.connection;
}

describe("the routing surfaces speak Source, Route, Rule and Level", () => {
  it("fires on a retired word in text, in a name or in a title (the control)", () => {
    // Without this, a sweep that read nothing would pass every case below.
    render(<div><span>Save policy</span><button type="button" aria-label="fastLane" /><span title="Doors">x</span></div>);
    expect(retired().map((line) => line.split(":")[0])).toEqual(["policy", "lane", "door"]);
  });

  it("on the route list and a route's page", async () => {
    use();
    await renderRouting("owner");
    expect(retired()).toEqual([]);
    fireEvent.click(screen.getByRole("button", { name: /^Open Local first,/ }));
    await settle();
    // A route inside this one, whose engine id carries a retired word.
    expect(screen.getByRole("button", { name: /^4\. Local, then best vendor,/ })).toBeTruthy();
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Restore shipped" }));
    expect(retired()).toEqual([]);
  });

  it("through every step of New route", async () => {
    use();
    await renderRouting("owner");
    fireEvent.click(screen.getByRole("button", { name: "New route" }));
    expect(retired()).toEqual([]);
    fireEvent.change(screen.getByLabelText("Route name"), { target: { value: "Night shift" } });
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Next" }));
    fireEvent.click(within(screen.getByLabelText("Sources you can add")).getByRole("button", { name: /^Local, then best vendor:/ }));
    expect(retired()).toEqual([]);
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Next" }));
    expect(retired()).toEqual([]);
  });

  it("on the rules, the Add wizard, the describe page and a rule's page", async () => {
    use();
    await renderRouting("owner", { intent: { id: "r", payload: { routingTab: "rules" } } });
    // `fastLane` and `backgroundLane` are shipped rule NAMES; the rows are words.
    expect(retired()).toEqual([]);
    fireEvent.click(screen.getByRole("button", { name: "Add a rule" }));
    expect(retired()).toEqual([]);
    fireEvent.click(screen.getByRole("radio", { name: /^Reasoning work/ }));
    expect(retired()).toEqual([]);
    fireEvent.click(screen.getByRole("radio", { name: /Local, then best vendor/ }));
    expect(retired()).toEqual([]);
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Cancel" }));
    fireEvent.click(screen.getByRole("button", { name: "Add a rule" }));
    fireEvent.click(screen.getByRole("button", { name: "Describe it instead" }));
    fireEvent.change(screen.getByLabelText("Describe a rule in your own words"), { target: { value: "keep planning local" } });
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Compile it" }));
    await settle();
    expect(retired()).toEqual([]);
    fireEvent.click(within(floorBar()).getByRole("button", { name: "Cancel" }));
    fireEvent.click(screen.getByRole("button", { name: /Tagged nightly/ }));
    expect(retired()).toEqual([]);
  });

  it("on Machines and History, with a call opened", async () => {
    use();
    await renderRouting("owner", { intent: { id: "m", payload: { routingTab: "machines" } } });
    expect(retired()).toEqual([]);
    cleanup();
    use();
    await renderRouting("owner", { intent: { id: "h", payload: { routingTab: "history" } } });
    await settle();
    const rows = within(screen.getByRole("list", { name: "Recent routed calls" })).getAllByRole("button");
    // `composeRoutingPolicy`, decided by `policyCompilerLocalOnly`.
    fireEvent.click(rows[0]!);
    expect(screen.getByText(/Decided by Route composing prompt/)).toBeTruthy();
    expect(retired()).toEqual([]);
    // The call was decided by `fastLane`; the row says "Fast work".
    fireEvent.click(rows[1]!);
    expect(screen.getByText(/Decided by Fast work/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Refine history" }));
    expect(retired()).toEqual([]);
  });
});

describe("the shipped rules the sweep reads are the ones the engine ships", () => {
  it("reads all of dsl/rules/rules.memql", () => {
    const names = shippedRulesFromDsl().map((r) => r.name);
    expect(names).toContain("policyCompilerLocalOnly");
    expect(names).toContain("operatorReasoning");
    expect(names.length).toBeGreaterThanOrEqual(9);
  });

  it("holds the fixtures' literal copy (shared with the QA harness) to the DSL", () => {
    const shape = (r: Record<string, unknown>) => ({ name: r.name, when: r.when, level: r.level ?? "", policy: r.policy, precedence: r.precedence, onUnavailable: r.onUnavailable, locked: r.locked });
    const byName = (a: { name: unknown }, b: { name: unknown }) => String(a.name).localeCompare(String(b.name));
    expect(shippedRules().map(shape).sort(byName)).toEqual(shippedRulesFromDsl().map(shape).sort(byName));
  });
});

describe("Settings keeps the vendors and the levels, in the same words", () => {
  function wrap(children: ReactNode) {
    return (
      <SessionProvider value={{ access: { userId: "u-1", primaryEmail: "o@example.com", role: "owner", roleName: "", rank: 0 }, config: { ...UNKNOWN_RUNTIME_CONFIG, domain: "example.com" }, ladderLoaded: true }}>
        <OsProvider registry={OS_REGISTRY} actorRole="owner" grid={{ cols: 12, rows: 8 }} layout="desktop">{children}</OsProvider>
      </SessionProvider>
    );
  }

  it.each(["providers", "levels", "rules", "decisions"])("Settings > %s", async (sectionId) => {
    use();
    installSeededAccess("owner");
    const conn = h.connection as { query: Record<string, unknown>; onStatusChange?: unknown };
    conn.query.providerAuthStatus = vi.fn(async () => ({ rows: () => [] }));
    // Settings' connection history listens to the socket's status.
    conn.onStatusChange = () => () => {};
    render(wrap(<SettingsApp sectionId={sectionId} navigate={vi.fn()} askContext={vi.fn()} />));
    await settle();
    // Vendors keeps the vendors' own field names ("Federation rule id" is
    // Anthropic's console term, copied from there), so it is swept for the
    // routing words only; the rest are swept whole.
    const found = retired().filter((line) => sectionId !== "providers" || !line.startsWith("federation"));
    expect(found).toEqual([]);
    if (sectionId !== "providers") {
      // One quiet link, and it goes to Fleet.
      expect(screen.getAllByRole("button", { name: "Routing is in Fleet" })).toHaveLength(1);
    }
  });
});
