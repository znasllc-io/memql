// Campaign authoring (memql#3323), end to end against a fake cluster.
//
// WHAT THIS FILE OWNS. The DSL conformance suite proves the campaigns
// constructs are shaped and gated correctly; it cannot see what the browser
// sends. This file asserts the wire form -- that authoring produces the named
// mutation calls the DSL declares, with the arguments quoted so a name
// carrying a quote or a newline cannot break the statement around it -- and
// the two honesty properties the surface promises: that rows render through
// the shared element library rather than through markup this repo drew, and
// that "schedule" never claims to have sent anything.
//
// memql#3348 added the SEND actions, and with them a third property worth
// asserting from the browser side: the one irreversible control on this
// surface must offer exactly one action per campaign state, and must not offer
// any for a campaign that is already sent. A row of disabled buttons answers
// "what can I do now" worse than one enabled one, and a Start button on a sent
// campaign invites a second run that would mail nobody (the delivery ledger is
// per (campaign, recipient), so every recipient is already terminal).
//
// The quoting case is not decorative. Argument interpolation is where a
// hand-built call string goes wrong, and the failure is silent-ish and nasty:
// an unescaped quote does not corrupt one field, it terminates the literal and
// re-parses the rest of the campaign name as MemQL.

import { describe, expect, it, vi } from "vitest";
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

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { buildCall } from "../src/integrations/calls";

const CAMPAIGN = "v1:campaigns:campaign";
const AUDIENCE = "v1:campaigns:audience";
const TEMPLATE = "v1:campaigns:template";
const DELIVERY = "v1:campaigns:delivery";

// The display cards are the REAL ones from dsl/campaigns/concepts.memql,
// because that is what the tables compose against.
const CONCEPTS: Concept[] = [
  {
    id: CAMPAIGN,
    version: "v1",
    domain: "campaigns",
    entity: "campaign",
    type: "concept",
    description: "One template addressed to one audience",
    displayCard: {
      primary: "name",
      secondary: "scheduledAt",
      tertiary: "audienceId",
      status: "status",
    },
  },
  {
    id: AUDIENCE,
    version: "v1",
    domain: "campaigns",
    entity: "audience",
    type: "concept",
    description: "A named set of email recipients",
    displayCard: {
      primary: "name",
      secondary: "description",
      tertiary: "ownerUserId",
      status: "status",
    },
  },
  {
    id: TEMPLATE,
    version: "v1",
    domain: "campaigns",
    entity: "template",
    type: "concept",
    description: "Subject line plus bodies",
    displayCard: { primary: "name", secondary: "subject", tertiary: "ownerUserId", status: "status" },
  },
  {
    id: DELIVERY,
    version: "v1",
    domain: "campaigns",
    entity: "delivery",
    type: "concept",
    description: "One recipient's outcome for one campaign",
    displayCard: { primary: "email", secondary: "campaignId", tertiary: "sentAt", status: "status" },
  },
];

// Wire-shaped rows: payload NESTED alongside the intrinsics, because that is
// what a named query returns and the flatten is part of what is exercised.
function node(concept: string, id: string, payload: Record<string, unknown>): Row {
  return {
    id,
    concept,
    type: "concept",
    createdBy: "user-1",
    createdAt: "2026-08-08T10:00:00Z",
    payload,
  };
}

const AUDIENCE_ROWS: Row[] = [
  node(AUDIENCE, "aud-1", {
    ownerUserId: "user-1",
    name: "Beta waitlist",
    description: "Signed up before launch",
    status: "active",
  }),
];

const TEMPLATE_ROWS: Row[] = [
  node(TEMPLATE, "tpl-1", {
    ownerUserId: "user-1",
    name: "August update",
    subject: "What shipped in August",
    status: "ready",
  }),
];

function campaignRows(status = "draft"): Row[] {
  return [
    node(CAMPAIGN, "camp-1", {
      ownerUserId: "user-1",
      name: "August send",
      audienceId: "aud-1",
      templateId: "tpl-1",
      status,
      recipientCount: 0,
      sentCount: 0,
      failedCount: 0,
    }),
  ];
}


const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
};

function renderAt(path: string, campaignStatus = "draft") {
  const rows = campaignRows(campaignStatus);
  const access: AccessSummary = {
    requestId: "r1",
    userId: "user-1",
    primaryEmail: "ada@example.com",
    clusterRole: "owner",
  };

  const calls: string[] = [];
  const executeNamed = vi.fn(async (name: string, call: string) => {
    calls.push(call);
    switch (name) {
      case "campaigns":
        return new Result({ bundle: { nodes: rows }, meta: { cursor: "" } });
      case "audiences":
        return new Result({ bundle: { nodes: AUDIENCE_ROWS }, meta: { cursor: "" } });
      case "templates":
        return new Result({ bundle: { nodes: TEMPLATE_ROWS }, meta: { cursor: "" } });
      default:
        return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
    }
  });

  const query = {
    listConcepts: vi.fn(async () => CONCEPTS),
    getMyAccess: vi.fn(async () => access),
    executeNamed,
  } as unknown as QueryClient;

  const dial = vi.fn(
    async () =>
      ({
        nodeId: "bff-test",
        serverVersion: "0.0.0-test",
        query,
        dispatcher: { sendAndWait: vi.fn(async () => ({})) },
        close: vi.fn(),
        done: vi.fn(() => new Promise<void>(() => {})),
      }) as unknown as Connection,
  ) as unknown as typeof Connection.dial;

  const utils = render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("the campaign tests must make no identity calls");
        }}
        storage={null}
        navigate={() => {}}
        redirectUri="https://cockpit.example.com/portal/auth/callback"
      >
        <ClusterProvider dial={dial}>
          <AppRoutes />
        </ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );

  return { ...utils, calls };
}

function callsNamed(calls: readonly string[], construct: string): string[] {
  return calls.filter((call) => call.includes(`${construct}(`));
}

describe("the campaigns list", () => {
  it("renders campaigns, audiences and templates through the element library", async () => {
    const { container } = renderAt("/integrations/campaigns");

    await waitFor(() => expect(screen.getAllByText("August send").length).toBeGreaterThan(0));
    expect(screen.getAllByText("Beta waitlist").length).toBeGreaterThan(0);
    expect(screen.getAllByText("August update").length).toBeGreaterThan(0);

    const classes = new Set<string>();
    for (const el of container.querySelectorAll("[class]")) {
      for (const cls of (el.getAttribute("class") ?? "").split(/\s+/)) {
        if (cls.startsWith("vk-")) classes.add(cls);
      }
    }
    // A table and a stat strip, both view-kit's markup rather than this repo's.
    expect(classes.has("vk-table")).toBe(true);
    expect(classes.has("vk-stat")).toBe(true);
  });

  it("reads its three populations with named calls, not raw DSL", async () => {
    const { calls } = renderAt("/integrations/campaigns");
    await waitFor(() => expect(screen.getAllByText("August send").length).toBeGreaterThan(0));

    expect(callsNamed(calls, "campaigns").length).toBe(1);
    expect(callsNamed(calls, "audiences").length).toBe(1);
    expect(callsNamed(calls, "templates").length).toBe(1);
    expect(callsNamed(calls, "campaigns")[0]).toBe("query campaigns()");
  });

  it("creates an audience through the declared mutation", async () => {
    const { calls } = renderAt("/integrations/campaigns");
    await waitFor(() => expect(screen.getAllByText("Beta waitlist").length).toBeGreaterThan(0));

    fireEvent.change(screen.getByPlaceholderText("Beta waitlist"), {
      target: { value: "Newsletter" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add audience" }));

    await waitFor(() => expect(callsNamed(calls, "createAudience").length).toBe(1));
    const call = callsNamed(calls, "createAudience")[0] ?? "";
    expect(call.startsWith("mutation createAudience(")).toBe(true);
    expect(call).toContain(`name: "Newsletter"`);
    // An empty optional is OMITTED, not sent as "". A `when()` guard drops an
    // ABSENT argument; an empty string is present and filters for nothing.
    expect(call).not.toContain("description:");
  });
});

describe("the campaign editor", () => {
  it("seeds the form from the row and saves through updateCampaign", async () => {
    const { calls } = renderAt("/integrations/campaigns/camp-1");

    await waitFor(() =>
      expect((screen.getByPlaceholderText("August product update") as HTMLInputElement).value).toBe(
        "August send",
      ),
    );

    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(callsNamed(calls, "updateCampaign").length).toBe(1));

    const call = callsNamed(calls, "updateCampaign")[0] ?? "";
    expect(call).toContain(`campaignId: "camp-1"`);
    expect(call).toContain(`audienceId: "aud-1"`);
    expect(call).toContain(`templateId: "tpl-1"`);
  });

  it("escapes a quote typed into the form rather than terminating the literal", async () => {
    const { calls } = renderAt("/integrations/campaigns/camp-1");
    await waitFor(() =>
      expect((screen.getByPlaceholderText("August product update") as HTMLInputElement).value).toBe(
        "August send",
      ),
    );

    fireEvent.change(screen.getByPlaceholderText("August product update"), {
      target: { value: 'The "big" one' },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(callsNamed(calls, "updateCampaign").length).toBe(1));
    const call = callsNamed(calls, "updateCampaign")[0] ?? "";
    expect(call).toContain('name: "The \\"big\\" one"');
    // Unescaped, the second quote closes the literal and `big` re-parses as
    // MemQL. Assert the statement still has exactly the argument list it should.
    expect(call.startsWith("mutation updateCampaign(")).toBe(true);
    expect(call.endsWith(")")).toBe(true);
  });

  // The newline case cannot be driven through the form: `name` is a
  // single-line <input>, and HTML value sanitization strips CR/LF before the
  // value ever reaches a change handler. Asserting it through the UI would be
  // asserting the platform's stripping, not this repo's escaping -- so it is
  // asserted where the escaping actually happens. Every authoring call in the
  // surface is built by this one function (src/integrations/calls.ts), which is
  // the whole reason the quoting decision was centralised there.
  it("escapes a newline in a call argument, wherever the value came from", () => {
    const call = buildCall("mutation", "updateCampaign", {
      campaignId: "camp-1",
      name: 'The "big" one\nline two',
    });
    expect(call).toContain('name: "The \\"big\\" one\\nline two"');
    // A raw newline in the statement would mean the escaping did not happen.
    expect(call.includes("\n")).toBe(false);
  });

  // Omission is load-bearing, not tidiness: a `when(args.x)` guard is dropped
  // when its argument is ABSENT, while an empty string is present -- so sending
  // status: "" filters for rows whose status is the empty string, i.e. none.
  it("omits an empty argument rather than sending an empty string", () => {
    const call = buildCall("query", "campaigns", { status: "", audienceId: "aud-1" });
    expect(call).toBe('query campaigns(audienceId: "aud-1")');
  });

  it("says out loud that recording a schedule does not send anything", async () => {
    const { calls } = renderAt("/integrations/campaigns/camp-1");
    await waitFor(() =>
      expect((screen.getByPlaceholderText("August product update") as HTMLInputElement).value).toBe(
        "August send",
      ),
    );

    fireEvent.change(screen.getByPlaceholderText("2026-09-01T09:00:00Z"), {
      target: { value: "2026-09-01T09:00:00Z" },
    });
    fireEvent.click(screen.getByRole("button", { name: /Record schedule/ }));

    await waitFor(() => expect(callsNamed(calls, "scheduleCampaign").length).toBe(1));
    // Sending exists now (memql#3348), but nothing STARTS a send from a clock,
    // so the confirmation still has to say that in words -- an operator who
    // reads "Scheduled." will otherwise expect mail that never comes.
    expect(await screen.findByText(/nothing starts a send from it yet/)).toBeTruthy();
  });

  it("explains an empty delivery list rather than implying a send reached nobody", async () => {
    renderAt("/integrations/campaigns/camp-1");
    await waitFor(() => expect(screen.getByText(/No delivery records/)).toBeTruthy());
    expect(screen.getByText(/has not been sent/)).toBeTruthy();
  });

  // The same empty table means something DIFFERENT mid-send, and saying so is
  // the point: for the first minute of a real send an empty ledger is normal,
  // and copy that reads "this has not been sent" would be a lie the operator
  // acts on.
  it("reads an empty delivery list differently while a send is running", async () => {
    renderAt("/integrations/campaigns/camp-1", "sending");
    await waitFor(() => expect(screen.getByText(/No delivery records yet/)).toBeTruthy());
    expect(screen.getByText(/in batches/)).toBeTruthy();
  });

  it("starts a send through the builtin, not a mutation", async () => {
    const { calls } = renderAt("/integrations/campaigns/camp-1");
    await waitFor(() =>
      expect((screen.getByPlaceholderText("August product update") as HTMLInputElement).value).toBe(
        "August send",
      ),
    );

    fireEvent.click(screen.getByRole("button", { name: /Start sending/ }));

    await waitFor(() => expect(callsNamed(calls, "campaignStartSend").length).toBe(1));
    // The keyword matters: `builtin` is what the engine's parser dispatches on,
    // and a `mutation` spelling would reach nothing.
    expect(callsNamed(calls, "campaignStartSend")[0]).toBe(
      'builtin campaignStartSend(campaignId: "camp-1")',
    );
    expect(await screen.findByText(/Sending started/)).toBeTruthy();
  });

  it("offers exactly one send action per campaign state", async () => {
    const cases: Array<[string, RegExp | null]> = [
      ["draft", /Start sending/],
      ["scheduled", /Start sending/],
      ["sending", /Pause sending/],
      ["paused", /Resume sending/],
      ["sent", null],
      ["cancelled", null],
    ];
    for (const [status, expected] of cases) {
      const { unmount } = renderAt("/integrations/campaigns/camp-1", status);
      await waitFor(() => expect(screen.getByRole("button", { name: /Cancel campaign/ })).toBeTruthy());

      for (const label of [/Start sending/, /Pause sending/, /Resume sending/]) {
        const present = screen.queryByRole("button", { name: label }) !== null;
        const want = expected !== null && label.source === expected.source;
        expect(present, `status=${status} label=${label.source}`).toBe(want);
      }
      unmount();
    }
  });

  it("pauses through the builtin and says the ledger survives", async () => {
    const { calls } = renderAt("/integrations/campaigns/camp-1", "sending");
    await waitFor(() => expect(screen.getByRole("button", { name: /Pause sending/ })).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: /Pause sending/ }));

    await waitFor(() => expect(callsNamed(calls, "campaignPauseSend").length).toBe(1));
    expect(await screen.findByText(/resuming continues where it stopped/)).toBeTruthy();
  });

  it("says an unknown id is not yours rather than opening a blank form", async () => {
    renderAt("/integrations/campaigns/camp-missing");
    await waitFor(() => expect(screen.getByText(/No campaign of yours has that id/)).toBeTruthy());
  });

  it("creates a campaign with a client-minted id", async () => {
    const { calls } = renderAt("/integrations/campaigns/new");

    await waitFor(() => expect(screen.getByRole("button", { name: "Create draft" })).toBeTruthy());
    fireEvent.change(screen.getByPlaceholderText("August product update"), {
      target: { value: "September send" },
    });
    // Wait for the pickers to have OPTIONS, not merely to exist. A <select>
    // ignores a value with no matching <option>, so firing change before the
    // audience and template reads settle leaves both ids empty, leaves the form
    // incomplete, and makes submit a no-op -- a test that looks like it drove
    // the form and asserted nothing.
    await waitFor(() => {
      expect(screen.getByRole("option", { name: "Beta waitlist" })).toBeTruthy();
      expect(screen.getByRole("option", { name: "August update" })).toBeTruthy();
    });
    const pickers = screen.getAllByRole("combobox");
    fireEvent.change(pickers[0] as HTMLSelectElement, { target: { value: "aud-1" } });
    fireEvent.change(pickers[1] as HTMLSelectElement, { target: { value: "tpl-1" } });
    fireEvent.click(screen.getByRole("button", { name: "Create draft" }));

    await waitFor(() => expect(callsNamed(calls, "createCampaign").length).toBe(1));
    const call = callsNamed(calls, "createCampaign")[0] ?? "";
    expect(call).toContain(`name: "September send"`);
    // The id is minted client-side, so it is present and non-empty without the
    // server having been asked for one first.
    expect(/campaignId: "[^"]+"/.test(call)).toBe(true);
  });
});
