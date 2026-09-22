import { act, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { OriginsSection } = await import("../../src/apps/cluster/origins/OriginsSection");
const { dataOriginRow, fakeConnection, syncStateRow, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

function mount(connection: Conn) {
  h.connection = connection;
  return render(withSession(<OriginsSection />));
}

async function click(el: Element) {
  await act(async () => {
    (el as HTMLElement).click();
  });
}

/** A concept whose connector has NEVER RUN: an inventory row, and no health
 *  row anywhere. Every figure for it is absent. */
const NEVER_RUN = dataOriginRow({
  conceptId: "v1:shopify:product",
  dataState: "mirror",
  origin: "shopify",
  connectors: ["shopify"],
});

/** A concept whose connector HAS run and measured zero of everything. The
 *  control for the test below: without it, a page that rendered nothing at
 *  all for every figure would pass the em-dash assertion. */
const RAN_CLEAN = dataOriginRow({
  conceptId: "v1:shopify:customer",
  dataState: "mirror",
  origin: "shopify",
  connectors: ["shopify"],
});

const RAN_CLEAN_HEALTH = syncStateRow({
  conceptId: "v1:shopify:customer",
  connector: "shopify",
  direction: "inbound",
  lagSeconds: 0,
  driftCount: 0,
  outboxDepth: 0,
  deadLetterCount: 0,
  backfillStatus: "complete",
  paused: false,
});

/** The row for a concept, AFTER the read that produced it has landed.
 *  Async because the two readings settle independently -- a synchronous
 *  lookup here would race the inventory and read as a missing row. */
async function rowFor(conceptId: string): Promise<HTMLElement> {
  const el = await screen.findByText(conceptId);
  return el.closest(".os-cluster-tr") as HTMLElement;
}

beforeEach(() => {
  h.connection = null;
});

describe("the data-origins table", () => {
  it("renders absent health as an em dash and NOT as a zero", async () => {
    // THE REGRESSION THAT MATTERS. A connector that has never run has no lag,
    // no drift, no outbox depth and no dead letters. Rendering any of those
    // as `0` says "we looked, and the answer is none" -- which is exactly
    // what a clean sweep produces, and the two lead to opposite actions.
    mount(
      fakeConnection({
        dataOrigins: [NEVER_RUN, RAN_CLEAN],
        syncStatesAll: [RAN_CLEAN_HEALTH],
      }),
    );

    // Both readings have to have landed: the inventory puts the rows on
    // screen, and the health puts the measured zeros in the control row.
    await screen.findByText("v1:shopify:product");
    await screen.findAllByText("0");

    const never = await rowFor("v1:shopify:product");
    // Four figures, four dashes.
    expect(within(never).getAllByText("—")).toHaveLength(4);
    // And NO zero anywhere in the row: the assertion the dash alone does not
    // make, because a row could render both.
    expect(never.textContent).not.toContain("0");
    // The row says so in words too, for a reader who does not know the dash.
    expect(within(never).getByText(/nothing has reported on this pairing/)).toBeTruthy();

    // THE NEGATIVE CONTROL. A connector that ran and measured zero renders
    // the zero -- so this suite fails if the page simply never prints
    // numbers, which would satisfy the assertion above for the wrong reason.
    const clean = await rowFor("v1:shopify:customer");
    expect(within(clean).getAllByText("0")).toHaveLength(4);
    expect(within(clean).queryByText("—")).toBeNull();
  });

  it("says how much of the declared inventory the table is about", async () => {
    mount(
      fakeConnection({
        dataOrigins: [
          NEVER_RUN,
          dataOriginRow({ conceptId: "v1:work:run" }),
          dataOriginRow({ conceptId: "v1:work:run" }),
        ],
      }),
    );
    expect(
      await screen.findByText("1 of 3 declared concepts have a connector"),
    ).toBeTruthy();
  });

  it("marks a mirror as refusing every write that is not its connector's", async () => {
    mount(fakeConnection({ dataOrigins: [NEVER_RUN] }));
    const row = await rowFor("v1:shopify:product");
    expect(within(row).getByText("mirror")).toBeTruthy();
    expect(
      within(row).getByText(/refuses every write that does not come from shopify/),
    ).toBeTruthy();
  });

  it("offers Pause on a running connector and Resume on a paused one, never both", async () => {
    mount(
      fakeConnection({
        dataOrigins: [NEVER_RUN, RAN_CLEAN],
        syncStatesAll: [
          syncStateRow({ conceptId: "v1:shopify:customer", connector: "shopify", paused: true }),
        ],
      }),
    );

    await screen.findByText("paused");
    const running = await rowFor("v1:shopify:product");
    expect(within(running).getByRole("button", { name: /^Pause/ })).toBeTruthy();
    expect(within(running).queryByRole("button", { name: /^Resume/ })).toBeNull();

    const paused = await rowFor("v1:shopify:customer");
    expect(within(paused).getByRole("button", { name: /^Resume/ })).toBeTruthy();
    expect(within(paused).queryByRole("button", { name: /^Pause/ })).toBeNull();
  });

  it("does not read dead letters until somebody asks", async () => {
    const connection = fakeConnection({ dataOrigins: [NEVER_RUN] });
    mount(connection);
    await screen.findByText("v1:shopify:product");

    // The fan-out is one read per connector, and on a healthy cluster every
    // answer is empty. Nothing has been asked yet.
    expect(connection.query.outboxDeadLetters).not.toHaveBeenCalled();

    await click(screen.getByRole("button", { name: "Check shopify" }));
    expect(connection.query.outboxDeadLetters).toHaveBeenCalledWith(
      { target: "shopify" },
      expect.anything(),
    );
  });
});

describe("connector coverage", () => {
  // memql#5574's production symptom, as a page: a connector that could not
  // read its own store list enumerated nothing, reported on nothing, and the
  // table drew a correct em dash per concept while saying nothing about the
  // connector. The issue's words were "the mirror stopped changing, with
  // nothing anywhere saying why".
  it("says a connector has reported on none of its concepts", async () => {
    mount(
      fakeConnection({
        dataOrigins: [
          NEVER_RUN,
          dataOriginRow({
            conceptId: "v1:shopify:order",
            dataState: "mirror",
            origin: "shopify",
            connectors: ["shopify"],
          }),
        ],
        syncStatesAll: [],
      }),
    );
    expect(await screen.findByText("0 of 2 reported")).toBeTruthy();
    expect(
      screen.getByText(/Nothing has reported on any of its 2 concepts/),
    ).toBeTruthy();
  });

  // THE NEGATIVE CONTROL, and it is the one that matters most here. A page
  // that reported "0 of N" for every connector regardless would satisfy the
  // assertion above, and would be wrong in the direction that raises a false
  // alarm on every healthy cluster.
  it("says a fully reporting connector has reported, and says nothing more", async () => {
    mount(
      fakeConnection({
        dataOrigins: [RAN_CLEAN],
        syncStatesAll: [RAN_CLEAN_HEALTH],
      }),
    );
    expect(await screen.findByText("1 of 1 reported")).toBeTruthy();
    // No sentence: there is nothing to say about a connector that is working,
    // and a line of reassurance per healthy connector is noise a reader has
    // to scan past to find the one that is not.
    expect(screen.queryByText(/Nothing has reported/)).toBeNull();
    expect(screen.queryByText(/have reported\. The rest/)).toBeNull();
  });

  // THE PARTIAL CASE is its own reading and not a rounding of either. Some
  // concepts measured and some not is what a backfill in progress looks like,
  // and it must not read as the silent case.
  it("distinguishes a partly reporting connector from a silent one", async () => {
    mount(
      fakeConnection({
        dataOrigins: [NEVER_RUN, RAN_CLEAN],
        syncStatesAll: [RAN_CLEAN_HEALTH],
      }),
    );
    expect(await screen.findByText("1 of 2 reported")).toBeTruthy();
    expect(
      screen.getByText(/1 of 2 concepts have reported\. The rest have no measurement/),
    ).toBeTruthy();
    expect(screen.queryByText(/Nothing has reported on any of its/)).toBeNull();
  });

  // THE PAGE NEVER CLAIMS A FAULT. It cannot tell a connector that has not
  // run from one that is running and reading nothing, and saying either would
  // be inventing a fact out of an absence -- on a page whose whole design
  // rests on an em dash not being a zero.
  it("names both causes of a silent connector and picks neither", async () => {
    mount(fakeConnection({ dataOrigins: [NEVER_RUN], syncStatesAll: [] }));
    const sentence = await screen.findByText(/Nothing has reported on any of its/);
    const text = sentence.textContent ?? "";
    expect(text).toContain("has not run yet");
    expect(text).toContain("reading nothing");
    // And it sends the reader where the difference actually is.
    expect(text).toContain("configuration");
    for (const verdict of ["broken", "failed", "healthy", "fine"]) {
      expect(text.toLowerCase()).not.toContain(verdict);
    }
  });

  // One line per connector, so a cluster with two connectors and one silent
  // one shows which.
  it("reports each connector separately", async () => {
    mount(
      fakeConnection({
        dataOrigins: [
          NEVER_RUN,
          dataOriginRow({
            conceptId: "v1:books:invoice",
            dataState: "mirror",
            origin: "quickBooks",
            connectors: ["quickBooks"],
          }),
        ],
        syncStatesAll: [
          syncStateRow({
            conceptId: "v1:books:invoice",
            connector: "quickBooks",
            direction: "inbound",
            lagSeconds: 2,
            driftCount: 0,
            outboxDepth: 0,
            deadLetterCount: 0,
            backfillStatus: "complete",
            paused: false,
          }),
        ],
      }),
    );
    expect(await screen.findByText("0 of 1 reported")).toBeTruthy();
    expect(screen.getByText("1 of 1 reported")).toBeTruthy();
    // The silent one's ratio carries the attention ink; the reporting one's
    // does not. Asserted through the tone attribute rather than a computed
    // colour, which jsdom cannot see.
    const silent = screen.getByText("0 of 1 reported").closest(".os-cluster-coverage-line");
    const reporting = screen.getByText("1 of 1 reported").closest(".os-cluster-coverage-line");
    expect(silent?.getAttribute("data-tone")).toBe("silent");
    expect(reporting?.getAttribute("data-tone")).toBe("reporting");
  });
});
