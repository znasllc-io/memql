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
// What a person TYPES into the credential field. It crosses the wire once and
// must never come back into the DOM afterwards.
const TYPED_SECRET = "TYPED-CLIENT-SECRET-NEVER-SHOW-THIS-AGAIN";
// The engine's own success sentence. The surface renders it verbatim; a
// client-authored line would be confidently wrong on a node running from the
// environment.
const ENGINE_TAKES_EFFECT =
  "Saved. The next message this node sends re-resolves its sender, so it takes effect without a restart. Other replicas pick it up on their own next send.";
const ENGINE_ENV_TAKES_EFFECT =
  "Saved. This node resolves its sender from the environment, which outranks stored rows, so the row is recorded but this node will keep using the environment value.";

const h = vi.hoisted(() => {
  const reply = (rows: unknown[]) => ({ rows: () => rows });
  const state = {
    report: [] as unknown[],
    probedReport: null as unknown[] | null,
    error: null as Error | null,
    calls: [] as { probe: boolean }[],
    writes: [] as { slot: string; value: string }[],
    writeError: null as Error | null,
    writeReply: null as unknown[] | null,
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
      integrationConfigure: vi.fn(async (args: { slot: string; value: string }) => {
        state.writes.push({ slot: args.slot, value: args.value });
        if (state.writeError) throw state.writeError;
        if (state.writeReply !== null) return reply(state.writeReply);
        return reply([
          {
            integrationConfigured: {
              payload: {
                slot: args.slot,
                envVar: "MEMQL_EMAIL_SENDER",
                secret: false,
                source: "globalVariable",
                reresolves: true,
                takesEffect: ENGINE_TAKES_EFFECT,
              },
            },
          },
        ]);
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
const { roleAdmits, roleLadder } = await import("../../src/system/roles");
const { readConfigureOutcome, readIntegrationsReport, stateOf } = await import(
  "../../src/apps/settings/integrationsReport"
);

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
    lane: "graph",
    required: true,
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
    lane: "graph",
    required: true,
    editable: true,
  },
  {
    name: "tenantId",
    value: "11111111-2222-3333-4444-555555555555",
    source: "env",
    envVar: "MEMQL_EMAIL_AZURE_TENANT_ID",
    purpose: "Entra tenant the app registration lives in.",
    lane: "graph",
    required: true,
    // The boot-envelope boundary, DECLARED by the engine. The section renders
    // this flag and never re-derives it.
    editable: false,
  },
  {
    name: "fromName",
    value: "",
    source: "unset",
    envVar: "MEMQL_EMAIL_FROM_NAME",
    purpose: "Display name on the From header.",
    lane: "graph",
    required: false,
    editable: true,
  },
  {
    name: "smtpHost",
    value: "",
    source: "unset",
    envVar: "SMTP_HOST",
    purpose: "Relay hostname.",
    lane: "smtp",
    required: true,
    editable: true,
  },
];

const CONFIGURED = {
  name: "email",
  registered: true,
  capabilities: ["sendEmail"],
  state: "configured",
  reasons: [],
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
  state: "needs_configuration",
  // Per LANE, each naming its own env var. Built by the engine
  // (ConfigResolution.Reasons) and rendered verbatim.
  reasons: [
    {
      code: "missing_slot",
      lane: "graph",
      slot: "clientSecret",
      envVar: "MEMQL_EMAIL_AZURE_CLIENT_SECRET",
      detail:
        "MEMQL_EMAIL_AZURE_CLIENT_SECRET is not set anywhere, and the graph lane cannot work without it.",
    },
    {
      code: "split_lane",
      lane: "graph",
      slot: "",
      envVar: "",
      detail:
        "Every value the graph lane needs is present, but they are not all in the same place. Move them together.",
    },
  ],
  configured: "no",
  health: "degraded",
  mode: "log",
  detail:
    "No sender is configured, so the integration is running in log-only mode: every send succeeds, writes a line to the node log, and delivers nothing.",
  credentials: [{ ...CREDENTIALS[0], present: false, source: "unset" }],
};

const UNHEALTHY = {
  ...CONFIGURED,
  state: "unhealthy",
  reasons: [
    {
      code: "probe_failed",
      lane: "graph",
      slot: "",
      envVar: "",
      // WORD FOR WORD what the engine concatenates into `detail`. The card
      // must show it ONCE, which is what visibleReasons is for.
      detail:
        "Live check failed: the Entra token endpoint refused these credentials -- AADSTS7000215: Invalid client secret provided.",
    },
  ],
  health: "unhealthy",
  probed: true,
  detail:
    "Sending via graph, configured from globalVariable. Live check failed: the Entra token endpoint refused these credentials -- AADSTS7000215: Invalid client secret provided.",
};

const SILENT = {
  name: "telephony",
  registered: true,
  capabilities: [],
  // An integration with no self-report has NO state -- the empty string, which
  // must be read as unknown rather than as a member of the closed set.
  state: "",
  reasons: [],
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
  h.state.writes = [];
  h.state.writeError = null;
  h.state.writeReply = null;
  h.connection.query.integrationStatus.mockClear();
  h.connection.query.integrationConfigure.mockClear();
});

describe("every key is read, and a typo in one cannot be silent", () => {
  // THE FAILURE THIS EXISTS FOR HAS NO SYMPTOM. A reader keyed on a name the
  // engine does not send returns the zero value, and a zero value renders as
  // an empty field, an unset chip or a missing sentence -- all of which are
  // legitimate answers, so nothing looks wrong. The key names below are
  // transcribed from the Go json tags in integrations/email/status.go,
  // integrations/email/configmanifest.go and the map literals in
  // capabilities.go (the top-level envelope) and configure.go (the write
  // reply); every one carries a DISTINCTIVE value, so a misread key shows up
  // as a missing value rather than as a plausible blank.
  const everyField = {
    checkedAt: "2026-08-31T12:00:00Z",
    probed: true,
    integrations: [
      {
        name: "email",
        registered: true,
        capabilities: ["sendEmail", "configure"],
        configured: "yes",
        health: "unhealthy",
        state: "unhealthy",
        reasons: [
          {
            code: "probe_failed",
            lane: "graph",
            slot: "clientSecret",
            envVar: "MEMQL_EMAIL_AZURE_CLIENT_SECRET",
            detail: "REASON-DETAIL",
          },
        ],
        detail: "CARD-DETAIL",
        mode: "graph",
        settings: [
          {
            name: "senderAddress",
            value: "SETTING-VALUE",
            source: "globalVariable",
            envVar: "SETTING-ENVVAR",
            purpose: "SETTING-PURPOSE",
            lane: "graph",
            required: true,
            reason: "SETTING-REASON",
            editable: true,
          },
        ],
        credentials: [
          {
            name: "clientSecret",
            present: true,
            source: "globalSecret",
            envVar: "CREDENTIAL-ENVVAR",
            purpose: "CREDENTIAL-PURPOSE",
            lane: "graph",
            required: true,
            reason: "CREDENTIAL-REASON",
            rotate: "CREDENTIAL-ROTATE",
          },
        ],
      },
    ],
  };

  it("populates every field of the report from the engine's own key names", () => {
    const report = readIntegrationsReport([{ integrationStatus: { payload: everyField } }]);
    expect(report).not.toBeNull();
    expect(report!.checkedAt).toBe("2026-08-31T12:00:00Z");
    expect(report!.probed).toBe(true);
    const card = report!.integrations[0]!;
    expect(card).toMatchObject({
      name: "email",
      registered: true,
      capabilities: ["sendEmail", "configure"],
      state: "unhealthy",
      configured: "yes",
      health: "unhealthy",
      detail: "CARD-DETAIL",
      mode: "graph",
    });
    expect(card.reasons[0]).toEqual({
      code: "probe_failed",
      lane: "graph",
      slot: "clientSecret",
      envVar: "MEMQL_EMAIL_AZURE_CLIENT_SECRET",
      detail: "REASON-DETAIL",
    });
    expect(card.slots[0]).toEqual({
      name: "senderAddress",
      value: "SETTING-VALUE",
      source: "globalVariable",
      envVar: "SETTING-ENVVAR",
      purpose: "SETTING-PURPOSE",
      lane: "graph",
      required: true,
      reason: "SETTING-REASON",
      editable: true,
      secret: false,
      present: true,
      rotate: "",
    });
    expect(card.slots[1]).toEqual({
      name: "clientSecret",
      // A credential carries no value and there is no key to read one from.
      value: "",
      source: "globalSecret",
      envVar: "CREDENTIAL-ENVVAR",
      purpose: "CREDENTIAL-PURPOSE",
      lane: "graph",
      required: true,
      reason: "CREDENTIAL-REASON",
      rotate: "CREDENTIAL-ROTATE",
      editable: true,
      secret: true,
      present: true,
    });
  });

  it("populates every field of the write reply", () => {
    const outcome = readConfigureOutcome([
      {
        integrationConfigured: {
          payload: {
            slot: "senderAddress",
            envVar: "OUTCOME-ENVVAR",
            secret: true,
            source: "globalSecret",
            reresolves: true,
            takesEffect: "OUTCOME-SENTENCE",
          },
        },
      },
    ]);
    expect(outcome).toEqual({
      slot: "senderAddress",
      envVar: "OUTCOME-ENVVAR",
      secret: true,
      source: "globalSecret",
      reresolves: true,
      takesEffect: "OUTCOME-SENTENCE",
    });
  });

  it("returns null rather than a blank outcome when the reply carries no sentence", () => {
    // The negative control for the walk: it keys on `takesEffect` precisely so
    // a reply without one is an absence the surface can talk about, rather
    // than an object full of empty strings it would render as a success.
    expect(readConfigureOutcome([{ integrationConfigured: { payload: { slot: "x" } } }])).toBeNull();
  });
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

describe("the reasons and the lanes (memql#4825)", () => {
  it("lists what has to happen, verbatim, with the lane and slot it points at", async () => {
    await renderIntegrations();
    const list = screen.getByRole("list", { name: "What has to happen" });
    const items = within(list).getAllByRole("listitem");
    expect(items).toHaveLength(2);
    expect(within(list).getByText(/is not set anywhere, and the graph lane/)).toBeTruthy();
    expect(within(list).getByText(/not all in the same place/)).toBeTruthy();
    // A missing-slot reason points AT a field; a split-lane reason belongs to
    // no field, and that difference is what makes this list more than a copy
    // of the slot list below it.
    expect(within(items[0]!).getByText("Application secret")).toBeTruthy();
    expect(within(items[1]!).queryByText("Application secret")).toBeNull();
    expect(within(items[1]!).getByText("Microsoft Graph")).toBeTruthy();
  });

  it("does not print a reason the summary already contains", async () => {
    // The engine emits a probe verdict BOTH as a reason and, by
    // concatenation, inside `detail`. Rendering both puts one sentence on the
    // screen twice a few centimetres apart.
    h.state.report = envelope(UNHEALTHY);
    await renderIntegrations();
    expect(screen.queryByRole("list", { name: "What has to happen" })).toBeNull();
    expect(screen.getAllByText(/AADSTS7000215/)).toHaveLength(1);
  });

  it("says nothing when there is nothing to say", async () => {
    h.state.report = envelope(CONFIGURED);
    await renderIntegrations();
    expect(screen.queryByRole("list", { name: "What has to happen" })).toBeNull();
  });

  it("groups the keys by LANE, in the order the reply sends them", async () => {
    // A lane is an alternative taken WHOLE, so eleven flat fields invite
    // somebody to fill half of each -- the one arrangement that resolves to
    // nothing. `lane` is on every slot for exactly this; grouping by a prefix
    // in the name would be this window inventing structure.
    h.state.report = envelope(CONFIGURED);
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    const groups = within(card).getAllByRole("group");
    expect(groups.map((g) => g.getAttribute("aria-label"))).toEqual([
      "Email -- Microsoft Graph",
      "Email -- SMTP relay",
    ]);
    const graph = within(card).getByRole("list", {
      name: "Email Microsoft Graph configuration",
    });
    expect(within(graph).getAllByRole("listitem")).toHaveLength(4);
    expect(within(card).getByText(/one arrangement that resolves to nothing/)).toBeTruthy();
  });

  it("marks the OPTIONAL keys, not the required ones", async () => {
    h.state.report = envelope(CONFIGURED);
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    // Marking eleven slots "Required" marks nothing; marking the one that is
    // not is what says which field can be left alone.
    expect(within(card).getAllByText("Optional")).toHaveLength(1);
    expect(within(card).queryByText("Required")).toBeNull();
    const optional = within(card).getByRole("list", { name: "From name state" });
    expect(within(optional).getByText("Optional")).toBeTruthy();
  });

  it("renders the engine's own sentence under the field it is about", async () => {
    h.state.report = envelope({
      ...CONFIGURED,
      settings: [
        {
          ...SETTINGS[0],
          source: "unset",
          reason:
            "Required by the graph lane and not set anywhere. Set MEMQL_EMAIL_SENDER.",
        },
      ],
      credentials: [],
    });
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    expect(within(card).getByText(/Required by the graph lane and not set anywhere/)).toBeTruthy();
  });

  it("keeps a healthy field quiet", async () => {
    // `reason` is empty whenever nothing is wrong -- including for an OPTIONAL
    // slot nobody set, which is a normal state. A mark there trains an
    // operator to ignore the marks that mean something.
    h.state.report = envelope(CONFIGURED);
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    expect(within(card).queryByText(/not set anywhere/)).toBeNull();
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

  it("gives a credential a field that POSTS and never one that displays", async () => {
    h.state.report = envelope(CONFIGURED);
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    // Asserted by NAME rather than by count: a count passes for the wrong
    // reason the moment the fixture grows a slot. tenantId is env-supplied and
    // therefore has none.
    const boxes = within(card)
      .getAllByRole("textbox")
      .map((box) => box.getAttribute("id"));
    expect(boxes).toEqual([
      "integration-slot-senderAddress",
      "integration-slot-fromName",
      "integration-slot-clientSecret",
      "integration-slot-smtpHost",
    ]);
    // The credential's field starts EMPTY even though the slot is set: there
    // is nothing to prefill it with, and that is the whole promise.
    const secretBox = within(card).getByLabelText(
      "Application secret (new value)",
    ) as HTMLInputElement;
    expect(secretBox.value).toBe("");
    expect(secretBox.getAttribute("placeholder")).toBe("Replace this credential");
    expect(within(card).getByText("Write-only")).toBeTruthy();
    expect(within(card).getByText(/Sent once and sealed in the cluster/)).toBeTruthy();
    // And the CLI route survives for operators who prefer it.
    expect(within(card).getByText(/make secret-set/)).toBeTruthy();
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

describe("the write half (memql#4825)", () => {
  async function typeAndSave(field: string, value: string) {
    const card = screen.getByRole("region", { name: "Email" });
    fireEvent.change(within(card).getByLabelText(field), { target: { value } });
    await act(async () => {
      fireEvent.click(within(card).getByRole("button", { name: `Save ${field.replace(/ \(new value\)$/, "")}` }));
      await Promise.resolve();
      await Promise.resolve();
    });
    return card;
  }

  it("calls the builtin with the slot NAME, never the environment variable", async () => {
    // The whole reason a browser may not call setGlobalVariable itself: a
    // caller that supplied the variable could write MEMQL_EMAIL_SENDR, get a
    // green save, and never be mailed anything again.
    h.state.report = envelope(CONFIGURED);
    await renderIntegrations();
    await typeAndSave("Sending mailbox", "ops@example.com");
    expect(h.state.writes).toEqual([{ slot: "senderAddress", value: "ops@example.com" }]);
    expect(h.state.writes[0]?.slot).not.toContain("MEMQL_");
  });

  it("re-reads after a save, so the card shows what the cluster now holds", async () => {
    h.state.report = envelope(CONFIGURED);
    await renderIntegrations();
    expect(h.state.calls).toHaveLength(1);
    await typeAndSave("Sending mailbox", "ops@example.com");
    expect(h.state.calls).toEqual([{ probe: false }, { probe: false }]);
  });

  it("renders the ENGINE's success sentence, not one of ours", async () => {
    // `takesEffect` has two forms and only the engine knows which happened.
    h.state.report = envelope(CONFIGURED);
    await renderIntegrations();
    const card = await typeAndSave("Sending mailbox", "ops@example.com");
    expect(within(card).getByText(ENGINE_TAKES_EFFECT)).toBeTruthy();
  });

  it("says the row is recorded and ignored when the engine says so", async () => {
    // The env branch. A client-authored "it takes effect shortly" would be
    // confidently wrong exactly here.
    h.state.report = envelope(CONFIGURED);
    h.state.writeReply = [
      {
        integrationConfigured: {
          payload: {
            slot: "senderAddress",
            envVar: "MEMQL_EMAIL_SENDER",
            secret: false,
            source: "globalVariable",
            reresolves: false,
            takesEffect: ENGINE_ENV_TAKES_EFFECT,
          },
        },
      },
    ];
    await renderIntegrations();
    const card = await typeAndSave("Sending mailbox", "ops@example.com");
    expect(within(card).getByText(ENGINE_ENV_TAKES_EFFECT)).toBeTruthy();
    expect(within(card).queryByText(/without a restart/)).toBeNull();
  });

  it("claims nothing about timing when the reply carries no sentence", async () => {
    h.state.report = envelope(CONFIGURED);
    h.state.writeReply = [{ somethingElse: { payload: { ok: true } } }];
    await renderIntegrations();
    const card = await typeAndSave("Sending mailbox", "ops@example.com");
    expect(within(card).getByText(/did not say when it takes effect/)).toBeTruthy();
  });

  it("renders a refusal beside the field, in the engine's own words", async () => {
    h.state.report = envelope(CONFIGURED);
    h.state.writeError = new Error(
      'email.configure: "senderAddres" is not a configurable setting of the email integration; the settings it has are tenantId, clientId, senderAddress',
    );
    await renderIntegrations();
    const card = await typeAndSave("Sending mailbox", "ops@example.com");
    expect(within(card).getByText("Sending mailbox was not saved.")).toBeTruthy();
    // Verbatim: the engine's sentence NAMES the settings that exist, which no
    // paraphrase of ours would.
    expect(within(card).getByText(/the settings it has are tenantId/)).toBeTruthy();
    expect(within(card).queryByText(ENGINE_TAKES_EFFECT)).toBeNull();
  });

  it("refuses to send a blank value rather than spending a refusal on it", async () => {
    h.state.report = envelope(CONFIGURED);
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    // `fromName` is the OPTIONAL, unset slot: its field starts empty.
    const save = within(card).getByRole("button", { name: "Save From name" });
    expect((save as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(within(card).getByLabelText("From name"), { target: { value: "MemQL" } });
    expect((within(card).getByRole("button", { name: "Save From name" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("offers no field for an env-supplied slot, whichever half it is in", async () => {
    // The engine's `editable` flag for a setting; the same env-first rule,
    // applied to a credential, which carries no such flag.
    h.state.report = envelope({
      ...CONFIGURED,
      credentials: [{ ...CREDENTIALS[0], source: "env" }],
    });
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    expect(within(card).queryByLabelText("Microsoft tenant")).toBeNull();
    expect(within(card).queryByLabelText("Application secret (new value)")).toBeNull();
    expect(within(card).queryByRole("button", { name: "Save Application secret" })).toBeNull();
  });

  it("says once per card how far a save reaches", async () => {
    h.state.report = envelope(CONFIGURED);
    await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    expect(within(card).getByText(/Other replicas pick it up on their own next send/)).toBeTruthy();
  });
});

describe("a credential is posted and never read back", () => {
  it("keeps a typed secret out of the DOM after the save", async () => {
    h.state.report = envelope(CONFIGURED);
    h.state.writeReply = [
      {
        integrationConfigured: {
          payload: {
            slot: "clientSecret",
            envVar: "MEMQL_EMAIL_AZURE_CLIENT_SECRET",
            secret: true,
            source: "globalSecret",
            reresolves: true,
            takesEffect: ENGINE_TAKES_EFFECT,
          },
        },
      },
    ];
    const { container } = await renderIntegrations();
    const card = screen.getByRole("region", { name: "Email" });
    fireEvent.change(within(card).getByLabelText("Application secret (new value)"), {
      target: { value: TYPED_SECRET },
    });
    await act(async () => {
      fireEvent.click(within(card).getByRole("button", { name: "Save Application secret" }));
      await Promise.resolve();
      await Promise.resolve();
    });

    // It reached the cluster ONCE...
    expect(h.state.writes).toEqual([{ slot: "clientSecret", value: TYPED_SECRET }]);
    // ...and the field cleared itself rather than holding it.
    expect(
      (within(card).getByLabelText("Application secret (new value)") as HTMLInputElement).value,
    ).toBe("");
    // ...and nothing anywhere renders it, in any attribute.
    expect(container.innerHTML).not.toContain(TYPED_SECRET);

    // The reachable positive: the save DID happen and the card says so.
    expect(within(card).getByText(ENGINE_TAKES_EFFECT)).toBeTruthy();
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
    // The engine admits owner, developer and admin (memql#4826 added
    // developer). A reader below that floor gets the engine's own sentence,
    // which names the roles -- this window never rewrites it, and never
    // renders a zero in its place.
    h.state.error = new Error(
      'email.status: role "writer" may not read integration configuration (owner, developer or admin required)',
    );
    await renderIntegrations("writer");
    expect(screen.getByText(/declined this read for writer/)).toBeTruthy();
    expect(screen.getByText(/owner, developer or admin required/)).toBeTruthy();
    // And it says which half is the authority, rather than implying the two
    // gates disagree: this window's role set decides what it OFFERS; the
    // cluster decides what it serves.
    expect(screen.getByText(/it is the one that counts/)).toBeTruthy();
    // Nothing is rendered as though it were configuration.
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
    // The ladder is cluster state now (epic memql#4832), installed for every
    // suite by test/setup.ts. Reading it here rather than a literal keeps the
    // control measuring the same rungs the assertions above name.
    const rungs = roleLadder().map((rung) => rung.slug);
    expect(rungs.length).toBeGreaterThan(0);
    for (const role of rungs) {
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
