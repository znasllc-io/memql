// The Data origins surface (epic memql#4378): the badge's three states and
// the one it refuses to guess, a mirror's read-only notice on the concept
// header, the page's cluster-owner gating, and the two dead-letter verbs
// going through a confirmation.

import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { Connection } from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { originBadgeLabel, isMirror } from "../src/dataorigins/OriginBadge";
import { connectorsIn } from "../src/dataorigins/DataOriginsPage";
import { healthFor, toOriginRows, toSyncStateRows } from "../src/dataorigins/useDataOrigins";
import { asQueryClient } from "./support/queryFake";

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
  domain: "",
};

const MIRROR_CONCEPT = {
  id: "v1:shopify:product",
  version: "v1",
  domain: "shopify",
  entity: "product",
  description: "The thin Shopify product index.",
  type: "object",
  dataState: "mirror",
  dataOrigin: "shopify",
  dataMirroredTo: [],
};

const NATIVE_CONCEPT = {
  id: "v1:planner:plan",
  version: "v1",
  domain: "planner",
  entity: "plan",
  description: "A unit of work.",
  type: "object",
  dataState: "native",
  dataOrigin: "memql",
  dataMirroredTo: [],
};

const INVENTORY_ROWS = [
  {
    conceptId: "v1:shopify:product",
    dataState: "mirror",
    origin: "shopify",
    mirroredTo: [],
    connectors: ["shopify"],
  },
  {
    conceptId: "v1:planner:plan",
    dataState: "native",
    origin: "memql",
    mirroredTo: [],
    connectors: [],
  },
];

const HEALTH_ROWS = [
  {
    id: "v1:shopify:product|shopify|inbound",
    conceptId: "v1:shopify:product",
    connector: "shopify",
    direction: "inbound",
    backfillStatus: "complete",
    lagSeconds: 42,
    driftCount: 3,
    outboxDepth: 0,
    deadLetterCount: 1,
    paused: false,
    lastError: "",
  },
];

const DEAD_LETTERS = [
  {
    id: "v1:platform:outboxEntry:abc",
    conceptId: "v1:wholesale:priceList",
    rowRef: "v1:wholesale:priceList:p1",
    action: "upsert",
    version: "2026-08-23T12:00:00Z",
    target: "shopify",
    attempts: 8,
    lastError: "receiver refused",
  },
];

function rowsResult(rows: unknown[]) {
  return { rows: () => rows } as unknown;
}

function fakeConnection(
  role: string,
  calls: Array<{ name: string; args: unknown }>,
  concepts: unknown[],
  inventory: unknown[],
): Connection {
  const query = asQueryClient({
    listConcepts: vi.fn(async () => concepts),
    getMyAccess: vi.fn(async () => ({
      userId: "user-1",
      primaryEmail: "op@example.test",
      clusterRole: role,
    })),
    dataOrigins: vi.fn(async (args: unknown) => {
      calls.push({ name: "dataOrigins", args });
      return rowsResult(inventory);
    }),
    syncStatesAll: vi.fn(async (args: unknown) => {
      calls.push({ name: "syncStatesAll", args });
      return rowsResult(HEALTH_ROWS);
    }),
    outboxDeadLetters: vi.fn(async (args: unknown) => {
      calls.push({ name: "outboxDeadLetters", args });
      return rowsResult(DEAD_LETTERS);
    }),
    datasyncStartBackfill: vi.fn(async (args: unknown) => {
      calls.push({ name: "datasyncStartBackfill", args });
      return rowsResult([]);
    }),
    datasyncSetSyncPaused: vi.fn(async (args: unknown) => {
      calls.push({ name: "datasyncSetSyncPaused", args });
      return rowsResult([]);
    }),
    datasyncRetryOutboxEntry: vi.fn(async (args: unknown) => {
      calls.push({ name: "datasyncRetryOutboxEntry", args });
      return rowsResult([]);
    }),
    datasyncDiscardOutboxEntry: vi.fn(async (args: unknown) => {
      calls.push({ name: "datasyncDiscardOutboxEntry", args });
      return rowsResult([]);
    }),
  });
  return {
    nodeId: "bff-test",
    serverVersion: "0.0.0-test",
    query,
    dispatcher: {
      send: vi.fn(),
      addEventListener: vi.fn(() => () => {}),
      registerStream: vi.fn(() => () => {}),
      sendAndWait: vi.fn(async () => ({ correlateTo: "x" })),
    },
    close: vi.fn(),
    done: vi.fn(() => new Promise<void>(() => {})),
  } as unknown as Connection;
}

function renderAt(
  role: string,
  path: string,
  calls: Array<{ name: string; args: unknown }> = [],
  concepts: unknown[] = [MIRROR_CONCEPT, NATIVE_CONCEPT],
  inventory: unknown[] = INVENTORY_ROWS,
) {
  const dial = vi.fn(async () =>
    fakeConnection(role, calls, concepts, inventory),
  ) as unknown as typeof Connection.dial;
  render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("data-origins tests must make no identity calls");
        }}
        storage={null}
        navigate={() => {}}
        redirectUri="https://api.example.com/portal/auth/callback"
      >
        <ClusterProvider dial={dial}>
          <AppRoutes />
        </ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
  return calls;
}

describe("the origin badge", () => {
  it("says one thing per state and NOTHING when the server did not say", () => {
    expect(originBadgeLabel({ dataState: "mirror", dataOrigin: "shopify" })).toBe(
      "Mirror of shopify",
    );
    expect(
      originBadgeLabel({ dataState: "origin", dataMirroredTo: ["shopify", "quickBooks"] }),
    ).toBe("Origin → shopify, quickBooks");
    expect(originBadgeLabel({ dataState: "native" })).toBe("Native");
    // A server that predates the fields sends "". Rendering "Native" for it
    // would be a guess, and a wrong badge is worse than none.
    expect(originBadgeLabel({ dataState: "" })).toBe("");
    expect(originBadgeLabel({})).toBe("");
  });

  it("answers `is this writable` from the STATE, not from the origin name", () => {
    expect(isMirror({ dataState: "mirror" })).toBe(true);
    // An ORIGIN mirrored TO shopify carries the same connector name and is
    // perfectly writable -- reading the name instead of the state would
    // refuse it.
    expect(isMirror({ dataState: "origin" })).toBe(false);
    expect(isMirror({ dataState: "native" })).toBe(false);
    expect(isMirror(null)).toBe(false);
  });
});

describe("the concept header", () => {
  it("badges a mirror and says why it is read-only", async () => {
    renderAt("owner", "/concepts/v1%3Ashopify%3Aproduct");
    await waitFor(() => expect(screen.getByText("Mirror of shopify")).toBeTruthy());
    expect(screen.getByText(/Read-only: a mirror of shopify/)).toBeTruthy();
    expect(screen.getByText(/change the record at the origin/)).toBeTruthy();
  });

  it("badges a native concept and offers no such notice", async () => {
    renderAt("owner", "/concepts/v1%3Aplanner%3Aplan");
    await waitFor(() => expect(screen.getByText("Native")).toBeTruthy());
    expect(screen.queryByText(/Read-only: a mirror/)).toBeNull();
  });
});

describe("the data origins page", () => {
  it("refuses below cluster owner and issues no read", async () => {
    const calls = renderAt("reader", "/data-origins");
    await waitFor(() =>
      expect(screen.getByText("This is an owner and admin surface")).toBeTruthy(),
    );
    expect(calls.some((c) => c.name === "dataOrigins")).toBe(false);
    expect(screen.queryByRole("link", { name: /Data origins/ })).toBeNull();
  });

  it("lists only the concepts that name a connector, with their health", async () => {
    renderAt("owner", "/data-origins");
    await waitFor(() => expect(screen.getByText("v1:shopify:product")).toBeTruthy());
    // The native concept is declared but has no connector, so it is not a
    // domain and does not take a row.
    expect(screen.queryByText("v1:planner:plan")).toBeNull();
    expect(screen.getByText("42s")).toBeTruthy();
    expect(screen.getByText("1 dead")).toBeTruthy();
    expect(screen.getByRole("link", { name: /Data origins/ })).toBeTruthy();
  });

  it("drives backfill and pause for the domain they belong to", async () => {
    const calls = renderAt("owner", "/data-origins");
    await waitFor(() => expect(screen.getByText("Backfill now")).toBeTruthy());

    fireEvent.click(screen.getByText("Backfill now"));
    await waitFor(() => expect(calls.some((c) => c.name === "datasyncStartBackfill")).toBe(true));
    expect(calls.find((c) => c.name === "datasyncStartBackfill")?.args).toEqual({
      connector: "shopify",
      conceptId: "v1:shopify:product",
    });

    fireEvent.click(screen.getByText("Pause"));
    await waitFor(() => expect(calls.some((c) => c.name === "datasyncSetSyncPaused")).toBe(true));
    expect(calls.find((c) => c.name === "datasyncSetSyncPaused")?.args).toEqual({
      connector: "shopify",
      conceptId: "v1:shopify:product",
      paused: true,
    });
  });

  it("puts both dead-letter verbs behind a confirmation", async () => {
    const calls = renderAt("owner", "/data-origins");
    // Wait for the INVENTORY, not merely for the button. The queue is read
    // per connector and the connector list comes from the inventory, so
    // clicking Load before it lands asks nobody and reports a clean queue --
    // which is why the button is disabled until then.
    await waitFor(() => expect(screen.getByText("v1:shopify:product")).toBeTruthy());

    fireEvent.click(screen.getByText("Load"));
    await waitFor(() => expect(screen.getByText("v1:wholesale:priceList")).toBeTruthy());

    // Discard states the consequence before it fires, and fires nothing
    // until it is confirmed.
    fireEvent.click(screen.getByText("Discard"));
    await waitFor(() => expect(screen.getByText("Discard this change?")).toBeTruthy());
    expect(screen.getByText(/will never reach the other system/)).toBeTruthy();
    expect(calls.some((c) => c.name === "datasyncDiscardOutboxEntry")).toBe(false);

    // Scoped to the DIALOG: the list row's own Discard button is still on
    // screen behind it, and clicking that one would prove nothing about the
    // confirmation.
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Discard" }));
    await waitFor(() =>
      expect(calls.some((c) => c.name === "datasyncDiscardOutboxEntry")).toBe(true),
    );
    expect(calls.find((c) => c.name === "datasyncDiscardOutboxEntry")?.args).toMatchObject({
      entryId: "v1:platform:outboxEntry:abc",
    });
  });

  it("will not read the queue when there is no connector to ask", async () => {
    // An inventory whose concepts name no connector. The queue is read PER
    // CONNECTOR, so there is nobody to ask -- and a Load that resolved
    // instantly against an empty list would render "Nothing is
    // dead-lettered", which is a silent wrong answer: the operator would see
    // a clean queue because the page asked nobody, not because nothing is
    // stuck. The button says so instead.
    const calls = renderAt("owner", "/data-origins", [], [NATIVE_CONCEPT], [
      {
        conceptId: "v1:planner:plan",
        dataState: "native",
        origin: "memql",
        mirroredTo: [],
        connectors: [],
      },
    ]);
    await waitFor(() => expect(screen.getByText("Load")).toBeTruthy());

    const load = screen.getByText("Load").closest("button");
    expect(load?.disabled).toBe(true);

    fireEvent.click(screen.getByText("Load"));
    expect(calls.some((c) => c.name === "outboxDeadLetters")).toBe(false);
    expect(screen.queryByText(/Nothing is dead-lettered/)).toBeNull();
  });
});

describe("the page's row shaping", () => {
  it("absorbs the shapes a materialized row actually carries", () => {
    const rows = toSyncStateRows([
      { conceptId: "c", connector: "k", lagSeconds: "42", paused: "true", driftCount: 3 },
    ]);
    expect(rows[0]!.lagSeconds).toBe(42);
    expect(rows[0]!.paused).toBe(true);
    expect(rows[0]!.driftCount).toBe(3);
    // A field the row does not carry is zero, not a crash: a partially
    // written health row must not stall the page.
    expect(rows[0]!.outboxDepth).toBe(0);
    expect(rows[0]!.backfillStatus).toBe("");
  });

  it("indexes health by (concept, connector) and reports a never-worked domain as ABSENT", () => {
    const lookup = healthFor(toSyncStateRows(HEALTH_ROWS));
    expect(lookup("v1:shopify:product", "shopify")?.lagSeconds).toBe(42);
    // Absent, not zeros: "never run" and "ran and found nothing" are
    // different answers and the table renders them differently.
    expect(lookup("v1:shopify:product", "quickBooks")).toBeNull();
  });

  it("collects the connectors the inventory names, sorted and deduplicated", () => {
    const origins = toOriginRows([
      { conceptId: "a", connectors: ["shopify"] },
      { conceptId: "b", connectors: ["quickBooks", "shopify"] },
      { conceptId: "c", connectors: [] },
    ]);
    expect(connectorsIn(origins)).toEqual(["quickBooks", "shopify"]);
  });
});
