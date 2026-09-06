import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { ReactNode } from "react";

// Settings -> AI providers (epic memql#4984). The surface the portal's admin
// console used to hold, and the one a cluster cannot be configured without.
//
// The planted key is long and distinctive: the "never rendered back" sweep at
// the bottom skips nothing, and a short planted value would make it vacuous.
const PLANTED_KEY = "sk-PLANTED-PROVIDER-KEY-DO-NOT-EMIT-000000";

const h = vi.hoisted(() => {
  const reply = (rows: unknown[]) => ({ rows: () => rows });
  const state = {
    providers: [] as unknown[],
    providerError: null as Error | null,
    keySetCalls: [] as { vendor: string; apiKey: string }[],
    federationCalls: [] as Record<string, string>[],
    verifyReply: { verified: true, reason: "" } as Record<string, unknown>,
    reloadCalls: 0,
  };
  const connection = {
    nodeId: "bff-test",
    engineVersion: "v9.9.9",
    engineCommit: "abcdef123456",
    subscriptions: null,
    dispatcher: null,
    query: {
      providerAuthStatus: vi.fn(async () => {
        if (state.providerError) throw state.providerError;
        return reply(state.providers);
      }),
      providerKeySet: vi.fn(async (args: { vendor: string; apiKey: string }) => {
        state.keySetCalls.push(args);
        return reply([{ name: `${args.vendor}Key`, fingerprint: "ab12cd", message: "Stored." }]);
      }),
      providerFederationSet: vi.fn(async (args: Record<string, string>) => {
        state.federationCalls.push(args);
        return reply([{ message: "Federation ids stored." }]);
      }),
      providerVerify: vi.fn(async () => reply([state.verifyReply])),
      providersReload: vi.fn(async () => {
        state.reloadCalls += 1;
        return reply([{ availableOnThisNode: 1, registered: 2 }]);
      }),
    },
    onStatusChange: () => () => {},
  };
  return { connection, state };
});

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { SessionProvider } = await import("../../src/chrome/access");
const { OsProvider } = await import("../../src/chrome/state");
const { OS_REGISTRY } = await import("../../src/apps/registry");
const { SettingsApp } = await import("../../src/apps/settings/SettingsApp");
const { LocalDesktopStore } = await import("../../src/system/store");
const { UNKNOWN_RUNTIME_CONFIG } = await import("../../src/cluster/config");
const { summarize } = await import("../../src/apps/settings/providerFacts");

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

function wrap(children: ReactNode, role: string) {
  return (
    <SessionProvider
      value={{
        access: { userId: "u-1", primaryEmail: "owner@example.com", clusterRole: role },
        config: {
          ...UNKNOWN_RUNTIME_CONFIG,
          domain: "example.com",
          identityUrl: "https://identity.example.com",
        },
      }}
    >
      <OsProvider
        registry={OS_REGISTRY}
        actorRole={role}
        grid={{ cols: 12, rows: 8 }}
        store={new LocalDesktopStore(memStorage())}
      >
        {children}
      </OsProvider>
    </SessionProvider>
  );
}

async function renderProviders(role = "owner") {
  const view = render(
    wrap(<SettingsApp sectionId="providers" navigate={vi.fn()} askContext={vi.fn()} />, role),
  );
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
  return view;
}

beforeEach(() => {
  // PER TEST, because vitest isolates modules per FILE and not per test: the
  // spies below are module state, so a click in one case is still recorded
  // when the next one asks whether anything was called. `clearAllMocks` clears
  // the call log and keeps the implementations `vi.fn(impl)` installed.
  vi.clearAllMocks();
  h.state.providers = [
    {
      name: "chat54Mini",
      vendor: "openai",
      model: "gpt-5.4-mini",
      available: true,
      authSource: "globalSecret",
      reason: "",
    },
    {
      name: "streamClaudeSonnet",
      vendor: "anthropic",
      model: "claude-sonnet-5",
      available: false,
      authSource: "unresolved",
      reason: "no credential configured",
    },
  ];
  h.state.providerError = null;
  h.state.keySetCalls = [];
  h.state.federationCalls = [];
  h.state.verifyReply = { verified: true, reason: "" };
  h.state.reloadCalls = 0;
});

describe("the provider summary", () => {
  // Pure, so the three states are pinned without rendering. The keyless case
  // is the one that matters: it is how a correctly-installed cluster starts.
  it("calls an empty registry the install state, not a fault", () => {
    expect(summarize([]).tone).toBe("unconfigured");
    expect(summarize([]).headline).toMatch(/how a cluster is installed/);
  });

  it("reports a partial registry as partial rather than ready", () => {
    const rows = [
      { name: "a", vendor: "openai", model: "m", available: true, authSource: "env", reason: "" },
      { name: "b", vendor: "anthropic", model: "m", available: false, authSource: "unresolved", reason: "" },
    ];
    expect(summarize(rows)).toEqual({ tone: "partial", headline: "1 of 2 providers can be called." });
  });

  it("calls a registry with no callable provider unconfigured, however many are registered", () => {
    // The trap this pins: `total > 0` is not the same question as "can this
    // cluster call a model". Two registered providers that both fail to
    // resolve is the keyless state wearing a count.
    const rows = [
      { name: "a", vendor: "openai", model: "m", available: false, authSource: "unresolved", reason: "" },
      { name: "b", vendor: "anthropic", model: "m", available: false, authSource: "unresolved", reason: "" },
    ];
    expect(summarize(rows).tone).toBe("unconfigured");
  });
});

describe("Settings -> AI providers", () => {
  it("says what can be called, and names the credential source per provider", async () => {
    await renderProviders();
    expect(screen.getByText("1 of 2 providers can be called.")).toBeTruthy();
    const registry = screen.getByRole("region", { name: "What this node can call" });
    expect(within(registry).getByText(/a sealed row in this cluster/)).toBeTruthy();
    expect(within(registry).getByText(/nothing configured/)).toBeTruthy();
  });

  it("posts a key and never renders one back", async () => {
    const { container } = await renderProviders();
    const openai = screen.getByRole("region", { name: "OpenAI" });
    fireEvent.change(within(openai).getByLabelText("OpenAI API key (new value)"), {
      target: { value: PLANTED_KEY },
    });
    fireEvent.click(within(openai).getByRole("button", { name: "Seal key" }));
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(h.state.keySetCalls).toEqual([{ vendor: "openai", apiKey: PLANTED_KEY }]);
    // THE FIELD IS CLEARED AND THE REPLY CARRIES A FINGERPRINT, NOT A KEY.
    // There is no read-back call anywhere in this surface, so the only way
    // the plaintext could reappear is the field holding it -- which is why
    // the sweep below covers the whole rendered tree rather than the reply.
    expect(container.innerHTML).not.toContain(PLANTED_KEY);
    expect(screen.getByText(/ab12cd/)).toBeTruthy();

    // The negative control. Without it this asserts only that the sweep ran.
    expect(container.innerHTML).toContain("chat54Mini");
  });

  it("will not save a partial federation set", async () => {
    await renderProviders();
    const anthropic = screen.getByRole("region", { name: "Anthropic" });
    fireEvent.change(within(anthropic).getByLabelText("Federation rule id"), {
      target: { value: "fdrl_1" },
    });
    // Three of the four required ids are still blank. A partial set REFUSES
    // BOOT, so accepting one here would take the fleet down at its next
    // restart, hours after the save that caused it.
    expect(
      within(anthropic).getByRole("button", { name: "Save federation" }).hasAttribute("disabled"),
    ).toBe(true);
    expect(h.state.federationCalls).toEqual([]);
  });

  it("saves a complete federation set without the optional workspace", async () => {
    await renderProviders();
    const anthropic = screen.getByRole("region", { name: "Anthropic" });
    for (const [label, value] of [
      ["Federation rule id", "fdrl_1"],
      ["Organization id", "org-1"],
      ["Service account id", "sa-1"],
      ["Projected token path", "/var/run/secrets/anthropic/token"],
    ] as const) {
      fireEvent.change(within(anthropic).getByLabelText(label), { target: { value } });
    }
    fireEvent.click(within(anthropic).getByRole("button", { name: "Save federation" }));
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(h.state.federationCalls).toHaveLength(1);
    expect(h.state.federationCalls[0]?.["workspaceId"]).toBeUndefined();
  });

  it("renders a vendor's refusal as the vendor's answer, not as a fault of ours", async () => {
    h.state.verifyReply = { verified: false, reason: "invalid x-api-key" };
    await renderProviders();
    const registry = screen.getByRole("region", { name: "What this node can call" });
    fireEvent.click(within(registry).getByRole("button", { name: "Verify chat54Mini with the vendor" }));
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(screen.getByText(/invalid x-api-key/)).toBeTruthy();
  });

  it("reaches no vendor until somebody presses Verify", async () => {
    await renderProviders();
    // Rendering must not spend a person's quota with a third party. The check
    // is a control, never something a panel does on open.
    expect(h.connection.query.providerVerify).not.toHaveBeenCalled();
  });

  it("renders a server refusal in surface, in the engine's own words", async () => {
    h.state.providerError = new Error("providerAuthStatus is owner-only");
    await renderProviders("admin");
    // The section is owner-floored in the manifest, so an admin should never
    // reach it -- but presentation is not the authorization, and if they do,
    // the engine's own sentence is what they read.
    expect(screen.getByText(/declined this read for admin/)).toBeTruthy();
    expect(screen.getByText("providerAuthStatus is owner-only")).toBeTruthy();
  });

  it("separates saving from applying, and says what Apply did", async () => {
    await renderProviders();
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(h.state.reloadCalls).toBe(1);
    expect(screen.getByText(/can call 1 of 2 providers/)).toBeTruthy();
  });
});
