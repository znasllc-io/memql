import { act, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { betweenTheClicks, readTheBar, useFakeDate } from "../readTheBar";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { OriginsSection } = await import("../../src/apps/cluster/origins/OriginsSection");
const { dataOriginRow, fakeConnection, inboundRequestRow, pagedRead, syncStateRow, withSession } = await import("./harness");

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
  return el.closest("li") as HTMLElement;
}

beforeEach(() => {
  h.connection = null;
});

describe("the data-origins table", () => {
  it("uses silent skeletons during a delayed read and exposes a failed read", async () => {
    const connection = fakeConnection();
    let reject!: (reason: Error) => void;
    connection.query.dataOrigins.mockImplementationOnce(() => new Promise((_resolve, fail) => { reject = fail; }));
    const view = mount(connection);
    const label = screen.getByText("Loading the declared inventory");
    expect(label.className).toBe("os-sr-only");
    expect(label.closest('[aria-busy="true"]')).not.toBeNull();
    expect(view.container.querySelector(".os-skeleton-block")).not.toBeNull();
    expect(screen.queryByText(/Reading the (trail|declared inventory)/)).toBeNull();
    await act(async () => reject(new Error("temporarily unavailable")));
    expect(view.container.querySelector(".os-record-skeleton")).toBeNull();
    expect(screen.getByText("temporarily unavailable")).toBeTruthy();
  });


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
    expect(connection.query.outboxDeadLettersBrowse).not.toHaveBeenCalled();

    await click(screen.getByRole("button", { name: "Check shopify" }));
    expect(connection.query.outboxDeadLettersBrowse).toHaveBeenCalledWith(
      { target: "shopify", search: "" },
      expect.anything(),
    );
  });
});

// A DEAD LETTER'S RETRY AND DISCARD ASK IN THE ROW (memql#6033). The question
// takes the place of the two buttons, and its confirmation stands where the
// act that asked it stood -- outside any bar, so nothing held the second click
// of the double-click, and it answered a question nobody had read. Discard is
// the one that cannot be undone.
describe("a dead letter's Retry and Discard", () => {
  useFakeDate();

  const ENTRY = {
    id: "dl-1",
    conceptId: "v1:shopify:product",
    rowRef: "p-1",
    action: "upsert",
    target: "shopify",
    attempts: 5,
    lastError: "429 from the vendor",
    createdAt: "2026-10-12T09:00:00Z",
  };

  async function openBand() {
    const connection = fakeConnection({ dataOrigins: [NEVER_RUN], outboxDeadLettersBrowse: [ENTRY] });
    mount(connection);
    await screen.findByText("v1:shopify:product");
    await click(screen.getByRole("button", { name: "Check shopify" }));
    await screen.findByText("429 from the vendor");
    return { connection, band: screen.getByRole("list", { name: "shopify dead letters" }) };
  }

  it("does not discard on the second click of the double-click that asked, and does once it is read", async () => {
    const { connection, band } = await openBand();
    await click(within(band).getByRole("button", { name: "Discard" }));
    expect(within(band).getByText(/Discard it\? The change it carried is lost/)).toBeTruthy();

    betweenTheClicks();
    await click(within(band).getByRole("button", { name: "Discard" }));
    expect(connection.query.datasyncDiscardOutboxEntry).not.toHaveBeenCalled();
    // Still asking: the press that came too soon neither answered nor withdrew the question.
    expect(within(band).getByText(/Discard it\? The change it carried is lost/)).toBeTruthy();

    await readTheBar();
    await click(within(band).getByRole("button", { name: "Discard" }));
    expect(connection.query.datasyncDiscardOutboxEntry).toHaveBeenCalledTimes(1);
    expect(connection.query.datasyncDiscardOutboxEntry).toHaveBeenCalledWith(expect.objectContaining({ entryId: "dl-1" }));
  });

  it("does not retry on the second click of the double-click that asked, and does once it is read", async () => {
    const { connection, band } = await openBand();
    await click(within(band).getByRole("button", { name: "Retry" }));
    expect(within(band).getByText("Send this entry to the drain again?")).toBeTruthy();

    betweenTheClicks();
    await click(within(band).getByRole("button", { name: "Retry" }));
    expect(connection.query.datasyncRetryOutboxEntry).not.toHaveBeenCalled();

    await readTheBar();
    await click(within(band).getByRole("button", { name: "Retry" }));
    expect(connection.query.datasyncRetryOutboxEntry).toHaveBeenCalledTimes(1);
  });

  it("holds the way out of the question as well", async () => {
    const { band } = await openBand();
    await click(within(band).getByRole("button", { name: "Discard" }));
    betweenTheClicks();
    await click(within(band).getByRole("button", { name: "Cancel" }));
    expect(within(band).getByText(/Discard it\?/)).toBeTruthy();
    await readTheBar();
    await click(within(band).getByRole("button", { name: "Cancel" }));
    expect(within(band).queryByText(/Discard it\?/)).toBeNull();
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
    const silent = screen.getByText("0 of 1 reported").closest(".os-record-row");
    const reporting = screen.getByText("1 of 1 reported").closest(".os-record-row");
    expect(silent?.querySelector(".os-record-status")?.getAttribute("data-tone")).toBe("warn");
    expect(reporting?.querySelector(".os-record-status")?.getAttribute("data-tone")).toBe("muted");
  });
});

// CLUSTER > DATA, BY STORE (U7). A connector that serves several stores keeps
// each store's sync state apart, and every row carries the store as its
// partition. The page groups a connector's domains by store, keeps what a
// connection never asked for out of the errors, and does not draw the rows the
// connector wrote before it kept its stores apart.
describe("data origins, by store", () => {
  const STORE = "acme-widgets";
  const DEV = "acme-widgets-dev";

  const mirror = (conceptId: string, connector = "shopify") =>
    dataOriginRow({ conceptId, dataState: "mirror", origin: connector, connectors: [connector] });
  const ORDER = mirror("v1:shopify:order");
  const CHECKOUT = mirror("v1:shopify:abandonedCheckout");
  const INVOICE = mirror("v1:books:invoice", "quickBooks");

  const state = (conceptId: string, partitionKey: string, over: Record<string, unknown> = {}) =>
    syncStateRow({
      id: `v1:platform:syncState:${conceptId}-${partitionKey}`,
      conceptId,
      connector: "shopify",
      partitionKey,
      lagSeconds: 1,
      driftCount: 0,
      outboxDepth: 0,
      deadLetterCount: 0,
      backfillStatus: "complete",
      paused: false,
      ...over,
    });
  const INVOICE_HEALTH = syncStateRow({ conceptId: "v1:books:invoice", connector: "quickBooks", lagSeconds: 2 });

  it("groups a connector's domains by store", async () => {
    mount(
      fakeConnection({
        dataOrigins: [ORDER, INVOICE],
        syncStatesAll: [state("v1:shopify:order", STORE), state("v1:shopify:order", DEV), INVOICE_HEALTH],
      }),
    );
    const own = await screen.findByRole("group", { name: STORE });
    const dev = screen.getByRole("group", { name: DEV });
    // Each store is headed by its partition, in plain text.
    expect(within(own).getByRole("heading", { name: STORE })).toBeTruthy();
    expect(within(own).getByText("v1:shopify:order")).toBeTruthy();
    expect(within(dev).getByText("v1:shopify:order")).toBeTruthy();
    // A connector that keeps no stores apart is drawn as it always was: in
    // no store's group, and with no heading of its own.
    const invoice = await rowFor("v1:books:invoice");
    expect(own.contains(invoice)).toBe(false);
    expect(dev.contains(invoice)).toBe(false);
    expect(screen.queryByRole("group", { name: "" })).toBeNull();
  });

  it("reads health past the first page", async () => {
    const connection = fakeConnection({ dataOrigins: [ORDER, CHECKOUT] });
    connection.query.syncStatesAll = pagedRead([
      [state("v1:shopify:order", STORE)],
      [state("v1:shopify:abandonedCheckout", STORE, { lagSeconds: 7 })],
    ]);
    mount(connection);
    const group = await screen.findByRole("group", { name: STORE });
    expect(await within(group).findByText("7")).toBeTruthy();
    expect(connection.query.syncStatesAll).toHaveBeenCalledTimes(2);
    expect(connection.query.syncStatesAll).toHaveBeenLastCalledWith({}, expect.objectContaining({ cursor: "p1" }));
  });

  it("keeps what a connection never asked for behind one closed line, and out of the error count", async () => {
    mount(
      fakeConnection({
        dataOrigins: [ORDER, CHECKOUT],
        syncStatesAll: [
          state("v1:shopify:order", STORE),
          state("v1:shopify:abandonedCheckout", STORE, { lastError: "connector: not requested: read_checkouts is not in the grant" }),
        ],
      }),
    );
    const group = await screen.findByRole("group", { name: STORE });
    const line = within(group).getByText("Not part of this connection");
    const details = line.closest("details") as HTMLDetailsElement;
    expect(details).not.toBeNull();
    expect(details.open).toBe(false);
    // Behind the line, and not a row of the store's list.
    expect(details.textContent).toContain("v1:shopify:abandonedCheckout");
    const list = within(group).getByRole("list", { name: STORE });
    expect(within(list).queryByText("v1:shopify:abandonedCheckout")).toBeNull();
    // Never an error: not in the connector's count, not in error ink, not a Notice.
    expect(screen.queryByText(/carries an error|carry an error/)).toBeNull();
    expect(document.querySelector(".os-cluster-row-error")).toBeNull();
    expect(screen.queryByText(/connector: not requested/, { selector: ".os-notice *" })).toBeNull();
    expect(document.querySelector(".os-notice")).toBeNull();
  });

  it("counts a store's real failure as an error, beside what its connection never asked for", async () => {
    // The control for the test above: a page that dropped every lastError
    // would pass it.
    mount(
      fakeConnection({
        dataOrigins: [ORDER, CHECKOUT],
        syncStatesAll: [
          state("v1:shopify:order", STORE, { lastError: "the Admin API answered 503" }),
          state("v1:shopify:abandonedCheckout", STORE, { lastError: "connector: not requested" }),
        ],
      }),
    );
    expect(await screen.findByText(/1 of them carries an error/)).toBeTruthy();
    const row = await rowFor("v1:shopify:order");
    expect(within(row).getByText("the Admin API answered 503").className).toContain("os-cluster-row-error");
  });

  it("does not draw a shared row of a connector that now keeps its stores apart", async () => {
    mount(
      fakeConnection({
        dataOrigins: [ORDER, INVOICE],
        syncStatesAll: [
          state("v1:shopify:order", STORE),
          syncStateRow({ conceptId: "v1:shopify:order", connector: "shopify", lastError: "a failure from before the stores were kept apart" }),
          INVOICE_HEALTH,
        ],
      }),
    );
    await screen.findByRole("group", { name: STORE });
    expect(screen.getAllByText("v1:shopify:order")).toHaveLength(1);
    expect(screen.queryByText("a failure from before the stores were kept apart")).toBeNull();
    // A connector with no partitioned row at all keeps its row.
    expect(await rowFor("v1:books:invoice")).toBeTruthy();
    // And the history is not counted either: each connector has one pairing.
    expect(screen.getAllByText("1 of 1 reported")).toHaveLength(2);
    expect(screen.queryByText("2 of 2 reported")).toBeNull();
  });

  it("sends the store with Backfill, Pause and Resume", async () => {
    const connection = fakeConnection({
      dataOrigins: [ORDER, INVOICE],
      syncStatesAll: [
        state("v1:shopify:order", STORE),
        state("v1:shopify:order", DEV, { paused: true }),
        INVOICE_HEALTH,
      ],
    });
    mount(connection);
    const own = await screen.findByRole("group", { name: STORE });
    const dev = screen.getByRole("group", { name: DEV });

    await click(within(own).getByRole("button", { name: /^Backfill now/ }));
    expect(connection.query.datasyncStartBackfill).toHaveBeenLastCalledWith({
      connector: "shopify",
      conceptId: "v1:shopify:order",
      partitionKey: STORE,
    });
    await click(within(own).getByRole("button", { name: /^Pause/ }));
    expect(connection.query.datasyncSetSyncPaused).toHaveBeenLastCalledWith({
      connector: "shopify",
      conceptId: "v1:shopify:order",
      partitionKey: STORE,
      paused: true,
    });
    await click(within(dev).getByRole("button", { name: /^Resume/ }));
    expect(connection.query.datasyncSetSyncPaused).toHaveBeenLastCalledWith({
      connector: "shopify",
      conceptId: "v1:shopify:order",
      partitionKey: DEV,
      paused: false,
    });

    // A row of a connector that keeps no stores apart sends none, as before.
    const invoice = await rowFor("v1:books:invoice");
    await click(within(invoice).getByRole("button", { name: /^Pause/ }));
    expect(connection.query.datasyncSetSyncPaused).toHaveBeenLastCalledWith({
      connector: "quickBooks",
      conceptId: "v1:books:invoice",
      paused: true,
    });
  });

  it("does not read given-up deliveries until somebody asks", async () => {
    const connection = fakeConnection({ dataOrigins: [ORDER] });
    mount(connection);
    await screen.findByText("v1:shopify:order");
    expect(connection.query.inboundRequestsGivenUp).not.toHaveBeenCalled();

    const section = screen.getByRole("region", { name: "Changes that could not be recorded" });
    await click(within(section).getByRole("button", { name: "Check" }));
    expect(connection.query.inboundRequestsGivenUp).toHaveBeenCalledWith({}, expect.anything());
  });

  it("lists given-up deliveries by source and tries one source again", async () => {
    const connection = fakeConnection({
      dataOrigins: [ORDER],
      inboundRequestsGivenUp: [
        inboundRequestRow({ id: "in-3", source: "shopify-acme-widgets", lastError: "the Admin API answered 503" }),
        inboundRequestRow({ id: "in-2", source: "shopify-acme-widgets-dev", lastError: "the topic is not handled" }),
        inboundRequestRow({ id: "in-1", source: "shopify-acme-widgets", lastError: "an older reason" }),
      ],
    });
    mount(connection);
    await screen.findByText("v1:shopify:order");
    const section = screen.getByRole("region", { name: "Changes that could not be recorded" });
    await click(within(section).getByRole("button", { name: "Check" }));

    const list = await within(section).findByRole("list", { name: "Changes that could not be recorded" });
    const own = within(list).getByText("shopify-acme-widgets").closest("li") as HTMLElement;
    const dev = within(list).getByText("shopify-acme-widgets-dev").closest("li") as HTMLElement;
    // One line per source, its count and its LAST reason, as the engine wrote it.
    expect(within(own).getByText("2 changes")).toBeTruthy();
    expect(within(own).getByText("the Admin API answered 503")).toBeTruthy();
    expect(within(own).queryByText("an older reason")).toBeNull();
    expect(within(dev).getByText("1 change")).toBeTruthy();
    expect(within(dev).getByText("the topic is not handled")).toBeTruthy();

    expect(connection.query.inboundRequestsGivenUp).toHaveBeenCalledTimes(1);
    await click(within(own).getByRole("button", { name: "Try again" }));
    expect(connection.query.datasyncRequeueInbound).toHaveBeenCalledTimes(1);
    expect(connection.query.datasyncRequeueInbound).toHaveBeenCalledWith({ source: "shopify-acme-widgets" });
    // And the section is read again.
    expect(connection.query.inboundRequestsGivenUp).toHaveBeenCalledTimes(2);
  });

  it("draws each count as a floor when pages remain after twenty", async () => {
    const connection = fakeConnection({ dataOrigins: [ORDER] });
    connection.query.inboundRequestsGivenUp = pagedRead(
      Array.from({ length: 25 }, (_, i) => [inboundRequestRow({ id: `in-${i}`, source: "shopify-acme-widgets" })]),
    );
    mount(connection);
    await screen.findByText("v1:shopify:order");
    const section = screen.getByRole("region", { name: "Changes that could not be recorded" });
    await click(within(section).getByRole("button", { name: "Check" }));
    expect(await within(section).findByText("At least 20 changes")).toBeTruthy();
    expect(connection.query.inboundRequestsGivenUp).toHaveBeenCalledTimes(20);
  });

  it("draws a refusal of Try again as the engine wrote it", async () => {
    const connection = fakeConnection({
      dataOrigins: [ORDER],
      inboundRequestsGivenUp: [inboundRequestRow({ id: "in-1", source: "shopify-acme-widgets" })],
    });
    connection.query.datasyncRequeueInbound.mockRejectedValueOnce(new Error("datasync: requeue refused: not a cluster owner"));
    mount(connection);
    await screen.findByText("v1:shopify:order");
    const section = screen.getByRole("region", { name: "Changes that could not be recorded" });
    await click(within(section).getByRole("button", { name: "Check" }));
    await click(await within(section).findByRole("button", { name: "Try again" }));
    expect(within(section).getByText("datasync: requeue refused: not a cluster owner")).toBeTruthy();
  });
});

// FIX ROUND 1 (rulings 44 and 45). Nothing outside a connection is counted
// anywhere; a count of concepts counts concepts, not one concept per store;
// and no act is offered while the page cannot tell a store's row from a
// shared one.
describe("data origins, by store: the numbers and the acts", () => {
  const STORE = "acme-widgets";
  const DEV = "acme-widgets-dev";
  const NOT_REQUESTED = "connector: not requested";

  const mirror = (conceptId: string, connector = "shopify") =>
    dataOriginRow({ conceptId, dataState: "mirror", origin: connector, connectors: [connector] });
  const shopify = (name: string, partitionKey: string, over: Record<string, unknown> = {}) =>
    syncStateRow({
      id: `v1:platform:syncState:${name}-${partitionKey}`,
      conceptId: `v1:shopify:${name}`,
      connector: "shopify",
      partitionKey,
      lagSeconds: 1,
      paused: false,
      ...over,
    });
  const books = (name: string) => syncStateRow({ conceptId: `v1:books:${name}`, connector: "quickBooks", lagSeconds: 2 });

  // The QA scene: two QuickBooks concepts; five Shopify concepts on two
  // stores, two of them not requested on each.
  const SCENE_ORIGINS = [
    ...["order", "customer", "product", "abandonedCheckout", "company"].map((n) => mirror(`v1:shopify:${n}`)),
    ...["invoice", "payment"].map((n) => mirror(`v1:books:${n}`, "quickBooks")),
  ];
  const SCENE_HEALTH = [
    ...["order", "customer", "product"].map((n) => shopify(n, STORE)),
    ...["abandonedCheckout", "company"].map((n) => shopify(n, STORE, { lastError: NOT_REQUESTED })),
    shopify("order", DEV, { lastError: "the Admin API answered 503" }),
    shopify("customer", DEV, { paused: true }),
    shopify("product", DEV),
    ...["abandonedCheckout", "company"].map((n) => shopify(n, DEV, { lastError: NOT_REQUESTED })),
    books("invoice"),
    books("payment"),
  ];

  const coverageRow = (connector: string) =>
    screen.getAllByText(connector, { selector: ".os-row-name" })
      .map((el) => el.closest(".os-record-row") as HTMLElement)
      .find((row) => row.querySelector(".os-record-status")?.textContent?.includes("reported")) as HTMLElement;

  it("counts concepts, and nothing outside a connection, in the scene's numbers", async () => {
    mount(fakeConnection({ dataOrigins: SCENE_ORIGINS, syncStatesAll: SCENE_HEALTH }));
    await screen.findByRole("group", { name: STORE });
    const head = screen.getByRole("heading", { name: "Data origins" }).closest(".os-head") as HTMLElement;
    expect(head.querySelector(".os-head-meta")?.textContent).toBe("7");
    expect(screen.getByRole("heading", { name: /^Concepts with a connector/ }).textContent).toBe("Concepts with a connector 7");
    const shop = coverageRow("shopify");
    expect(within(shop).getByText("6 of 6 reported")).toBeTruthy();
    expect(within(shop).getByText("All 6 concepts of its 2 stores have reported; 1 is paused.")).toBeTruthy();
    expect(within(shop).getByText(/1 of them carries an error/)).toBeTruthy();
    expect(within(coverageRow("quickBooks")).getByText("2 of 2 reported")).toBeTruthy();
    expect(screen.getByText("Reported means something has written a health row for that concept, on each store.")).toBeTruthy();
  });

  it("keeps the caption as it was when no connector keeps its stores apart", async () => {
    mount(fakeConnection({ dataOrigins: [mirror("v1:books:invoice", "quickBooks")], syncStatesAll: [books("invoice")] }));
    expect(await screen.findByText("Reported means something has written a health row for that concept.")).toBeTruthy();
  });

  it("reads a store's connector as all paused although what it never asked for is not", async () => {
    mount(
      fakeConnection({
        dataOrigins: [mirror("v1:shopify:order"), mirror("v1:shopify:company")],
        syncStatesAll: [shopify("order", STORE, { paused: true }), shopify("company", STORE, { lastError: NOT_REQUESTED })],
      }),
    );
    expect(await screen.findByText("Every concept of its store is paused. Deliveries are staged and nothing is being applied.")).toBeTruthy();
    expect(screen.getByText("1 of 1 reported")).toBeTruthy();
  });

  it("does not read a connector whose every row is outside its connection as reporting", async () => {
    mount(
      fakeConnection({
        dataOrigins: [mirror("v1:shopify:company")],
        syncStatesAll: [shopify("company", STORE, { lastError: NOT_REQUESTED })],
      }),
    );
    const shop = await screen.findByText("0 of 0 reported");
    expect(shop.getAttribute("data-tone")).toBe("warn");
    expect(screen.getByText("None of its concepts is part of the connection of its store.")).toBeTruthy();
    expect(screen.queryByText(/have reported/)).toBeNull();
  });

  it("names the stores in the partial and the silent sentences", async () => {
    mount(
      fakeConnection({
        dataOrigins: [mirror("v1:shopify:order"), mirror("v1:shopify:product")],
        syncStatesAll: [shopify("order", STORE), shopify("order", DEV)],
      }),
    );
    expect(await screen.findByText("2 of 4 concepts of its 2 stores have reported. The rest have no measurement, which is not the same as no lag.")).toBeTruthy();
  });

  it("offers no act while the health read has not landed, and offers them once it has", async () => {
    const connection = fakeConnection({ dataOrigins: [mirror("v1:shopify:order")] });
    let land!: (rows: unknown) => void;
    connection.query.syncStatesAll.mockImplementationOnce(() => new Promise((resolve) => { land = resolve; }));
    mount(connection);
    await screen.findByText("v1:shopify:order");
    expect(screen.queryByRole("button", { name: /^(Backfill now|Pause|Resume)/ })).toBeNull();
    const { rowsResult } = await import("./harness");
    await act(async () => land(rowsResult([shopify("order", STORE)])));
    const group = await screen.findByRole("group", { name: STORE });
    expect(within(group).getByRole("button", { name: /^Backfill now/ })).toBeTruthy();
    expect(within(group).getByRole("button", { name: /^Pause/ })).toBeTruthy();
  });

  it("offers no act when the health read failed", async () => {
    const connection = fakeConnection({ dataOrigins: [mirror("v1:shopify:order")] });
    connection.query.syncStatesAll.mockRejectedValueOnce(new Error("syncStatesAll: the cluster is busy"));
    mount(connection);
    expect(await screen.findByText("syncStatesAll: the cluster is busy")).toBeTruthy();
    expect(screen.getByText("v1:shopify:order")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^(Backfill now|Pause|Resume)/ })).toBeNull();
  });

  it("says the list is incomplete when a cursor comes back twice, and offers no act", async () => {
    const connection = fakeConnection({ dataOrigins: [mirror("v1:shopify:order")] });
    const { rowsResult } = await import("./harness");
    connection.query.syncStatesAll.mockImplementation(async () => rowsResult([shopify("order", STORE)], "again"));
    mount(connection);
    expect(await screen.findByText("The connector health was not read to its end, so this list is incomplete.")).toBeTruthy();
    expect(connection.query.syncStatesAll).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("button", { name: /^(Backfill now|Pause|Resume)/ })).toBeNull();
  });

  it("stops the health read at 200 pages and says the list is incomplete", async () => {
    const connection = fakeConnection({ dataOrigins: [mirror("v1:shopify:order")] });
    const { rowsResult } = await import("./harness");
    let n = 0;
    connection.query.syncStatesAll.mockImplementation(async () => rowsResult([shopify("order", STORE)], `c${++n}`));
    mount(connection);
    expect(await screen.findByText("The connector health was not read to its end, so this list is incomplete.")).toBeTruthy();
    expect(connection.query.syncStatesAll).toHaveBeenCalledTimes(200);
  });

  it("draws a failing later page as a failed read: the Notice, no store groups, no acts", async () => {
    const connection = fakeConnection({ dataOrigins: [mirror("v1:shopify:order")] });
    const { rowsResult } = await import("./harness");
    connection.query.syncStatesAll.mockImplementation(async (_a: unknown, opts?: { cursor?: string }) => {
      if (opts?.cursor) throw new Error("syncStatesAll: page two timed out");
      return rowsResult([shopify("order", STORE)], "p1");
    });
    mount(connection);
    expect(await screen.findByText("syncStatesAll: page two timed out")).toBeTruthy();
    expect(screen.queryByRole("group", { name: STORE })).toBeNull();
    expect(screen.queryByRole("button", { name: /^(Backfill now|Pause|Resume)/ })).toBeNull();
  });

  it("never puts two connectors that share a store key in one group", async () => {
    mount(
      fakeConnection({
        dataOrigins: [mirror("v1:shopify:order"), mirror("v1:shopify:company"), mirror("v1:books:invoice", "quickBooks"), mirror("v1:books:vendor", "quickBooks")],
        syncStatesAll: [
          shopify("order", STORE),
          shopify("company", STORE, { lastError: NOT_REQUESTED }),
          syncStateRow({ conceptId: "v1:books:invoice", connector: "quickBooks", partitionKey: STORE }),
          syncStateRow({ conceptId: "v1:books:vendor", connector: "quickBooks", partitionKey: STORE, lastError: NOT_REQUESTED }),
        ],
      }),
    );
    await screen.findAllByRole("group", { name: STORE });
    const groups = screen.getAllByRole("group", { name: STORE });
    expect(groups).toHaveLength(2);
    for (const group of groups) {
      const lines = group.querySelectorAll("details");
      expect(lines).toHaveLength(1);
      const own = within(group).queryByText("v1:shopify:order") ? "shopify" : "books";
      expect(lines[0]!.textContent).toContain(own === "shopify" ? "v1:shopify:company" : "v1:books:vendor");
      expect(lines[0]!.textContent).not.toContain(own === "shopify" ? "v1:books:vendor" : "v1:shopify:company");
    }
  });

  it("runs one Try again at a time", async () => {
    const connection = fakeConnection({
      dataOrigins: [mirror("v1:shopify:order")],
      inboundRequestsGivenUp: [
        inboundRequestRow({ id: "in-2", source: "shopify-acme-widgets" }),
        inboundRequestRow({ id: "in-1", source: "shopify-acme-widgets-dev" }),
      ],
    });
    connection.query.datasyncRequeueInbound.mockImplementation(() => new Promise(() => {}));
    mount(connection);
    await screen.findByText("v1:shopify:order");
    const section = screen.getByRole("region", { name: "Changes that could not be recorded" });
    await click(within(section).getByRole("button", { name: "Check" }));
    const list = await within(section).findByRole("list", { name: "Changes that could not be recorded" });
    const own = within(list).getByText("shopify-acme-widgets").closest("li") as HTMLElement;
    const dev = within(list).getByText("shopify-acme-widgets-dev").closest("li") as HTMLElement;
    const press = within(own).getByRole("button", { name: "Try again" });
    await act(async () => { press.click(); press.click(); });
    await click(within(dev).getByRole("button", { name: "Try again" }));
    expect(connection.query.datasyncRequeueInbound).toHaveBeenCalledTimes(1);
  });

  it("says plainly that the list of changes could not be read", async () => {
    const connection = fakeConnection({ dataOrigins: [mirror("v1:shopify:order")] });
    connection.query.inboundRequestsGivenUp.mockRejectedValueOnce(new Error("inboundRequestsGivenUp: the cluster is busy"));
    mount(connection);
    await screen.findByText("v1:shopify:order");
    const section = screen.getByRole("region", { name: "Changes that could not be recorded" });
    await click(within(section).getByRole("button", { name: "Check" }));
    expect(await within(section).findByText("The list could not be read.")).toBeTruthy();
    expect(within(section).getByText("inboundRequestsGivenUp: the cluster is busy")).toBeTruthy();
  });
});
