// The reactive grammar (memql#4180), driven against the fake dial the shell
// tests established: the rail mark's connection states, the shaped skeleton
// that replaces the loading text, and the accent wash on a live arrival.
//
// The mark's ANIMATION is CSS keyed off data-conn; what a jsdom test can and
// should pin is the state machine -- which data-conn value each connection
// state produces, and that the accessible name says the same thing in words.

import { describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { Concept, Connection} from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { asQueryClient } from "./support/queryFake";

const CONCEPTS: Concept[] = [
  {
    id: "v1:cluster:node",
    version: "v1",
    domain: "cluster",
    entity: "node",
    description: "A registered node in the cluster",
    type: "concept",
    displayCard: { primary: "name" },
  },
];

type GraphHandler = (event: { kind: string; payload: Record<string, unknown> }) => void;

function fakeConnection({
  browse,
  captureGraph,
}: {
  // browseConceptPage implementation; default returns one empty exhausted page.
  browse?: () => Promise<unknown>;
  // Receives the subscribed graph handler so a test can push CDC events.
  captureGraph?: (handler: GraphHandler) => void;
} = {}): Connection {
  const query = asQueryClient({
    listConcepts: vi.fn(async () => CONCEPTS),
    // The shell reads the caller's access to decide what the rail offers
    // (the Modules item is owner/admin-only, memql#4191).
    getMyAccess: vi.fn(async () => ({
      userId: "user-test",
      primaryEmail: "op@example.test",
      clusterRole: "admin",
    })),
    browseConceptPage: vi.fn(
      browse ?? (async () => ({ rows: [], cursor: "", hasMore: false })),
    ),
  });
  const subscriptions =
    captureGraph === undefined
      ? null
      : {
          subscribeGraph: (handler: GraphHandler) => {
            captureGraph(handler);
            return () => {};
          },
        };
  return {
    nodeId: "bff-test",
    serverVersion: "0.0.0-test",
    query,
    subscriptions,
    close: vi.fn(),
    done: vi.fn(() => new Promise<void>(() => {})),
  } as unknown as Connection;
}

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
};

function renderApp(dial: typeof Connection.dial, path = "/concepts/v1%3Acluster%3Anode") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("the reactive tests must make no identity calls");
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
}

function markState(): string | null {
  const mark = document.querySelector(".mark-connection");
  return mark ? mark.getAttribute("data-conn") : null;
}

describe("the reactive grammar", () => {
  it("renders the rail mark connected when the stream is up, with the state in words", async () => {
    const dial = vi.fn(async () => fakeConnection()) as unknown as typeof Connection.dial;
    renderApp(dial);

    await waitFor(() => expect(markState()).toBe("connected"));
    expect(screen.getByRole("img", { name: "Connected to the cluster" })).toBeTruthy();
  });

  it("dims the mark to offline when the dial fails", async () => {
    const dial = vi.fn(async () => {
      throw new Error("websocket closed before open: code=1006");
    }) as unknown as typeof Connection.dial;
    renderApp(dial);

    await waitFor(() => expect(markState()).toBe("offline"));
    expect(
      screen.getByRole("img", { name: "Not connected to the cluster" }),
    ).toBeTruthy();
  });

  it("shows a shaped row skeleton while the first page is in flight", async () => {
    // A browse that never settles holds the walk in "loading" -- the pane
    // must render content-shaped bars, not a text line, and no footer claim.
    const dial = vi.fn(async () =>
      fakeConnection({ browse: () => new Promise(() => {}) }),
    ) as unknown as typeof Connection.dial;
    renderApp(dial);

    await waitFor(() =>
      expect(document.querySelector('[data-skeleton="rows"]')).toBeTruthy(),
    );
    expect(screen.queryByText(/rows loaded/)).toBeNull();
  });

  it("marks the mark streaming and washes the live band when a row arrives", async () => {
    let graphHandler: GraphHandler | null = null;
    const dial = vi.fn(async () =>
      fakeConnection({ captureGraph: (handler) => (graphHandler = handler) }),
    ) as unknown as typeof Connection.dial;
    renderApp(dial);

    await waitFor(() => expect(markState()).toBe("connected"));
    await waitFor(() => expect(graphHandler).not.toBeNull());

    act(() => {
      // The SDK delivers Event.kind with the EVENT_KIND_ prefix stripped --
      // "NODE_CREATED" is the wire spelling the band switches on.
      graphHandler?.({
        kind: "NODE_CREATED",
        payload: {
          id: "v1:cluster:node:live-1",
          concept: "v1:cluster:node",
          name: "a live arrival",
        },
      });
    });

    // The arrival lands in the band above the list, washed in accent -- never
    // spliced into the cursor walk -- and the mark reports streaming.
    await waitFor(() => expect(screen.getByText(/New since you opened this/)).toBeTruthy());
    expect(document.querySelector(".row-wash")).toBeTruthy();
    await waitFor(() => expect(markState()).toBe("streaming"));
  });
});
