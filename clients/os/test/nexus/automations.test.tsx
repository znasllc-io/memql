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
const { rung, rungWord } = await import("../../src/apps/nexus/automations");
const { CONSTRUCT_CONCEPT, LADDER_POLICY_CONCEPT } = await import("../../src/apps/nexus/concepts");
const { constructRow, fakeConnection, ladderPolicyRow, procedureRow, withSession } = await import("./harness");
const { chooseOption } = await import("../selectControl");

type Conn = ReturnType<typeof fakeConnection>;

// AUTOMATIONS: what this instance can replay without a model.
//
// This is where the product's claim becomes checkable, so the things that
// would quietly break the reading are what the tests are about: a list that
// stops moving while the ladder does, a cue that rings on every evidence tick,
// and a trust figure dressed up as odds.

function memoryStore() {
  const bag = new Map<string, string>();
  return new LocalNexusSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
}

function mount(connection: Conn) {
  h.connection = connection;
  return render(
    withSession(
      <NexusApp
        sectionId="automations"
        navigate={vi.fn()}
        askContext={vi.fn()}
        store={memoryStore()}
      />,
    ),
  );
}

const PROVEN: Row = constructRow({ id: "c1", name: "nightlyReconcile" });
const UNPROVEN: Row = constructRow({
  id: "c2",
  name: "draftTheAnnouncement",
  status: "draft",
  reliability: 0,
  reinforceCount: 0,
  lastReinforced: "",
});

describe("what the catalog lists", () => {
  it("lists the automations, armed first", async () => {
    mount(fakeConnection({ constructs: [UNPROVEN, PROVEN] }));
    const rows = await screen.findAllByRole("button", { name: /Reconcile|nightlyReconcile|draftThe/ });
    expect(rows.length).toBeGreaterThan(0);
    expect(await screen.findByText("nightlyReconcile")).toBeTruthy();
    expect(screen.getByText("draftTheAnnouncement")).toBeTruthy();
  });

  // `cataloguedConstructsForOwner` returns every kind. A shape or a query in
  // this list would be an automation that is not one.
  it("keeps out everything that is not an automation", async () => {
    mount(
      fakeConnection({
        constructs: [PROVEN, constructRow({ id: "c3", name: "spaceCard", kind: "shape" })],
      }),
    );
    await screen.findByText("nightlyReconcile");
    expect(screen.queryByText("spaceCard")).toBeNull();
  });

  it("says an empty catalog is where automations COME FROM, not a place to make one", async () => {
    mount(fakeConnection({ constructs: [] }));
    expect(await screen.findByText(/An automation appears when a goal is worked out/)).toBeTruthy();
    // Deliberately NOT a "New automation" button: nobody authors one by hand
    // in this app, and offering it would be a control that cannot work.
    expect(screen.queryByText(/New automation/)).toBeNull();
    // ...and the bar does not tell somebody to select from a list that has
    // nothing in it.
    expect(screen.queryByText("select an automation to arm or retire it")).toBeNull();
    expect(screen.getByText("nothing to arm yet")).toBeTruthy();
  });
});

/** Deliver one construct write the way the cluster does: an event on the feed. */
function emitConstruct(conn: Conn, row: Row) {
  act(() => {
    conn.subscriptions.emit(CONSTRUCT_CONCEPT, row);
  });
}

describe("it is live", () => {
  it("moves a learned row's rung word when the ladder is written, without a re-read", async () => {
    const conn = fakeConnection({
      constructs: [PROVEN],
      learnedProcedures: [procedureRow({ id: "p1" })],
      ladderPolicy: [ladderPolicyRow()],
    });
    mount(conn);
    const list = await screen.findByLabelText("Automations this instance can replay");
    const before = await within(list).findByRole("button", { name: /Reconcile last month's ledger/ });
    expect(before.textContent).toContain("Shadow");

    // An agent node promoted it and the canary replayed once.
    emitConstruct(conn, procedureRow({ id: "p1", ladder: "canary", canaryMatches: 1 }));

    const after = await within(list).findByRole("button", { name: /Reconcile last month's ledger/ });
    await waitFor(() => expect(after.textContent).toContain("Canary"));
    expect(after.textContent).not.toContain("Shadow");
    expect(conn.query.learnedProceduresForOwner).toHaveBeenCalledTimes(1);
    expect(conn.query.cataloguedConstructsForOwner).toHaveBeenCalledTimes(1);
  });

  it("rings on a rung change and stays silent on an evidence tick", async () => {
    const conn = fakeConnection({
      constructs: [],
      learnedProcedures: [procedureRow({ id: "p1" })],
      ladderPolicy: [ladderPolicyRow()],
    });
    mount(conn);
    const row = (await screen.findByText("Reconcile last month's ledger against the bank export")).closest("li");

    // A shadow comparison matched: the count and the replay time move, and
    // the line says so -- but nothing a person would call news happened.
    emitConstruct(
      conn,
      procedureRow({ id: "p1", shadowMatches: 4, lastReplayAt: "2026-09-04T09:00:00Z" }),
    );
    await screen.findByText("4 of 5 matches beside the app");
    expect(row?.getAttribute("data-arrival")).toBeNull();

    // A promotion was proposed: that is news.
    emitConstruct(conn, procedureRow({ id: "p1", shadowMatches: 5, promotionApprovalId: "a-promo" }));
    await screen.findByText("Promotion waiting for you");
    const now = screen.getByText("Reconcile last month's ledger against the bank export").closest("li");
    expect(now?.getAttribute("data-arrival")).toBe("updated");
  });

  it("does not ring when an authored automation merely ran again", async () => {
    const conn = fakeConnection({ constructs: [PROVEN] });
    mount(conn);
    // Open it, so the run count the tick moves is on screen.
    fireEvent.click(await screen.findByText("nightlyReconcile"));
    expect(screen.getByText("12")).toBeTruthy();
    emitConstruct(conn, { ...PROVEN, reinforceCount: 13, lastReinforced: "2026-09-05T09:00:00Z" });
    expect(await screen.findByText("13")).toBeTruthy();
    const row = () => screen.getByText("nightlyReconcile").closest("li");
    expect(row()?.getAttribute("data-arrival")).toBeNull();
    // Retired, by contrast, is a status flip, and rings.
    emitConstruct(conn, { ...PROVEN, reinforceCount: 13, status: "retired" });
    await waitFor(() => expect(row()?.getAttribute("data-arrival")).toBe("updated"));
  });

  it("offers no Look again and prints no read time: the list follows the cluster", async () => {
    mount(fakeConnection({ constructs: [PROVEN] }));
    await screen.findByText("nightlyReconcile");
    expect(screen.queryByRole("button", { name: /Look again/ })).toBeNull();
    expect(screen.queryByText(/not live/)).toBeNull();
    expect(screen.queryByText(/^Read /)).toBeNull();
  });

  it("counts only a whole, settled answer", async () => {
    mount(
      fakeConnection({
        constructs: [PROVEN],
        learnedProcedures: new Error("PERMISSION_DENIED: no procedures for you"),
      }),
    );
    await screen.findByText("nightlyReconcile");
    // Half the catalog was refused, so no number stands over the list as if
    // it were the total.
    expect(document.querySelector(".os-head-meta")).toBeNull();
  });

  it("shows a refusal in the server's own words", async () => {
    mount(fakeConnection({ constructs: new Error("PERMISSION_DENIED: not your catalog") }));
    expect(await screen.findByText(/PERMISSION_DENIED: not your catalog/)).toBeTruthy();
  });

  it("says a full page is a PAGE rather than a total", async () => {
    const many: Row[] = [];
    for (let i = 0; i < 50; i += 1) {
      many.push(constructRow({ id: `c${i}`, name: `automation${i}` }));
    }
    mount(fakeConnection({ constructs: many }));
    await screen.findByText("automation0");
    expect(screen.getByText(/Showing the first 50 catalogued constructs/)).toBeTruthy();
  });
});

describe("the trust ladder is a word, never a percentage", () => {
  it("distinguishes a template nobody has run from one that keeps missing", () => {
    const never = { ...PROVEN, reliability: 0, reinforceCount: 0 } as never;
    const missing = { ...PROVEN, reliability: 0.1, reinforceCount: 4 } as never;
    expect(rungWord(rung(never))).toBe("Not yet proven");
    expect(rungWord(rung(missing))).toBe("Struggling");
  });

  it("prints no percentage anywhere on the list", async () => {
    mount(fakeConnection({ constructs: [PROVEN] }));
    await screen.findByText("nightlyReconcile");
    expect(document.body.textContent ?? "").not.toMatch(/\d+%/);
    expect(screen.getAllByLabelText(/^Proven\./).length).toBeGreaterThan(0);
  });
});

describe("arm and retire", () => {
  it("offers Retire on an armed automation and Arm on one that is not", async () => {
    mount(fakeConnection({ constructs: [PROVEN, UNPROVEN] }));
    fireEvent.click(await screen.findByText("nightlyReconcile"));
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).getByText("Retire it")).toBeTruthy();
    // AN ILLEGAL ACT IS ABSENT, never disabled.
    expect(within(bar).queryByText("Arm it")).toBeNull();

    fireEvent.click(screen.getByText("draftTheAnnouncement"));
    expect(within(bar).getByText("Arm it")).toBeTruthy();
    expect(within(bar).queryByText("Retire it")).toBeNull();
  });

  it("writes through the catalog's own verb, and the feed carries the result back", async () => {
    const conn = fakeConnection({ constructs: [UNPROVEN] });
    mount(conn);
    fireEvent.click(await screen.findByText("draftTheAnnouncement"));
    fireEvent.click(screen.getByText("Arm it"));
    await waitFor(() =>
      expect(conn.query.setConstructStatus).toHaveBeenCalledWith({
        constructId: "c2",
        status: "active",
      }),
    );
    // The write's own event moves the row; nothing reads the catalog again.
    emitConstruct(conn, { ...UNPROVEN, status: "active" });
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(await within(bar).findByText("Retire it")).toBeTruthy();
    expect(conn.query.cataloguedConstructsForOwner).toHaveBeenCalledTimes(1);
  });

  it("shows a refused write verbatim and leaves the act offered", async () => {
    const conn = fakeConnection({
      constructs: [UNPROVEN],
      writeError: new Error("PERMISSION_DENIED: not yours to arm"),
    });
    mount(conn);
    fireEvent.click(await screen.findByText("draftTheAnnouncement"));
    fireEvent.click(screen.getByText("Arm it"));
    expect(await screen.findByText(/PERMISSION_DENIED: not yours to arm/)).toBeTruthy();
    expect(screen.getByText("Arm it")).toBeTruthy();
  });
});

describe("learned procedures are automations with a different origin", () => {
  const LEARNED: Row = procedureRow({ id: "p1" });

  it("lists a learned procedure in the SAME list, with its rung as a word and its origin as a fact", async () => {
    mount(
      fakeConnection({
        constructs: [PROVEN],
        learnedProcedures: [LEARNED],
        ladderPolicy: [ladderPolicyRow()],
      }),
    );
    const list = await screen.findByLabelText("Automations this instance can replay");
    const learned = await within(list).findByRole("button", { name: /Reconcile last month's ledger/ });
    expect(learned.textContent).toContain("Learned from 2 runs");
    expect(learned.textContent).toContain("Shadow");
    expect(learned.textContent).toContain("3 of 5 matches beside the app");
    const authored = within(list).getByRole("button", { name: /nightlyReconcile/ });
    expect(authored.textContent).toContain("Authored");
    // No heading splits the list by origin.
    expect(screen.queryByRole("heading", { name: /Learned/ })).toBeNull();
  });

  it("puts a procedure waiting on you first, and says so in words", async () => {
    mount(
      fakeConnection({
        constructs: [PROVEN],
        learnedProcedures: [procedureRow({ id: "p1", shadowMatches: 5, promotionApprovalId: "a-promo" })],
        ladderPolicy: [ladderPolicyRow()],
      }),
    );
    const list = await screen.findByLabelText("Automations this instance can replay");
    await within(list).findByText("Promotion waiting for you");
    const rows = within(list).getAllByRole("button");
    expect(rows[0]?.textContent).toContain("Promotion waiting for you");
  });

  it("filters by origin behind Refine, and shows the facet as a removable chip", async () => {
    mount(fakeConnection({ constructs: [PROVEN], learnedProcedures: [LEARNED] }));
    await screen.findByText("nightlyReconcile");
    fireEvent.click(screen.getByRole("button", { name: "Refine automations" }));
    chooseOption(screen.getByLabelText("Origin"), "Learned only");
    await waitFor(() => expect(screen.queryByText("nightlyReconcile")).toBeNull());
    expect(screen.getByText("Reconcile last month's ledger against the bank export")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove learned" }));
    expect(await screen.findByText("nightlyReconcile")).toBeTruthy();
  });

  it("never offers Arm or Retire on a learned procedure", async () => {
    mount(fakeConnection({ constructs: [], learnedProcedures: [LEARNED] }));
    await screen.findByText("Reconcile last month's ledger against the bank export");
    expect(screen.queryByText("Arm it")).toBeNull();
    expect(screen.queryByText("Retire it")).toBeNull();
    // ...and the bar does not tell somebody to arm what cannot be armed.
    expect(screen.getByText("select one to see where it stands")).toBeTruthy();
  });

  it("still lists every authored automation when the learned read is refused", async () => {
    mount(
      fakeConnection({
        constructs: [PROVEN],
        learnedProcedures: new Error("PERMISSION_DENIED: no procedures for you"),
      }),
    );
    expect(await screen.findByText("nightlyReconcile")).toBeTruthy();
    expect(screen.getByText("PERMISSION_DENIED: no procedures for you")).toBeTruthy();
  });

  it("follows the ladder's policy too: a new value arrives without a re-read", async () => {
    const conn = fakeConnection({
      constructs: [PROVEN],
      learnedProcedures: [LEARNED],
      ladderPolicy: [ladderPolicyRow()],
    });
    mount(conn);
    await screen.findByText("3 of 5 matches beside the app");
    act(() => {
      conn.subscriptions.emit(LADDER_POLICY_CONCEPT, ladderPolicyRow({ shadowMatches: 8 }));
    });
    expect(await screen.findByText("3 of 8 matches beside the app")).toBeTruthy();
    expect(conn.query.ladderPolicyCurrent).toHaveBeenCalledTimes(1);
  });

  it("keeps an event for a construct outside the read's scope out of the list", async () => {
    const conn = fakeConnection({ constructs: [PROVEN], learnedProcedures: [LEARNED] });
    mount(conn);
    await screen.findByText("nightlyReconcile");
    // A bundle's draft construct: not catalogued and not learned. It is the
    // caller's own row, so the subscription delivers it -- and the list must
    // not, because neither read would.
    emitConstruct(conn, constructRow({ id: "c9", name: "draftInABundle", catalogued: false }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.queryByText("draftInABundle")).toBeNull();
  });
});

describe("what a template is FOR", () => {
  it("says whether it answers a goal shape, rather than printing the hash", async () => {
    mount(fakeConnection({ constructs: [PROVEN] }));
    fireEvent.click(await screen.findByText("nightlyReconcile"));
    expect(screen.getByText("yes — it answers a goal shape")).toBeTruthy();
    expect(document.body.textContent ?? "").not.toContain("sha256:goal-1");
  });

  it("shows the source, which is what it will actually replay", async () => {
    mount(fakeConnection({ constructs: [PROVEN] }));
    fireEvent.click(await screen.findByText("nightlyReconcile"));
    expect(screen.getByText("automation nightlyReconcile { }")).toBeTruthy();
  });
});
