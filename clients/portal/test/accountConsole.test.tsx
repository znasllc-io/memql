// Account management (memql#3322), against a fake cluster.
//
// WHAT THIS FILE OWNS. The Go side proves the gate: a non-owner cannot read or
// mutate an account, and every mint and revoke is audited. Those are server
// properties and they are tested where they are enforced. What no Go test can
// see is what the BROWSER does with a plaintext credential it is handed once,
// and that is the property this file exists for:
//
//   * the plaintext appears exactly once, on the mint reply
//   * it is never written to storage, never put in a URL, and never survives
//     the operator dismissing it
//   * a re-read of the credential list does not bring it back
//
// The second and third are the ones that go wrong quietly. A token stashed in
// localStorage "so the copy button still works after a refresh" is a 43-char
// bearer sitting in a place every script on the origin can read, and nothing
// about the UI looks different.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import {
  Result,
  type AccessSummary,
  type Concept,
  type Connection,
  type QueryClient,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { AccountConsole } from "../src/accounts/AccountConsole";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";

const ACCOUNT = "v1:identity:account";

const ACCOUNT_CONCEPT: Concept = {
  id: ACCOUNT,
  entity: "account",
  displayCard: {
    primary: "name",
    secondary: "description",
    tertiary: "primaryContactEmail",
    status: "status",
  },
} as unknown as Concept;

const ACCOUNT_ROWS: Row[] = [
  {
    id: "acct-1",
    name: "Northwind Trading",
    status: "active",
    description: "Runs their storefront search",
    primaryContactName: "Ada Fournier",
    primaryContactEmail: "ada@northwind.example",
    externalRef: "CRM-4471",
  } as unknown as Row,
];

// Shaped as the wire shapes a node -- payload nested under `payload` -- because
// that is what Result.rows() reads. A flat fixture would type-check, render
// "(unlabelled)", and quietly assert nothing about the projection.
function node(concept: string, id: string, payload: Record<string, unknown>): Row {
  return {
    id,
    concept,
    type: "concept",
    createdBy: "user-1",
    createdAt: "2026-08-08T10:00:00Z",
    payload,
  } as unknown as Row;
}

const TOKEN_ROWS: Row[] = [
  node("v1:identity:identity", "ident-1", {
    label: "Nightly export",
    active: true,
    accountId: "acct-1",
  }),
];

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "portal",
  authEnabled: false,
};

const PLAINTEXT = "mql_acct_2f4d6a8c0e1b3d5f7a9c1e3b5d7f9a1c3e5b7d9";

function renderConsole(opts: { mintFails?: boolean } = {}) {
  const access: AccessSummary = {
    requestId: "r1",
    userId: "user-1",
    primaryEmail: "ada@example.com",
    clusterRole: "owner",
  } as unknown as AccessSummary;

  const calls: string[] = [];
  const executeNamed = vi.fn(async (name: string, call: string) => {
    calls.push(call);
    if (name === "accountTokensForAccount") {
      return new Result({ bundle: { nodes: TOKEN_ROWS }, meta: { cursor: "" } });
    }
    if (name === "accounts") {
      return new Result({ bundle: { nodes: ACCOUNT_ROWS }, meta: { cursor: "" } });
    }
    return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
  });

  // The mint reply. This is the ONLY place the plaintext ever exists, which is
  // exactly the shape the server contract promises: only the SHA-256 hash was
  // persisted, so there is no second read that could return it.
  const sent: unknown[] = [];
  const sendAndWait = vi.fn(async (envelope: Record<string, unknown>) => {
    sent.push(envelope);
    if ("createAccountToken" in envelope) {
      if (opts.mintFails) {
        return { queryError: { error: { message: "refused" } } };
      }
      const req = envelope["createAccountToken"] as Record<string, unknown>;
      return {
        createAccountTokenResult: {
          requestId: req["requestId"],
          success: true,
          plainToken: PLAINTEXT,
          identityId: "ident-2",
          accountId: "acct-1",
          subjectUserId: "user-1",
          auditEventId: "audit-9",
        },
      };
    }
    return { revokeAccountTokenResult: { success: true, auditEventId: "audit-10" } };
  });

  const query = {
    listConcepts: vi.fn(async () => [ACCOUNT_CONCEPT]),
    getMyAccess: vi.fn(async () => access),
    executeNamed,
  } as unknown as QueryClient;

  const dial = vi.fn(
    async () =>
      ({
        nodeId: "bff-test",
        serverVersion: "0.0.0-test",
        query,
        dispatcher: { sendAndWait },
        close: vi.fn(),
        done: vi.fn(() => new Promise<void>(() => {})),
      }) as unknown as Connection,
  ) as unknown as typeof Connection.dial;

  const utils = render(
    <MemoryRouter initialEntries={["/views/customers/rows/acct-1"]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("the account tests must make no identity calls");
        }}
        storage={null}
        navigate={() => {}}
        redirectUri="https://cockpit.example.com/portal/auth/callback"
      >
        <ClusterProvider dial={dial}>
          <AccountConsole concept={ACCOUNT_CONCEPT} rows={ACCOUNT_ROWS} selectedRowId="acct-1" />
        </ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );

  return { ...utils, calls, sent };
}

// A real Storage, so "nothing was written" is a measured fact about an object
// that WOULD have recorded a write rather than an assertion about a null.
class RecordingStorage implements Storage {
  private readonly data = new Map<string, string>();
  readonly writes: string[] = [];
  get length(): number {
    return this.data.size;
  }
  clear(): void {
    this.data.clear();
  }
  getItem(key: string): string | null {
    return this.data.get(key) ?? null;
  }
  key(index: number): string | null {
    return [...this.data.keys()][index] ?? null;
  }
  removeItem(key: string): void {
    this.data.delete(key);
  }
  setItem(key: string, value: string): void {
    this.writes.push(value);
    this.data.set(key, value);
  }
}

describe("the account console", () => {
  let storage: RecordingStorage;

  beforeEach(() => {
    storage = new RecordingStorage();
    vi.stubGlobal("localStorage", storage);
    vi.stubGlobal("sessionStorage", storage);
  });

  it("mints a credential, shows the plaintext once, and forgets it on dismiss", async () => {
    renderConsole();

    await waitFor(() => expect(screen.getByPlaceholderText("Nightly export job")).toBeTruthy());
    fireEvent.change(screen.getByPlaceholderText("Nightly export job"), {
      target: { value: "CI runner" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Issue credential" }));

    // Shown once...
    await waitFor(() => expect(screen.getByText(PLAINTEXT)).toBeTruthy());
    expect(screen.getByText(/You will not see it again/)).toBeTruthy();
    // ...and honest about what it authenticates as: the operator's user, never
    // the account. Nothing signs in as a customer.
    expect(screen.getByText("user-1")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "I have copied it" }));

    // ...and gone. Not hidden behind a toggle -- absent from the document.
    await waitFor(() => expect(screen.queryByText(PLAINTEXT)).toBeNull());
  });

  it("never writes the plaintext to storage or a URL", async () => {
    renderConsole();

    await waitFor(() => expect(screen.getByPlaceholderText("Nightly export job")).toBeTruthy());
    // A label is required -- the console refuses an unnamed credential rather
    // than minting one nobody can later identify in an audit.
    fireEvent.change(screen.getByPlaceholderText("Nightly export job"), {
      target: { value: "CI runner" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Issue credential" }));
    await waitFor(() => expect(screen.getByText(PLAINTEXT)).toBeTruthy());

    // The two places a 43-char bearer must never end up. Both are asserted on
    // the REAL objects, while the plaintext is on screen -- the moment it
    // would have to have been written for a refresh to survive.
    expect(storage.writes.some((written) => written.includes(PLAINTEXT))).toBe(false);
    expect(window.location.href.includes(PLAINTEXT)).toBe(false);
    expect(window.location.search.includes("mql_acct_")).toBe(false);
  });

  it("does not recover the plaintext from a re-read of the credential list", async () => {
    renderConsole();

    await waitFor(() => expect(screen.getByPlaceholderText("Nightly export job")).toBeTruthy());
    // A label is required -- the console refuses an unnamed credential rather
    // than minting one nobody can later identify in an audit.
    fireEvent.change(screen.getByPlaceholderText("Nightly export job"), {
      target: { value: "CI runner" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Issue credential" }));
    await waitFor(() => expect(screen.getByText(PLAINTEXT)).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "I have copied it" }));
    await waitFor(() => expect(screen.queryByText(PLAINTEXT)).toBeNull());

    // The list re-reads after a mint. It comes back from accountTokenSummary,
    // which projects no secret at all -- so there is nothing for a second read
    // to leak, and the assertion pins that rather than trusting it.
    await waitFor(() => expect(screen.getByText("Nightly export")).toBeTruthy());
    expect(screen.queryByText(PLAINTEXT)).toBeNull();
  });

  it("reports a refused mint instead of rendering a blank credential", async () => {
    renderConsole({ mintFails: true });

    await waitFor(() => expect(screen.getByPlaceholderText("Nightly export job")).toBeTruthy());
    // A label is required -- the console refuses an unnamed credential rather
    // than minting one nobody can later identify in an audit.
    fireEvent.change(screen.getByPlaceholderText("Nightly export job"), {
      target: { value: "CI runner" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Issue credential" }));

    await waitFor(() => expect(screen.getByText(/That did not work/)).toBeTruthy());
    expect(screen.queryByText(/You will not see it again/)).toBeNull();
  });

  it("sends the account id on the mint, and never an owner", async () => {
    const { sent } = renderConsole();

    await waitFor(() => expect(screen.getByPlaceholderText("Nightly export job")).toBeTruthy());
    // A label is required -- the console refuses an unnamed credential rather
    // than minting one nobody can later identify in an audit.
    fireEvent.change(screen.getByPlaceholderText("Nightly export job"), {
      target: { value: "CI runner" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Issue credential" }));
    await waitFor(() => expect(screen.getByText(PLAINTEXT)).toBeTruthy());

    const mint = sent.find(
      (envelope) => typeof envelope === "object" && envelope !== null && "createAccountToken" in envelope,
    ) as Record<string, Record<string, unknown>>;
    expect(mint["createAccountToken"]?.["accountId"]).toBe("acct-1");
    // ownerUserId is @serverSet and stamped from the actor. A client that sent
    // one would be claiming an owner, which is the thing the tier exists to
    // make impossible -- so it must not appear even hopefully.
    expect(JSON.stringify(mint)).not.toContain("ownerUserId");
  });

  it("offers a reader nothing to press, and says why", async () => {
    // A reader is refused by the coarse write gate before row-authz is
    // consulted, so a button here would be a guaranteed failure dressed as an
    // affordance. This is a courtesy, NOT the gate -- the engine refuses the
    // write whatever this renders.
    const access = {
      requestId: "r1",
      userId: "user-9",
      primaryEmail: "reader@example.com",
      clusterRole: "reader",
    } as unknown as AccessSummary;

    const query = {
      listConcepts: vi.fn(async () => [ACCOUNT_CONCEPT]),
      getMyAccess: vi.fn(async () => access),
      executeNamed: vi.fn(
        async () => new Result({ bundle: { nodes: [] }, meta: { cursor: "" } }),
      ),
    } as unknown as QueryClient;

    const dial = vi.fn(
      async () =>
        ({
          nodeId: "bff-test",
          serverVersion: "0.0.0-test",
          query,
          dispatcher: { sendAndWait: vi.fn() },
          close: vi.fn(),
          done: vi.fn(() => new Promise<void>(() => {})),
        }) as unknown as Connection,
    ) as unknown as typeof Connection.dial;

    render(
      <MemoryRouter initialEntries={["/views/customers"]}>
        <AuthProvider
          config={AUTH_DISABLED_CLUSTER}
          fetchImpl={async () => {
            throw new Error("no identity calls");
          }}
          storage={null}
          navigate={() => {}}
          redirectUri="https://cockpit.example.com/portal/auth/callback"
        >
          <ClusterProvider dial={dial}>
            <AccountConsole concept={ACCOUNT_CONCEPT} rows={ACCOUNT_ROWS} selectedRowId="" />
          </ClusterProvider>
        </AuthProvider>
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText(/can read customers but not change them/)).toBeTruthy());
    expect(screen.queryByRole("button", { name: "New customer" })).toBeNull();
  });
});
