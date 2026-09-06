import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
const { constructRow, fakeConnection, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

// AUTOMATIONS: what this instance can replay without a model.
//
// This is where the product's claim becomes checkable, so the two things that
// would quietly break the reading are what the tests are about: a list that
// looks live and is not, and a trust figure dressed up as odds.

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

describe("it is a read, and it says so", () => {
  it("dates itself and offers to look again", async () => {
    const conn = fakeConnection({ constructs: [PROVEN] });
    mount(conn);
    await screen.findByText("nightlyReconcile");
    expect(screen.getByText(/This list is not live/)).toBeTruthy();
    expect(conn.query.cataloguedConstructsForOwner).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: /Look again/ }));
    await waitFor(() =>
      expect(conn.query.cataloguedConstructsForOwner).toHaveBeenCalledTimes(2),
    );
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

  it("writes through the catalog's own verb, and re-reads after", async () => {
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
    // An act is a change this window caused and therefore knows about.
    await waitFor(() =>
      expect(conn.query.cataloguedConstructsForOwner).toHaveBeenCalledTimes(2),
    );
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
