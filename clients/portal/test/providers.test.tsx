// Settings -> AI providers (epic memql#4440, task memql#4444).
//
// FOUR CLAIMS, and the first is the one the epic exists for: a cluster with no
// AI provider configured is a correctly installed cluster, and this page has
// to say so rather than reading as a fault. The other three are the ways a
// credential surface can quietly betray an operator -- rendering a key back,
// telling an admin they may configure something they may not, and reporting a
// save as though it were in force.

import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { Connection } from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { adminSurfacesFor } from "../src/admin/urls";
import { summarize, toProviderRows } from "../src/admin/useProviders";
import { asQueryClient } from "./support/queryFake";

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
  domain: "",
};

const KEYLESS_ROWS = [
  {
    name: "chat54Mini",
    vendor: "OpenAI",
    model: "gpt-5.4-mini",
    available: false,
    authSource: "unresolved",
    reason: 'auth "apiKey" references MEMQL_AI_OPENAI_API_KEY but no value is in concept storage or OS env.',
  },
  {
    name: "streamClaudeSonnet",
    vendor: "AnthropicStream",
    model: "claude-sonnet",
    available: false,
    authSource: "unresolved",
    reason: 'auth "apiKey" references MEMQL_AI_ANTHROPIC_API_KEY but no value is in concept storage or OS env.',
  },
];

const CONFIGURED_ROWS = [
  { ...KEYLESS_ROWS[0], available: true, authSource: "globalSecret", reason: "" },
  { ...KEYLESS_ROWS[1], available: true, authSource: "federation", reason: "" },
];

function rowsResult(rows: unknown[]) {
  return { rows: () => rows } as unknown;
}

interface Call {
  name: string;
  args: unknown;
}

const VERIFY_PASSES = [{ provider: "chat54Mini", verified: true, reason: "", modelsListed: 12 }];

// A REFUSAL, as the engine returns it: verified=false carrying the vendor's
// own words, NOT a thrown error. The distinction is the page's whole contract
// with this call -- a refusal is something to show in place, and an error is
// an exception dialog about a question that could not be asked.
const VERIFY_REFUSES = [
  {
    provider: "chat54Mini",
    verified: false,
    reason: "models.list failed on the api-key credential path: 401 invalid x-api-key",
    modelsListed: 0,
  },
];

function fakeConnection(
  role: string,
  calls: Call[],
  statusRows: unknown[],
  verifyRows: unknown[] = VERIFY_PASSES,
): Connection {
  const record = (name: string, result: unknown) => async (args: unknown) => {
    calls.push({ name, args });
    return result;
  };
  const query = asQueryClient({
    listConcepts: vi.fn(async () => []),
    getMyAccess: vi.fn(async () => ({
      userId: "user-1",
      primaryEmail: "op@example.test",
      clusterRole: role,
    })),
    providerAuthStatus: vi.fn(record("providerAuthStatus", rowsResult(statusRows))),
    providerKeySet: vi.fn(
      record(
        "providerKeySet",
        rowsResult([
          {
            name: "MEMQL_AI_ANTHROPIC_API_KEY",
            vendor: "anthropic",
            fingerprint: "9f2c",
            applied: false,
            message: "Saved. It takes effect on every node when you Apply.",
          },
        ]),
      ),
    ),
    providerFederationSet: vi.fn(
      record(
        "providerFederationSet",
        rowsResult([{ name: "anthropic-federation", vendor: "anthropic", applied: false, message: "Saved." }]),
      ),
    ),
    providerVerify: vi.fn(record("providerVerify", rowsResult(verifyRows))),
    providersReload: vi.fn(
      record(
        "providersReload",
        rowsResult([{ requestId: "r1", availableOnThisNode: 2, registered: 14, broadcast: true }]),
      ),
    ),
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
  path = "/admin/providers",
  statusRows: unknown[] = KEYLESS_ROWS,
  verifyRows: unknown[] = VERIFY_PASSES,
) {
  const calls: Call[] = [];
  const dial = vi.fn(async () =>
    fakeConnection(role, calls, statusRows, verifyRows),
  ) as unknown as typeof Connection.dial;
  render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("provider tests must make no identity calls");
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

describe("the surface is owner-only, and absent rather than disabled", () => {
  it("offers the tab to an owner and to nobody else", () => {
    const owner = adminSurfacesFor("owner").map((s) => s.id);
    expect(owner).toContain("providers");
    for (const role of ["admin", "developer", "writer", "reader", ""]) {
      expect(adminSurfacesFor(role).map((s) => s.id)).not.toContain("providers");
      // The reachable positive: the OTHER admin tabs are still offered, so
      // this is a filter on one surface rather than an empty strip.
      expect(adminSurfacesFor(role).map((s) => s.id)).toContain("settings");
    }
  });

  it("refuses an admin at the page, naming the floor", async () => {
    const calls = renderAt("admin");
    await waitFor(() => {
      expect(screen.getByText(/cluster-owner surface/i)).toBeTruthy();
    });
    // AND IT ASKS THE SERVER NOTHING. A refused page that still issued the
    // read would be teaching an operator that the console decides.
    expect(calls.some((c) => c.name === "providerAuthStatus")).toBe(false);
  });
});

describe("a cluster with no provider configured", () => {
  it("reads as a choice, not a fault", async () => {
    renderAt("owner");
    await waitFor(() => {
      expect(
        screen.getByText(/No AI provider is configured yet, which is how a cluster is installed/i),
      ).toBeTruthy();
    });
    expect(screen.getByText(/installing spends no inference/i)).toBeTruthy();
  });

  it("lists every provider with the reason it cannot be called", async () => {
    renderAt("owner");
    await waitFor(() => {
      expect(screen.getByText("chat54Mini")).toBeTruthy();
    });
    expect(screen.getByText("streamClaudeSonnet")).toBeTruthy();
    // The constructor's own words, not a rewrite -- they name the variable.
    expect(screen.getAllByText(/MEMQL_AI_OPENAI_API_KEY/).length).toBeGreaterThan(0);
    // Verify is not offered for something that was never configured: it would
    // ask the vendor about a credential that never reached them.
    expect(screen.queryByRole("button", { name: "Verify" })).toBeNull();
  });

  it("leads Anthropic with federation and states the all-or-none rule", async () => {
    renderAt("owner");
    await waitFor(() => {
      expect(screen.getByText(/no API key exists anywhere/i)).toBeTruthy();
    });
    expect(screen.getByText(/All four ids below are required together/i)).toBeTruthy();
    // OpenAI's slot is visibly open rather than silently absent.
    expect(screen.getByText(/no workload-identity option to offer here/i)).toBeTruthy();
  });
});

describe("keys are write-only", () => {
  it("seals a key, renders a fingerprint, and never the value", async () => {
    const calls = renderAt("owner");
    await waitFor(() => {
      expect(screen.getAllByLabelText(/API key/i).length).toBeGreaterThan(0);
    });
    const box = screen.getAllByLabelText(/API key/i)[0] as HTMLInputElement;
    // A password field, so the value is not legible on a shared screen.
    expect(box.getAttribute("type")).toBe("password");

    fireEvent.change(box, { target: { value: "sk-ant-secret-value-nobody-should-see" } });
    fireEvent.click(screen.getAllByRole("button", { name: /Save key/i })[0]!);

    await waitFor(() => {
      expect(screen.getByText(/9f2c/)).toBeTruthy();
    });
    // THE ASSERTION THAT MATTERS: nothing on the page renders what was typed.
    expect(document.body.textContent).not.toContain("sk-ant-secret-value-nobody-should-see");
    // And the box was cleared, so the key is not left sitting in a DOM node
    // behind a page somebody walked away from.
    expect((screen.getAllByLabelText(/API key/i)[0] as HTMLInputElement).value).toBe("");

    const sent = calls.find((c) => c.name === "providerKeySet");
    expect(sent).toBeTruthy();
    // The VENDOR travels, and the row name does not -- the engine derives it,
    // so an operator cannot name a row the resolver never tries.
    expect(Object.keys(sent!.args as object)).toEqual(
      expect.arrayContaining(["vendor", "apiKey"]),
    );
    expect(Object.keys(sent!.args as object)).not.toContain("name");
  });

  it("says saved is not applied", async () => {
    renderAt("owner");
    await waitFor(() => {
      expect(screen.getAllByLabelText(/API key/i).length).toBeGreaterThan(0);
    });
    const box = screen.getAllByLabelText(/API key/i)[0] as HTMLInputElement;
    fireEvent.change(box, { target: { value: "sk-test" } });
    fireEvent.click(screen.getAllByRole("button", { name: /Save key/i })[0]!);
    await waitFor(() => {
      expect(screen.getByText(/takes effect on every node when you Apply/i)).toBeTruthy();
    });
  });
});

describe("apply", () => {
  it("reloads every node and reports what the answering one can now call", async () => {
    const calls = renderAt("owner", "/admin/providers", CONFIGURED_ROWS);
    await waitFor(() => {
      expect(screen.getAllByText(/callable/i).length).toBeGreaterThan(0);
    });
    fireEvent.click(screen.getByRole("button", { name: /Apply to every node/i }));
    await waitFor(() => {
      expect(screen.getByText(/every other node was told to re-resolve/i)).toBeTruthy();
    });
    expect(calls.some((c) => c.name === "providersReload")).toBe(true);
    // The status is re-read after an apply, so the page is not left showing
    // the state that prompted it.
    expect(calls.filter((c) => c.name === "providerAuthStatus").length).toBeGreaterThan(1);
  });

  it("verifies a configured provider against the vendor", async () => {
    const calls = renderAt("owner", "/admin/providers", CONFIGURED_ROWS);
    await waitFor(() => {
      expect(screen.getAllByRole("button", { name: "Verify" }).length).toBe(2);
    });
    fireEvent.click(screen.getAllByRole("button", { name: "Verify" })[0]!);
    await waitFor(() => {
      expect(screen.getByText(/the vendor accepted this credential/i)).toBeTruthy();
    });
    expect(calls.some((c) => c.name === "providerVerify")).toBe(true);
  });

  it("shows a REFUSED credential in place, with the vendor's own words", async () => {
    // The half a happy-path test cannot reach. `providerVerify` returns
    // verified=false rather than erroring, so the page has to turn that into
    // something the operator reads -- and it must carry the vendor's reason,
    // because "verification failed" tells them nothing about which of the
    // half-dozen causes they are looking at.
    renderAt("owner", "/admin/providers", CONFIGURED_ROWS, VERIFY_REFUSES);
    await waitFor(() => {
      expect(screen.getAllByRole("button", { name: "Verify" }).length).toBe(2);
    });
    fireEvent.click(screen.getAllByRole("button", { name: "Verify" })[0]!);
    await waitFor(() => {
      expect(screen.getByText(/401 invalid x-api-key/i)).toBeTruthy();
    });
    // NOT reported as a pass. The failure mode worth guarding is a page that
    // renders the reason somewhere and the success message as well.
    expect(screen.queryByText(/the vendor accepted this credential/i)).toBeNull();
  });

  it("names the credential's SOURCE, which is what a saved-but-not-used key looks like", async () => {
    renderAt("owner", "/admin/providers", [
      { ...CONFIGURED_ROWS[0], authSource: "env" },
      CONFIGURED_ROWS[1],
    ]);
    await waitFor(() => {
      // The one distinction an operator cannot debug without: a key in the
      // pod's environment is the one source a portal write cannot override.
      expect(screen.getByText(/a saved key will not override it/i)).toBeTruthy();
    });
    expect(screen.getByText(/no key at rest/i)).toBeTruthy();
  });
});

describe("the summary is a pure function of the rows", () => {
  it("tells the three states apart", () => {
    expect(summarize([]).tone).toBe("unconfigured");
    expect(summarize(toProviderRows(KEYLESS_ROWS)).tone).toBe("unconfigured");
    expect(summarize(toProviderRows(CONFIGURED_ROWS)).tone).toBe("ready");
    expect(
      summarize(toProviderRows([KEYLESS_ROWS[0]!, CONFIGURED_ROWS[1]!])).tone,
    ).toBe("partial");
  });

  it("never calls an unconfigured cluster broken", () => {
    const { headline } = summarize(toProviderRows(KEYLESS_ROWS));
    expect(headline).toMatch(/how a cluster is installed/i);
    expect(headline).not.toMatch(/error|fail|broken|problem/i);
  });
});
