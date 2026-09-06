import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { conceptOf, fakeConnection, nodeOf, withSession, type FakeConnection } from "./harness";

/** A click that lets the resulting effects settle, so a test never asserts
 *  against a render React has not finished. */
async function click(el: Element): Promise<void> {
  await act(async () => {
    fireEvent.click(el);
  });
}

const h: { connection: FakeConnection | null } = { connection: null };
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  OsConnectionProvider: ({ children }: { children: unknown }) => children,
}));

const { RegistrySection } = await import("../../src/apps/concepts/RegistrySection");
const { DEFAULT_CONCEPTS_SETTINGS } = await import("../../src/apps/concepts/settings");

const ARTIFACT = conceptOf({
  id: "v1:library:artifact",
  description: "An index row over the Library",
});
const ORDER = conceptOf({
  id: "v1:shopify:order",
  dataState: "mirror",
  dataOrigin: "shopify",
  fields: [{ name: "total", kind: "number", required: true, enumValues: [], description: "" }],
});

function mount(connection: FakeConnection, openConceptId?: string) {
  h.connection = connection;
  return render(
    withSession(
      <RegistrySection
        settings={DEFAULT_CONCEPTS_SETTINGS}
        {...(openConceptId === undefined ? {} : { openConceptId })}
      />,
    ),
  );
}

/** Seed the follow subscription with a snapshot, inside act so the render
 *  it causes has settled before anything is asserted. */
async function snapshot(connection: FakeConnection): Promise<void> {
  await act(async () => {
    connection.pushDelta({ generation: 1, added: [ARTIFACT, ORDER], removed: [], reset: true });
  });
}

beforeEach(() => {
  h.connection = null;
});
afterEach(() => {
  vi.clearAllMocks();
});

describe("the registry list", () => {
  it("lists what the follow subscription sent, grouped by domain", async () => {
    const connection = fakeConnection();
    mount(connection);
    await snapshot(connection);
    expect(await screen.findByText("artifact")).toBeTruthy();
    expect(screen.getByText("order")).toBeTruthy();
    expect(connection.query.subscribeConceptRegistry).toHaveBeenCalled();
  });

  it("keeps the domain facet BEHIND Refine (DESIGN.md rule 2)", async () => {
    // The portal stood a horizontal domain chip rail above this list in
    // every session, whether or not anybody was asking a question. That is
    // exactly the filter chrome over content rule 2 exists to remove, so
    // the regression to guard is a domain control that is reachable before
    // the affordance is opened.
    const connection = fakeConnection();
    mount(connection);
    await snapshot(connection);
    await screen.findByText("artifact");

    expect(screen.queryByLabelText("Domain")).toBeNull();

    await click(screen.getByRole("button", { name: /search concepts/i }));
    expect(await screen.findByLabelText("Domain")).toBeTruthy();
  });

  it("a mirror earns a badge and a native concept does not", async () => {
    const connection = fakeConnection();
    mount(connection);
    await snapshot(connection);
    await screen.findByText("artifact");
    // Native is the default and most of the registry is native; badging it
    // would mark almost every row and hide the marks that mean something.
    expect(screen.getByText("Mirror of shopify")).toBeTruthy();
    expect(screen.queryByText(/^Native$/)).toBeNull();
  });

  it("re-snapshots when the delta generation skips (a dropped delta)", async () => {
    // A gap means this browser's registry is wrong in a way waiting cannot
    // repair -- the missing add or remove is never resent.
    const connection = fakeConnection();
    mount(connection);
    await snapshot(connection);
    await screen.findByText("artifact");
    expect(connection.unsubscribed()).toBe(0);

    await act(async () => {
      connection.pushDelta({ generation: 5, added: [], removed: [], reset: false });
    });
    await waitFor(() => expect(connection.unsubscribed()).toBe(1));
    await waitFor(() =>
      expect(connection.query.subscribeConceptRegistry.mock.calls.length).toBeGreaterThan(1),
    );
  });
});

describe("one concept", () => {
  it("replaces the list rather than appending under it (rule 11)", async () => {
    const connection = fakeConnection();
    mount(connection);
    await snapshot(connection);
    await click(await screen.findByText("artifact"));

    // ONE Head. Two in one scroller is the tell that neither rule-11 shape
    // was taken, so the list must be gone.
    expect(screen.queryByText("v1:shopify:order")).toBeNull();
    expect(screen.getByRole("button", { name: /Concepts/ })).toBeTruthy();
  });

  it("says out loud that a mirror refuses writes", async () => {
    const connection = fakeConnection();
    mount(connection);
    await snapshot(connection);
    await click(await screen.findByText("order"));
    expect(await screen.findByText(/order is a mirror\. shopify owns this data\./)).toBeTruthy();
    expect(screen.getByText(/refuses every write/)).toBeTruthy();
  });

  it("names a concept the registry does not hold, rather than showing a blank page", async () => {
    const connection = fakeConnection();
    mount(connection, "v1:gone:missing");
    await snapshot(connection);
    expect(
      await screen.findByText(/declares no concept called v1:gone:missing/),
    ).toBeTruthy();
  });
});

describe("a concept's rows", () => {
  it("distinguishes 'that is all of them' from 'there are more'", async () => {
    const connection = fakeConnection({
      pages: [{ rows: [nodeOf("a", { total: 1 })], cursor: "" }],
    });
    mount(connection);
    await snapshot(connection);
    await click(await screen.findByText("order"));
    expect(await screen.findByText(/All 1 readable rows loaded/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  });

  it("offers Load more while the walk has a cursor", async () => {
    const connection = fakeConnection({
      pages: [{ rows: [nodeOf("a", { total: 1 })], cursor: "next" }],
    });
    mount(connection);
    await snapshot(connection);
    await click(await screen.findByText("order"));
    expect(await screen.findByText(/1 rows loaded, more available/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Load more" })).toBeTruthy();
  });

  it("an empty answer says 'not readable by this account', never 'this concept is empty'", async () => {
    // Row admission decides what reaches this browser, so the stronger
    // sentence would be this window inventing a fact about the cluster.
    const connection = fakeConnection({ pages: [{ rows: [], cursor: "" }] });
    mount(connection);
    await snapshot(connection);
    await click(await screen.findByText("order"));
    expect(
      await screen.findByText(/No rows of this concept are readable by this account/),
    ).toBeTruthy();
  });

  it("COUNTS an arrival instead of splicing it into the paged list", async () => {
    // A keyset walk orders by createdAt asc, so a row created now belongs
    // after pages the walk has not reached. Splicing it would draw it among
    // rows it does not belong between, and the next page would fetch it
    // again.
    const connection = fakeConnection({
      pages: [{ rows: [nodeOf("a", { total: 1 })], cursor: "next" }],
    });
    mount(connection);
    await snapshot(connection);
    await click(await screen.findByText("order"));
    await screen.findByText(/1 rows loaded, more available/);

    await act(async () => {
      connection.subscriptions.emit("v1:shopify:order", { id: "brand-new" });
    });

    expect(await screen.findByText(/1 new since you opened this/)).toBeTruthy();
    // Counted, NOT rendered in the list.
    expect(screen.queryByText("brand-new")).toBeNull();
    expect(screen.getByRole("button", { name: /Reload the rows/ })).toBeTruthy();
  });

  it("surfaces the server's own sentence when the walk is refused", async () => {
    const connection = fakeConnection({ walkError: "conceptBrowse: permission denied" });
    mount(connection);
    await snapshot(connection);
    await click(await screen.findByText("order"));
    expect(await screen.findByText(/conceptBrowse: permission denied/)).toBeTruthy();
  });
});

describe("the walk starts once", () => {
  // A BROWSER FOUND THIS AND THE SUITE COULD NOT (epic memql#5009). Under
  // StrictMode the restart effect and the walk effect both ran on mount, the
  // restart bumped `attempt`, and the walk ran a SECOND time and APPENDED its
  // page to the first one's -- three rows rendered as six, under a footer
  // confidently reporting "All 6 readable rows loaded". A wrong total stated
  // with confidence is the worst shape this surface can take, because the
  // whole point of its four footer states is that "that is all of them" can
  // be trusted.
  //
  // THIS ASSERTS THE CALL COUNT RATHER THAN THE RENDERED ROWS, and that is
  // the difference between a test that catches it and one that does not. The
  // first version of this case rendered under `<StrictMode>` and asserted two
  // `.os-rows-row` elements -- and PASSED with the fix reverted, because
  // jsdom's effect timing lets the duplicate page land before the assertion
  // in a way the browser's does not. The symptom is not reproducible here;
  // the mechanism is. One mount of one concept is ONE browse.
  it("issues exactly one browse per concept, however the effects settle", async () => {
    const connection = fakeConnection({
      pages: [{ rows: [nodeOf("a", { total: 1 }), nodeOf("b", { total: 2 })], cursor: "" }],
    });
    mount(connection, "v1:shopify:order");
    await snapshot(connection);
    await screen.findByText(/All 2 readable rows loaded/);

    const browses = connection.query.executeNamed.mock.calls.filter(
      (call) => call[0] === "conceptBrowse",
    );
    expect(browses.length).toBe(1);
  });
});
