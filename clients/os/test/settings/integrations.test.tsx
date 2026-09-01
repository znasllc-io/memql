import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { ReactNode } from "react";

// The Integrations section (issue #4826 / program decision P6).
//
// Planted values are long and distinctive on purpose: the server-side sweep
// (`TestStatusNeverLeaksACredential`) skips anything under eight characters,
// and a short planted value makes a sweep vacuous. They are planted on keys
// the engine NEVER SENDS for a credential -- that is the point. If a future
// reply ever carried one, or a renderer ever spread a raw slot into the DOM,
// this is the test that fails.
const PLANTED_SECRET = "PLANTED-GRAPH-CLIENT-SECRET-DO-NOT-EMIT";
const PLANTED_CIPHERTEXT = "PLANTED-SEALED-CIPHERTEXT-DO-NOT-EMIT";
const PLANTED_FINGERPRINT = "PLANTED-FINGERPRINT-DO-NOT-EMIT";

const h = vi.hoisted(() => {
  const reply = (rows: unknown[]) => ({ rows: () => rows });
  const state = {
    report: [] as unknown[],
    probedReport: null as unknown[] | null,
    error: null as Error | null,
    calls: [] as { probe: boolean }[],
  };
  const connection = {
    nodeId: "bff-test",
    engineVersion: "v9.9.9",
    engineCommit: "abcdef123456",
    subscriptions: null,
    query: {
      integrationStatus: vi.fn(async (args: { probe: boolean }) => {
        state.calls.push({ probe: args.probe });
        if (state.error) throw state.error;
        if (args.probe && state.probedReport !== null) return reply(state.probedReport);
        return reply(state.report);
      }),
    },
    onStatusChange: (fn: (ev: { status: string; attempt: number; error: string }) => void) => {
      fn({ status: "connected", attempt: 0, error: "" });
      return () => {};
    },
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
const { INTEGRATIONS_SECTION_ROLE } = await import(
  "../../src/apps/settings/IntegrationsSection"
);
const { LocalDesktopStore } = await import("../../src/system/store");
const { UNKNOWN_RUNTIME_CONFIG } = await import("../../src/cluster/config");
const { roleAdmits, ROLE_LADDER } = await import("../../src/system/roles");
const { stateOf } = await import("../../src/apps/settings/integrationsReport");

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

async function renderIntegrations(role = "owner") {
  const view = render(
    wrap(<SettingsApp sectionId="integrations" navigate={vi.fn()} askContext={vi.fn()} />, role),
  );
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
  return view;
}

/** One reply envelope, shaped exactly as the engine marshals a builtin's. */
function envelope(email: Record<string, unknown>, extra: Record<string, unknown>[] = []) {
  return [
    {
      integrationStatus: {
        payload: {
          checkedAt: "2026-08-31T12:00:00Z",
          probed: false,
          integrations: [email, ...extra],
        },
      },
    },
  ];
}

/** The credential slots, with the values the engine never sends planted on
 *  them. `rotate` carries the real operator command, which IS rendered. */
const CREDENTIALS = [
  {
    name: "clientSecret",
    present: true,
    source: "globalSecret",
    envVar: "MEMQL_EMAIL_AZURE_CLIENT_SECRET",
    purpose: "Client-credentials secret for the app registration.",
    rotate: "make secret-set NAME=MEMQL_EMAIL_AZURE_CLIENT_SECRET VALUE=<new value> SCOPE=global",
    // None of these three is a key the reply carries. They are here so that a
    // renderer that ever reached for one fails loudly.
    value: PLANTED_SECRET,
    encryptedValue: PLANTED_CIPHERTEXT,
    fingerprint: PLANTED_FINGERPRINT,
  },
];

const SETTINGS = [
  {
    name: "senderAddress",
    value: "noreply@example.com",
    source: "globalVariable",
    envVar: "MEMQL_EMAIL_SENDER",
    purpose: "The mailbox mail is sent AS.",
    editable: true,
  },
  {
    name: "smtpHost",
    value: "",
    source: "unset",
    envVar: "SMTP_HOST",
    purpose: "Relay hostname.",
    editable: true,
  },
  {
    name: "tenantId",
    value: "11111111-2222-3333-4444-555555555555",
    source: "env",
    envVar: "MEMQL_EMAIL_AZURE_TENANT_ID",
    purpose: "Entra tenant the app registration lives in.",
    // The boot-envelope boundary, DECLARED by the engine. The section renders
    // this flag and never re-derives it.
    editable: false,
  },
];

const CONFIGURED = {
  name: "email",
  registered: true,
  capabilities: ["sendEmail"],
  configured: "yes",
  health: "unknown",
  mode: "graph",
  detail: "Sending via graph, configured from globalVariable.",
  probed: false,
  settings: SETTINGS,
  credentials: CREDENTIALS,
};

const NEEDS_CONFIGURATION = {
  ...CONFIGURED,
  configured: "no",
  health: "degraded",
  mode: "log",
  detail:
    "No sender is configured, so the integration is running in log-only mode: every send succeeds, writes a line to the node log, and delivers nothing.",
  credentials: [{ ...CREDENTIALS[0], present: false, source: "unset" }],
};

const UNHEALTHY = {
  ...CONFIGURED,
  health: "unhealthy",
  probed: true,
  detail:
    "Sending via graph, configured from globalVariable. Live check failed: the Entra token endpoint refused these credentials.",
};

const SILENT = {
  name: "telephony",
  registered: true,
  capabilities: [],
  configured: "unknown",
  health: "unknown",
  detail: "Registered on this node. This integration publishes no configuration self-report.",
  settings: [],
  credentials: [],
};

beforeEach(() => {
  h.state.report = envelope(NEEDS_CONFIGURATION, [SILENT]);
  h.state.probedReport = null;
  h.state.error = null;
  h.state.calls = [];
  h.connection.query.integrationStatus.mockClear();
});

describe("the three states come from the engine", () => {
  it("reads an unconfigured integration as an invitation, not a fault", async () => {
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    expect(within(card).getByText("Needs configuration")).toBeTruthy();
    // The engine's own sentence, verbatim -- it names the log-only trap, which
    // no paraphrase of ours would.
    expect(within(card).getByText(/delivers nothing/)).toBeTruthy();
    // The invitation half: what it would let the cluster do.
    expect(within(card).getByText(/sign-in links, guest invitations/)).toBeTruthy();
    expect(within(card).getByText(/Nothing is broken until then/)).toBeTruthy();
    // The error voice is reserved for `unhealthy`. An unconfigured card must
    // not be an alert.
    expect(within(card).queryAllByRole("alert")).toEqual([]);
  });

  it("reads a configured integration as configured", async () => {
    h.state.report = envelope(CONFIGURED);
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    expect(within(card).getByText("Configured")).toBeTruthy();
    expect(within(card).getByText(/configured from globalVariable/)).toBeTruthy();
    expect(within(card).queryAllByRole("alert")).toEqual([]);
  });

  it("reads a configured-and-failing integration as unhealthy, in the error voice", async () => {
    h.state.report = envelope(UNHEALTHY);
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    expect(within(card).getByText("Unhealthy")).toBeTruthy();
    expect(within(card).getAllByRole("alert").length).toBeGreaterThan(0);
    expect(within(card).getByText(/Entra token endpoint refused/)).toBeTruthy();
  });

  it("takes a machine-readable state when the reply carries one", async () => {
    // Stream A's generalization. A declared state wins over the pair, so the
    // section does not have to be edited again when it lands.
    expect(stateOf({ state: "unhealthy", configured: "no", health: "degraded" })).toBe("unhealthy");
    expect(stateOf({ state: "not_a_state", configured: "yes", health: "healthy" })).toBe("configured");
  });

  it("reads CONFIGURED before HEALTH, so an unset cluster is never an error", async () => {
    // An install that must deliver mail reports configured=no AND
    // health=unhealthy. Reading health first would put the error voice on a
    // cluster nobody has set up yet; the engine's sentence carries the rest,
    // including that sends are refused meanwhile.
    expect(stateOf({ configured: "no", health: "unhealthy" })).toBe("needs_configuration");
    expect(stateOf({ configured: "yes", health: "unhealthy" })).toBe("unhealthy");
    expect(stateOf({ configured: "unknown", health: "unknown" })).toBe("not_reported");
  });
});

describe("a secret is write-only", () => {
  it("renders no credential value anywhere in the DOM", async () => {
    const { container } = await renderIntegrations();
    const dom = container.innerHTML;
    for (const planted of [PLANTED_SECRET, PLANTED_CIPHERTEXT, PLANTED_FINGERPRINT]) {
      expect(dom).not.toContain(planted);
    }
    // Not masked either: the promise is that no value is read back at all,
    // and a row of dots is a claim that one was.
    expect(dom).not.toContain("•••");

    // THE NEGATIVE CONTROL. Without it this asserts only that the sweep ran,
    // not that it could ever have seen anything. The slot IS on the page --
    // by its name, its purpose, its env var and its rotate command.
    expect(dom).toContain("Application secret");
    expect(dom).toContain("MEMQL_EMAIL_AZURE_CLIENT_SECRET");
    expect(dom).toContain("make secret-set");
  });

  it("offers no field for a credential, and says where one is changed", async () => {
    h.state.report = envelope(CONFIGURED);
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    // Every field on the card belongs to a non-secret slot, and the two
    // credentials have none. Asserted by NAME rather than by count: a count
    // passes for the wrong reason the moment the fixture grows a setting.
    const boxes = within(card)
      .getAllByRole("textbox")
      .map((box) => box.getAttribute("id"));
    expect(boxes).toEqual(["integration-slot-senderAddress", "integration-slot-smtpHost"]);
    for (const credential of ["clientSecret", "smtpPassword"]) {
      expect(boxes.some((id) => id?.includes(credential))).toBe(false);
    }
    expect(within(card).getByText(/never from a browser/)).toBeTruthy();
    expect(within(card).getByText("Write-only")).toBeTruthy();
  });

  it("says a credential is SET and where it came from, without saying what it is", async () => {
    h.state.report = envelope(CONFIGURED);
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    const chips = within(card).getByRole("list", { name: "Application secret state" });
    expect(within(chips).getByText("Set")).toBeTruthy();
    expect(within(chips).getByText("Sealed in the cluster")).toBeTruthy();
    // ...and an UNSET slot carries no source chip: "No source" beside "Not
    // set" says the same thing twice.
    const unset = within(card).getByRole("list", { name: "Relay host state" });
    expect(within(unset).getByText("Not set")).toBeTruthy();
    expect(within(unset).queryByText("No source")).toBeNull();
  });
});

describe("a boot-envelope variable is listed, not offered", () => {
  it("renders an env-managed slot with no input and says why", async () => {
    h.state.report = envelope(CONFIGURED);
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    // The env-sourced slot has no control at all. An editable-looking field
    // that cannot work is worse than an absent one.
    expect(within(card).queryByLabelText("Microsoft tenant")).toBeNull();
    expect(within(card).getByText(/which the resolver reads first/)).toBeTruthy();
    expect(within(card).getByText(/Changing it is a redeploy/)).toBeTruthy();
    // Named by what it is, with the variable name as secondary text -- an
    // operator setting it in a cluster needs exactly that string.
    expect(within(card).getByText(/Microsoft tenant -- MEMQL_EMAIL_AZURE_TENANT_ID/)).toBeTruthy();
  });

  it("takes the boundary from the engine's own flag, not from the source name", async () => {
    // A slot stored in the graph and still marked non-editable renders as
    // env-managed. Re-deriving the boundary from `source` in TypeScript would
    // produce a second opinion, and the failure it produces is a field that
    // silently does nothing.
    h.state.report = envelope({
      ...CONFIGURED,
      settings: [{ ...SETTINGS[0], source: "globalVariable", editable: false }],
      credentials: [],
    });
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    expect(within(card).queryAllByRole("textbox")).toEqual([]);
    expect(within(card).getByText(/Changing it is a redeploy/)).toBeTruthy();
  });
});

describe("the write half is inert, and says exactly what is missing", () => {
  it("disables every field and names what has to exist", async () => {
    h.state.report = envelope(CONFIGURED);
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    for (const box of within(card).getAllByRole("textbox")) {
      expect((box as HTMLInputElement).disabled).toBe(true);
    }
    expect(within(card).getByText(/Saving from here is not wired up yet/)).toBeTruthy();
    expect(within(card).getByText(/nothing that writes it back/)).toBeTruthy();
    // No Save. A button that silently does nothing is the thing this avoids.
    expect(within(card).queryByRole("button", { name: /save/i })).toBeNull();
  });
});

describe("the probe is an action", () => {
  it("does not run on mount", async () => {
    await renderIntegrations();
    expect(h.state.calls).toEqual([{ probe: false }]);
  });

  it("runs on the action, and only then", async () => {
    h.state.probedReport = envelope({ ...UNHEALTHY, probed: true });
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    await act(async () => {
      fireEvent.click(within(card).getByRole("button", { name: "Check connection" }));
      await Promise.resolve();
    });
    expect(h.state.calls).toEqual([{ probe: false }, { probe: true }]);
    expect(screen.getByText(/Entra token endpoint refused/)).toBeTruthy();
  });

  it("says whether a check has been made at all", async () => {
    await renderIntegrations();
    expect(screen.getByText(/Not checked yet/)).toBeTruthy();
    expect(screen.getByText(/Nobody is mailed/)).toBeTruthy();
  });

  it("does not probe when a Refresh is pressed", async () => {
    await renderIntegrations();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
      await Promise.resolve();
    });
    expect(h.state.calls).toEqual([{ probe: false }, { probe: false }]);
  });
});

describe("what the section will not invent", () => {
  it("gives an integration with no self-report a roll-call line, not a card", async () => {
    await renderIntegrations();
    const roll = screen.getByRole("region", { name: "Also registered on this node" });
    expect(within(roll).getByText("telephony")).toBeTruthy();
    expect(within(roll).getByText(/not knowable from here/)).toBeTruthy();
    // A card would promise there is something to configure here.
    expect(screen.queryByRole("region", { name: "Telephony" })).toBeNull();
  });

  it("renders a refusal in surface, in the engine's own words -- never a zero", async () => {
    h.state.error = new Error(
      'email.status: role "developer" may not read integration configuration (owner or admin required)',
    );
    await renderIntegrations("developer");
    expect(screen.getByText(/declined this read for developer/)).toBeTruthy();
    expect(screen.getByText(/may not read integration configuration/)).toBeTruthy();
    // The gap is NAMED. A developer is the role this section exists for and
    // the engine's own check does not admit one yet.
    expect(screen.getByText(/Closing that gap is engine work/)).toBeTruthy();
    // And nothing is rendered as though it were configuration.
    expect(screen.queryByRole("region", { name: "Email" })).toBeNull();
  });

  it("stamps the read with the SERVER's own moment", async () => {
    await renderIntegrations();
    expect(screen.getByText(/Read at 2026-08-31T12:00:00Z/)).toBeTruthy();
    expect(screen.getByText(/which replica answered is not knowable/)).toBeTruthy();
  });
});

describe("the role gate", () => {
  it("admits owner and developer, and refuses admin, reader and writer", async () => {
    // P6: gated owner-or-developer, explicitly NOT admin. `{ min: "developer" }`
    // would admit admin, so this requirement is a role SET -- and the whole
    // reason system/roles.ts grew one.
    expect(INTEGRATIONS_SECTION_ROLE).toEqual({ any: ["owner", "developer"] });
    expect(roleAdmits("owner", INTEGRATIONS_SECTION_ROLE)).toBe(true);
    expect(roleAdmits("developer", INTEGRATIONS_SECTION_ROLE)).toBe(true);
    expect(roleAdmits("admin", INTEGRATIONS_SECTION_ROLE)).toBe(false);
    expect(roleAdmits("writer", INTEGRATIONS_SECTION_ROLE)).toBe(false);
    expect(roleAdmits("reader", INTEGRATIONS_SECTION_ROLE)).toBe(false);
    expect(roleAdmits("", INTEGRATIONS_SECTION_ROLE)).toBe(false);
    // The reachable positive: every one of those roles is admitted somewhere,
    // so the five falses above are about this requirement rather than about a
    // predicate that refuses everything.
    for (const role of ROLE_LADDER) {
      expect(roleAdmits(role, undefined)).toBe(true);
    }
  });

  it("matches whatever the settings manifest declares for the section", async () => {
    // Stream E owns the manifest; this is the one value, and a manifest that
    // declared a different one would gate the nav differently from the
    // section's own account of itself. Skipped until that half lands, because
    // a missing section is E's PR, not a defect here.
    const settings = OS_REGISTRY.apps.find((a) => a.id === "settings");
    const section = (settings?.sections ?? []).find((s) => s.id === "integrations");
    if (!section) return;
    expect(section.roles).toEqual(INTEGRATIONS_SECTION_ROLE);
  });
});
