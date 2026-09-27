import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { NexusApp } = await import("../../src/apps/nexus/NexusApp");
const { LocalNexusSettingsStore } = await import("../../src/apps/nexus/settings");
const { reuseFactsFromRow } = await import("../../src/apps/nexus/reuse");
const { constructRow, fakeConnection, ladderPolicyRow, procedureRow, withSession } = await import("./harness");
const { CONSTRUCT_CONCEPT } = await import("../../src/apps/nexus/concepts");
const { chooseOption } = await import("../selectControl");

type Conn = ReturnType<typeof fakeConnection>;

// WHAT AN AUTOMATION IS FOR (epic memql#5414, design D24): the evidence's
// label, a person's own over it, and the list narrowed by either. The tests
// are about the three ways that goes wrong quietly -- a choice drawn as saved
// before the server said so, the evidence hidden once a person overrules it,
// and "not yet labelled" read as a label.

function memoryStore() {
  const bag = new Map<string, string>();
  return new LocalNexusSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
}

function mount(connection: Conn, intent?: { id: string; payload: Record<string, unknown> }) {
  h.connection = connection;
  render(
    withSession(
      <NexusApp
        sectionId="automations"
        navigate={vi.fn()}
        askContext={vi.fn()}
        intent={intent}
        consumeIntent={vi.fn()}
        store={memoryStore()}
      />,
    ),
  );
}

async function openProcedure(connection: Conn): Promise<HTMLElement> {
  mount(connection, { id: "i1", payload: { procedureId: "p1" } });
  await screen.findByRole("list", { name: /The certification ladder/ });
  return screen.getByRole("region", { name: "Reuse" });
}

/** A Fact's value, found by its label inside one panel. */
function fact(panel: HTMLElement, label: string): string | null {
  const dt = Array.from(panel.querySelectorAll("dt")).find((d) => d.textContent === label);
  return dt?.nextElementSibling?.textContent ?? null;
}

function choice(panel: HTMLElement, name: string): HTMLElement {
  return within(within(panel).getByRole("radiogroup", { name: "Reuse label" })).getByRole("radio", { name });
}

const GOAL_SPECIFIC: Row = procedureRow({
  id: "p1",
  reuse: "goalSpecific",
  reuseEvidence: { goalSignatures: ["sig-a"], signatureCount: 1, accountIds: [], uses: 4, decidedAt: "2026-09-25T00:00:00Z" },
});

describe("the reuse projection", () => {
  it("keeps the evidence and the person's label apart, and absent as absent", () => {
    const facts = reuseFactsFromRow(
      constructRow({
        id: "c1",
        name: "a",
        reuse: "goalSpecific",
        reuseEvidence: { goalSignatures: ["s1", "s2"], signatureCount: 2, accountIds: ["acme", "acme"], uses: 7 },
        reuseOverride: JSON.stringify({ label: "reusable", version: 3, at: "2026-09-20T00:00:00Z" }),
      }),
    );
    expect(facts).toEqual({
      evidence: "goalSpecific",
      override: "reusable",
      overrideVersion: 3,
      overrideAt: "2026-09-20T00:00:00Z",
      signatureCount: 2,
      uses: 7,
      accountCount: 1,
    });
    const unread = reuseFactsFromRow(constructRow({ id: "c2", name: "b" }));
    expect(unread.evidence).toBe("");
    expect(unread.signatureCount).toBeNull();
    expect(unread.uses).toBeNull();
  });
});

describe("the reuse panel on a procedure's page", () => {
  it("says what the evidence decided, and how much use it counted against the cluster's threshold", async () => {
    const panel = await openProcedure(fakeConnection({ learnedProcedures: [GOAL_SPECIFIC], ladderPolicy: [ladderPolicyRow()] }));
    expect(choice(panel, "Automatic").getAttribute("aria-checked")).toBe("true");
    expect(fact(panel, "From its use")).toBe("For one goal");
    await waitFor(() => expect(fact(panel, "Used for")).toBe("1 kind of goal in 4 runs; reusable at 2 kinds"));
    expect(fact(panel, "Accounts")).toBeNull();
  });

  it("says Not yet labelled, and counts nothing, before the sweep has looked", async () => {
    const panel = await openProcedure(fakeConnection({ learnedProcedures: [procedureRow({ id: "p1" })], ladderPolicy: [ladderPolicyRow()] }));
    expect(fact(panel, "From its use")).toBe("Not yet labelled");
    expect(fact(panel, "Used for")).toBeNull();
  });

  it("checks a label only once the server confirmed it, and keeps the evidence beside it", async () => {
    const conn = fakeConnection({ learnedProcedures: [GOAL_SPECIFIC], ladderPolicy: [ladderPolicyRow()] });
    let answer: (() => void) | null = null;
    const real = conn.query.setConstructReuse.getMockImplementation();
    conn.query.setConstructReuse.mockImplementationOnce(
      (args: Record<string, unknown>, opts?: unknown) =>
        new Promise((resolve) => {
          answer = () => resolve(real!(args, opts));
        }) as never,
    );
    const panel = await openProcedure(conn);

    fireEvent.click(choice(panel, "Reusable"));
    expect(conn.query.setConstructReuse).toHaveBeenCalledWith({ constructId: expect.stringContaining("p1"), label: "reusable" });
    // A PROPOSAL, NOT A SAVE: nothing is checked differently yet, and the
    // choice is busy rather than answered.
    expect(choice(panel, "Reusable").getAttribute("aria-checked")).toBe("false");
    expect(choice(panel, "Automatic").getAttribute("aria-checked")).toBe("true");
    expect(choice(panel, "Reusable").getAttribute("aria-busy")).toBe("true");

    await act(async () => answer!());
    await waitFor(() => expect(choice(panel, "Reusable").getAttribute("aria-checked")).toBe("true"));
    // The person's label wins; the evidence still says what it says.
    expect(fact(panel, "From its use")).toBe("For one goal");

    // The feed catching up keeps the choice where the server put it.
    act(() => {
      conn.subscriptions.emit(
        CONSTRUCT_CONCEPT,
        procedureRow({ ...GOAL_SPECIFIC, id: "p1", reuseOverride: { label: "reusable", version: 1 } }),
      );
    });
    expect(choice(panel, "Reusable").getAttribute("aria-checked")).toBe("true");
  });

  it("hands the label back to the evidence with Automatic", async () => {
    const conn = fakeConnection({
      learnedProcedures: [procedureRow({ ...GOAL_SPECIFIC, id: "p1", reuseOverride: { label: "reusable", version: 1 } })],
      ladderPolicy: [ladderPolicyRow()],
      reuseVersion: 2,
    });
    const panel = await openProcedure(conn);
    expect(choice(panel, "Reusable").getAttribute("aria-checked")).toBe("true");
    fireEvent.click(choice(panel, "Automatic"));
    await waitFor(() => expect(choice(panel, "Automatic").getAttribute("aria-checked")).toBe("true"));
    expect(conn.query.setConstructReuse).toHaveBeenCalledWith({ constructId: expect.stringContaining("p1"), label: "evidence" });
  });

  it("writes nothing for the choice already made", async () => {
    const conn = fakeConnection({ learnedProcedures: [GOAL_SPECIFIC], ladderPolicy: [ladderPolicyRow()] });
    const panel = await openProcedure(conn);
    fireEvent.click(choice(panel, "Automatic"));
    expect(conn.query.setConstructReuse).not.toHaveBeenCalled();
  });

  it("says a refusal verbatim beside the choice and leaves the saved choice checked", async () => {
    const conn = fakeConnection({
      learnedProcedures: [GOAL_SPECIFIC],
      ladderPolicy: [ladderPolicyRow()],
      writeError: new Error("construct_not_found: procedure.setReuse: construct p1 is not one of your constructs"),
    });
    const panel = await openProcedure(conn);
    fireEvent.click(choice(panel, "For one account"));
    expect(
      await within(panel).findByText("construct_not_found: procedure.setReuse: construct p1 is not one of your constructs"),
    ).toBeTruthy();
    expect(choice(panel, "Automatic").getAttribute("aria-checked")).toBe("true");
    expect(choice(panel, "For one account").getAttribute("aria-checked")).toBe("false");
  });
});

describe("reuse in the Automations list", () => {
  const LIST: Row[] = [
    constructRow({ id: "c1", name: "summariseInvoices", reuse: "reusable" }),
    constructRow({ id: "c2", name: "closeAcmeBooks", reuse: "goalSpecific", reuseOverride: { label: "accountSpecific", version: 1 } }),
    constructRow({ id: "c3", name: "fileQuarterlyReport" }),
  ];

  it("shows the label a person sees as a quiet fact, and nothing on an unlabelled row", async () => {
    mount(fakeConnection({ constructs: LIST }));
    const list = await screen.findByLabelText("Automations this instance can replay");
    await within(list).findByText("summariseInvoices");
    const row = (name: string) => within(list).getByText(name).closest("li") as HTMLElement;
    expect(within(row("summariseInvoices")).getByText("Reusable")).toBeTruthy();
    expect(within(row("closeAcmeBooks")).getByText("For one account")).toBeTruthy();
    expect(within(row("fileQuarterlyReport")).queryByText(/Not yet labelled|Reusable|For one/)).toBeNull();
  });

  it("narrows to one label with the Reuse facet, and names the facet as a removable chip", async () => {
    mount(fakeConnection({ constructs: LIST }));
    // RE-FOUND EVERY TIME: the list is keyed on the question, so a new facet
    // is a new list element, and the old one would still hold the old rows.
    const list = () => screen.getByLabelText("Automations this instance can replay");
    await screen.findByText("summariseInvoices");
    fireEvent.click(screen.getByRole("button", { name: "Refine automations" }));
    chooseOption(screen.getByLabelText("Reuse"), "Not yet labelled");
    await waitFor(() => expect(within(list()).queryByText("summariseInvoices")).toBeNull());
    expect(within(list()).getByText("fileQuarterlyReport")).toBeTruthy();
    expect(within(list()).queryByText("closeAcmeBooks")).toBeNull();

    // A person's own label is what the facet asks about, not the evidence's.
    fireEvent.click(screen.getByRole("button", { name: "Remove not yet labelled" }));
    chooseOption(screen.getByLabelText("Reuse"), "For one account");
    await waitFor(() => expect(within(list()).queryByText("fileQuarterlyReport")).toBeNull());
    expect(within(list()).getByText("closeAcmeBooks")).toBeTruthy();
  });

  it("opens the reuse panel beside an authored automation", async () => {
    mount(fakeConnection({ constructs: LIST }));
    fireEvent.click(await screen.findByText("summariseInvoices"));
    const panel = await screen.findByRole("region", { name: "Reuse" });
    expect(choice(panel, "Automatic").getAttribute("aria-checked")).toBe("true");
    expect(fact(panel, "From its use")).toBe("Reusable");
  });
});
