import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { chooseOption, openSelect } from "../selectControl";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

vi.mock("../../src/auth/context", () => ({
  useAuthSource: () => ({ bearer: async () => "test-token", refresh: async () => {} }),
}));

const { CampaignsApp } = await import("../../src/apps/campaigns/CampaignsApp");
const { LocalCampaignsSettingsStore } = await import("../../src/apps/campaigns/settings");
const {
  audienceRow,
  campaignRow,
  deliveryRow,
  fakeConnection,
  recipientRow,
  ruleRow,
  senderRow,
  templateRow,
  withSession,
} = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

const CAMPAIGN_CONCEPT = "v1:campaigns:campaign";

function memoryStore(over: Record<string, unknown> = {}) {
  const bag = new Map<string, string>();
  const store = new LocalCampaignsSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
  if (Object.keys(over).length > 0) store.save({ ...store.load(), ...over });
  return store;
}

/** An upload provider that never touches the network. Nothing in the shell
 *  passes one; the parameter exists for exactly this. */
function fakeUploads(artifactId = "v1:library:artifact:csv") {
  return {
    upload: vi.fn(() => ({
      done: Promise.resolve({
        artifactId,
        title: "list.csv",
        fileKind: "file",
        source: "uploaded",
      }),
      abort: () => {},
    })),
  };
}

function mount(
  connection: Conn,
  sectionId = "campaigns",
  uploads = fakeUploads(),
  settings: Record<string, unknown> = {},
) {
  h.connection = connection;
  const navigate = vi.fn();
  const view = render(
    withSession(
      <CampaignsApp
        sectionId={sectionId}
        navigate={navigate}
        askContext={() => {}}
        store={memoryStore(settings)}
        uploads={uploads}
      />,
    ),
  );
  return { view, navigate, uploads };
}

// ---------------------------------------------------------------------------
// The list
// ---------------------------------------------------------------------------

describe("the campaigns list", () => {
  it("renders the campaigns the cluster returned", async () => {
    const conn = fakeConnection({
      campaigns: [
        campaignRow({ id: "v1:campaigns:campaign:c1", name: "August update" }),
        campaignRow({ id: "v1:campaigns:campaign:c2", name: "September update" }),
      ],
    });
    mount(conn);
    expect(await screen.findByText("August update")).toBeTruthy();
    expect(screen.getByText("September update")).toBeTruthy();
  });

  it("SEEDS UNFILTERED and folds the finished filter client-side", async () => {
    // Seeding filtered would make the toggle re-run the read and re-baseline
    // every arrival cue, so revealing rows the browser already had would
    // announce them as new.
    const conn = fakeConnection({
      campaigns: [campaignRow({ id: "v1:campaigns:campaign:c1", name: "August update" })],
    });
    mount(conn);
    await screen.findByText("August update");
    expect(conn.query.campaigns).toHaveBeenCalledWith({}, expect.anything());
  });

  it("hides finished campaigns by default and shows them under the settings preference", async () => {
    // The in-surface checkbox is gone (DESIGN.md rules 4/10): visibility of
    // filed rows is the app-settings preference, exactly like Fleet's
    // revoked machines and Users' deactivated people.
    const seed = {
      campaigns: [
        campaignRow({ id: "v1:campaigns:campaign:c1", name: "In flight", status: "sending" }),
        campaignRow({ id: "v1:campaigns:campaign:c2", name: "All done", status: "sent" }),
      ],
    };
    const first = mount(fakeConnection(seed));
    await screen.findByText("In flight");
    expect(screen.queryByText("All done")).toBeNull();
    first.view.unmount();

    mount(fakeConnection(seed), "campaigns", fakeUploads(), { showFiled: true });
    expect(await screen.findByText("All done")).toBeTruthy();
  });
});

// ===========================================================================
// THE ARRIVAL CUE, THROUGH THE REAL LIVELIST, IN BOTH DIRECTIONS
// ===========================================================================
// rows.test.ts pins the fingerprint as a function. This pins that the wiring
// carries it: the drain worker's counter ticks must NOT ring, and a rename
// must. A test of only one half would pass against a cue that fires on
// everything or on nothing.
describe("the arrival cue on a live send", () => {
  const CAMPAIGN = campaignRow({
    id: "v1:campaigns:campaign:c1",
    name: "August update",
    status: "sending",
    recipientCount: 100,
    sentCount: 10,
  });

  it("stays SILENT while the send counters tick, and RINGS on a rename", async () => {
    const conn = fakeConnection({ campaigns: [CAMPAIGN] });
    mount(conn);
    await screen.findByText("August update");

    const rowOf = (name: string) =>
      (screen.getByText(name).closest(".os-livelist-row") as HTMLElement) ?? null;

    // A drain tick: only the counters move. This happens several times a
    // second for the whole duration of every send in the cluster.
    await act(async () => {
      conn.subscriptions.emit(
        CAMPAIGN_CONCEPT,
        { ...CAMPAIGN, sentCount: 40, skippedCount: 3, failedCount: 1 },
      );
    });
    expect(rowOf("August update").getAttribute("data-arrival")).toBeNull();
    // ...and the counters DID re-render. Not ringing is not the same as not
    // updating, and this is the half a fingerprint test cannot see.
    expect(screen.getByText("40/100")).toBeTruthy();

    // A rename: news.
    await act(async () => {
      conn.subscriptions.emit(
        CAMPAIGN_CONCEPT,
        { ...CAMPAIGN, name: "September update", sentCount: 41 },
      );
    });
    expect(rowOf("September update").getAttribute("data-arrival")).toBe("updated");
  });

  it("announces a campaign created while somebody is watching", async () => {
    const conn = fakeConnection({ campaigns: [CAMPAIGN] });
    mount(conn);
    await screen.findByText("August update");

    await act(async () => {
      conn.subscriptions.emit(
        CAMPAIGN_CONCEPT,
        campaignRow({ id: "v1:campaigns:campaign:new", name: "Brand new" }),
        "NODE_CREATED",
      );
    });

    const row = (await screen.findByText("Brand new")).closest(".os-livelist-row") as HTMLElement;
    expect(row.getAttribute("data-arrival")).toBe("added");
    expect(within(row).getByText("new")).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// The send bar and the breakdown
// ---------------------------------------------------------------------------

describe("one campaign", () => {
  async function openCampaign(conn: Conn, name = "August update") {
    mount(conn);
    fireEvent.click(await screen.findByText(name));
  }

  it("draws the send as one band a screen reader can read", async () => {
    const conn = fakeConnection({
      campaigns: [
        campaignRow({
          id: "v1:campaigns:campaign:c1",
          status: "sending",
          recipientCount: 100,
          sentCount: 40,
          skippedCount: 10,
          failedCount: 5,
        }),
      ],
    });
    await openCampaign(conn);
    const bar = await screen.findByRole("img", {
      name: "100 recipients: 40 sent, 10 skipped, 5 failed, 45 not yet sent.",
    });
    expect(bar).toBeTruthy();
  });

  it("renders the server's own sentence when a send hit something", async () => {
    const conn = fakeConnection({
      campaigns: [
        campaignRow({
          id: "v1:campaigns:campaign:c1",
          status: "failed",
          lastError: "no email sender is registered on this node",
        }),
      ],
    });
    mount(conn, "campaigns", fakeUploads(), { showFiled: true });
    fireEvent.click(await screen.findByText("August update"));
    // VERBATIM. The preflight's sentence names the thing to go and fix.
    expect(
      await screen.findByText("no email sender is registered on this node"),
    ).toBeTruthy();
  });

  it("reports an unmeasured unique open count as an em dash WITH its reason", async () => {
    const conn = fakeConnection({
      campaigns: [
        campaignRow({ id: "v1:campaigns:campaign:c1", status: "sent", recipientCount: 10, sentCount: 10 }),
      ],
      campaignStats: [{ sent: 10, opens: { total: 90 }, clicks: { total: 5, unique: 3 } }],
    });
    mount(conn, "campaigns", fakeUploads(), { showFiled: true });
    fireEvent.click(await screen.findByText("August update"));
    // A zero there would be this window inventing a fact -- and a zero open
    // rate is a thing operators act on.
    expect(await screen.findByText(/count distinct recipients exactly/)).toBeTruthy();
  });

  it("says soft bounces are not measured rather than reporting a zero", async () => {
    const conn = fakeConnection({
      campaigns: [campaignRow({ id: "v1:campaigns:campaign:c1", status: "sent" })],
      campaignStats: [{ sent: 10 }],
    });
    mount(conn, "campaigns", fakeUploads(), { showFiled: true });
    fireEvent.click(await screen.findByText("August update"));
    expect(await screen.findByText(/Soft bounces are not counted per campaign/)).toBeTruthy();
  });

  it("A REFUSAL IS NOT A ZERO: the ledger says the read was refused", async () => {
    const conn = fakeConnection({
      campaigns: [campaignRow({ id: "v1:campaigns:campaign:c1" })],
      deliveriesForCampaign: new Error("reading deliveries is owner and admin only"),
    });
    await openCampaign(conn);
    expect(await screen.findByText("reading deliveries is owner and admin only")).toBeTruthy();
    expect(screen.getByText(/that is silence, not an empty send/)).toBeTruthy();
  });

  it("PRINTS WHEN THE LEDGER WAS READ, because it is not live", async () => {
    const conn = fakeConnection({
      campaigns: [campaignRow({ id: "v1:campaigns:campaign:c1" })],
      deliveriesForCampaign: [deliveryRow({ id: "v1:campaigns:delivery:d1" })],
    });
    await openCampaign(conn);
    expect(await screen.findByText(/Delivery records are not broadcast/)).toBeTruthy();
  });

  it("asks an in-surface confirm that NAMES the audience size before sending", async () => {
    // `window.confirm` blocks the whole shell and a refusal inside a dialog
    // that then closes is a refusal nobody can re-read.
    const conn = fakeConnection({
      campaigns: [campaignRow({ id: "v1:campaigns:campaign:c1", recipientCount: 4182 })],
    });
    await openCampaign(conn);
    fireEvent.click(await screen.findByText("Send now"));
    expect(await screen.findByText(/Send August update now to 4182 people\?/)).toBeTruthy();

    fireEvent.click(screen.getByText("Send now", { selector: ".os-button" }));
    await waitFor(() =>
      expect(conn.query.campaignStartSend).toHaveBeenCalledWith({
        campaignId: "v1:campaigns:campaign:c1",
      }),
    );
  });

  it("names the merge tags a test send could not resolve", async () => {
    // The only check that catches a typo'd {{fields.compnay}} before the whole
    // audience gets it.
    const conn = fakeConnection({
      campaigns: [campaignRow({ id: "v1:campaigns:campaign:c1" })],
    });
    conn.query.campaignTestSend = vi.fn(async (_args: Record<string, unknown>) =>
      (await import("./harness")).rowsResult([{ unresolved: ["{{fields.compnay}}"] }]),
    );
    await openCampaign(conn);
    fireEvent.change(await screen.findByLabelText("Test recipient address"), {
      target: { value: "me@example.com" },
    });
    fireEvent.click(screen.getByText("Send test"));
    expect(await screen.findByText("{{fields.compnay}}")).toBeTruthy();
    expect(screen.getByText(/did not resolve/)).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// The needs-configuration cue
// ---------------------------------------------------------------------------

describe("a cluster that cannot send mail", () => {
  it("says so ONCE at the top, not once per action", async () => {
    const conn = fakeConnection({
      campaigns: [campaignRow({ id: "v1:campaigns:campaign:c1" })],
      integrationStatus: [
        {
          integrations: [
            { name: "email", configured: "no", health: "degraded", mode: "log", detail: "no sender configured" },
          ],
        },
      ],
    });
    mount(conn);
    const notices = await screen.findAllByText(/nothing sent from here will arrive/);
    expect(notices.length).toBe(1);
    expect(screen.getByText(/Settings, under Integrations/)).toBeTruthy();
  });

  it("stays SILENT on a healthy cluster", async () => {
    const conn = fakeConnection({
      campaigns: [campaignRow({ id: "v1:campaigns:campaign:c1" })],
    });
    mount(conn);
    await screen.findByText("August update");
    expect(screen.queryByText(/nothing sent from here will arrive/)).toBeNull();
  });

  it("stays silent when the report says nothing about email at all", async () => {
    // Warning on "unknown" would put a permanent banner on a healthy cluster.
    const conn = fakeConnection({
      campaigns: [campaignRow({ id: "v1:campaigns:campaign:c1" })],
      integrationStatus: [{ integrations: [{ name: "storage", configured: "unknown" }] }],
    });
    mount(conn);
    await screen.findByText("August update");
    expect(screen.queryByText(/missing what it needs to send mail/)).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// Audiences and the import
// ---------------------------------------------------------------------------

describe("audiences", () => {
  it("shows the roster and the difference a send would actually reach", async () => {
    const conn = fakeConnection({
      audiences: [audienceRow({ id: "v1:campaigns:audience:a1", name: "Newsletter" })],
      recipientsForAudience: [
        recipientRow({ id: "r1", email: "one@acme.com" }),
        recipientRow({ id: "r2", email: "two@acme.com", subscriptionStatus: "unsubscribed" }),
      ],
    });
    mount(conn, "audiences");
    fireEvent.click(await screen.findByText("Newsletter"));
    // The difference between these two IS the suppression rate.
    expect(await screen.findByText("On the list")).toBeTruthy();
    expect(screen.getByText("A send would reach")).toBeTruthy();
    expect(screen.getByText(/1 of these cannot be mailed/)).toBeTruthy();
  });

  it("says the roster is NOT LIVE and what that costs", async () => {
    const conn = fakeConnection({
      audiences: [audienceRow({ id: "v1:campaigns:audience:a1", name: "Newsletter" })],
      recipientsForAudience: [recipientRow({ id: "r1" })],
    });
    mount(conn, "audiences");
    fireEvent.click(await screen.findByText("Newsletter"));
    expect(await screen.findByText(/an address added in another window/)).toBeTruthy();
  });

  it("imports through the SHELL'S ONE UPLOAD PATH and keeps the report on screen", async () => {
    const conn = fakeConnection({
      audiences: [audienceRow({ id: "v1:campaigns:audience:a1", name: "Newsletter" })],
      recipientsForAudience: [],
    });
    conn.query.campaignImportRecipients = vi.fn(async (_args: Record<string, unknown>) =>
      (await import("./harness")).rowsResult([
        {
          added: 412,
          duplicates: 38,
          invalid: 2,
          total: 452,
          samples: [
            { line: 12, text: "not-an-address", reason: "no @" },
            { line: 415, text: ",,", reason: "no email column value" },
          ],
        },
      ]),
    );
    const uploads = fakeUploads("v1:library:artifact:csv");
    mount(conn, "audiences", uploads);
    fireEvent.click(await screen.findByText("Newsletter"));

    const input = document.getElementById("os-audience-csv-v1:campaigns:audience:a1");
    const file = new File(["email\na@b.com\n"], "list.csv", { type: "text/csv" });
    await act(async () => {
      fireEvent.change(input as HTMLInputElement, { target: { files: [file] } });
    });

    // The upload rode the provider, and the artifact id -- never the file --
    // went to the engine, which reads it under the caller's own actor.
    expect(uploads.upload).toHaveBeenCalled();
    await waitFor(() =>
      expect(conn.query.campaignImportRecipients).toHaveBeenCalledWith({
        audienceId: "v1:campaigns:audience:a1",
        artifactId: "v1:library:artifact:csv",
        hasHeader: true,
      }),
    );

    // THE REPORT IS A PANEL, NOT A TOAST: the sentence, the bad lines with
    // their numbers, and a dismiss the person chooses.
    expect(await screen.findByText("412 added, 38 already here, 2 could not be read from 452 rows.")).toBeTruthy();
    expect(screen.getByText("Line 12")).toBeTruthy();
    expect(screen.getByText("not-an-address")).toBeTruthy();
    expect(screen.getByText("Done with this")).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// Templates and the merge tags
// ---------------------------------------------------------------------------

describe("the template editor", () => {
  it("offers the base merge tags and inserts one at the cursor", async () => {
    const conn = fakeConnection({
      templates: [templateRow({ id: "v1:campaigns:template:t1", name: "August copy" })],
      audiences: [],
    });
    mount(conn, "templates");
    fireEvent.click(await screen.findByText("August copy"));

    const body = (await screen.findByLabelText("Message")) as HTMLTextAreaElement;
    fireEvent.change(body, { target: { value: "Hello ," } });
    body.setSelectionRange(6, 6);
    fireEvent.click(screen.getByTitle("Insert {{displayName}}"));
    await waitFor(() => expect(body.value).toBe("Hello {{displayName}},"));
  });

  it("DISCOVERS fields.* from a sampled recipient -- nothing else can", async () => {
    const conn = fakeConnection({
      templates: [templateRow({ id: "v1:campaigns:template:t1", name: "August copy" })],
      audiences: [audienceRow({ id: "v1:campaigns:audience:a1", name: "Newsletter" })],
      recipientsForAudience: [
        recipientRow({ id: "r1", fields: { company: "Acme Corp" } }),
      ],
    });
    mount(conn, "templates");
    fireEvent.click(await screen.findByText("August copy"));

    // Nothing sampled: no fields.* tag anywhere.
    expect(screen.queryByText("{{fields.company}}")).toBeNull();

    chooseOption(screen.getByLabelText("Audience to sample a recipient from"), "Newsletter");
    expect(await screen.findByText("{{fields.company}}")).toBeTruthy();
    // ...and it says what it renders to, which is what makes it documentation.
    expect(screen.getByText("Acme Corp")).toBeTruthy();
  });

  it("says why there is no test send rather than showing a dead control", async () => {
    const conn = fakeConnection({
      templates: [templateRow({ id: "v1:campaigns:template:t1", name: "August copy" })],
      campaigns: [],
    });
    mount(conn, "templates");
    fireEvent.click(await screen.findByText("August copy"));
    expect(await screen.findByText(/No campaign uses this template yet/)).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// Senders
// ---------------------------------------------------------------------------

describe("sender identities", () => {
  it("retires rather than deletes, and says why", async () => {
    const conn = fakeConnection({
      senderIdentities: [senderRow({ id: "v1:campaigns:senderIdentity:s1" })],
    });
    mount(conn, "senders");
    fireEvent.click(await screen.findByText("news@acme.com"));

    // The reason is on screen BEFORE anybody reaches for a control: past
    // campaigns name the row and the reputation history is keyed on its
    // address, which is why there is no delete to find.
    expect(
      await screen.findByText(/sending reputation is kept against its address/),
    ).toBeTruthy();

    fireEvent.click(screen.getByText("Retire this mailbox"));
    // The confirm warns that a campaign naming it will be REFUSED rather than
    // quietly sent from the default mailbox.
    expect(
      await screen.findByText(/REFUSED rather than sent from the default mailbox/),
    ).toBeTruthy();

    fireEvent.click(screen.getByText("Retire", { selector: ".os-button" }));
    await waitFor(() =>
      expect(conn.query.setSenderIdentityStatus).toHaveBeenCalledWith({
        senderIdentityId: "v1:campaigns:senderIdentity:s1",
        status: "disabled",
      }),
    );
  });

  it("offers no delete anywhere", async () => {
    const conn = fakeConnection({
      senderIdentities: [senderRow({ id: "v1:campaigns:senderIdentity:s1" })],
    });
    mount(conn, "senders");
    fireEvent.click(await screen.findByText("news@acme.com"));
    await screen.findByText("Retire this mailbox");
    expect(screen.queryByText(/^Delete/)).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// Rules
// ---------------------------------------------------------------------------

describe("the rules builder", () => {
  it("reads as a sentence and offers concepts from the LIVE registry", async () => {
    const conn = fakeConnection({
      emailRules: [],
      templates: [templateRow({ id: "v1:campaigns:template:t1", name: "Welcome" })],
      concepts: [
        { id: "v1:identity:user", domain: "identity", entity: "user" },
        { id: "v1:accounts:account", domain: "accounts", entity: "account" },
      ],
    });
    mount(conn, "rules");
    fireEvent.click(await screen.findByText("New rule"));

    expect(await screen.findByText("When a")).toBeTruthy();
    // A hardcoded list would make the newest half of a cluster's schema
    // untriggerable with no way to tell. The kit's Select draws its own list,
    // so the choices exist once it is opened -- which is also the only moment
    // a person can see them.
    const list = openSelect(screen.getByLabelText("The kind of thing that fires this rule"));
    expect(within(list).getByRole("option", { name: "user (identity)" })).toBeTruthy();
    expect(within(list).getByRole("option", { name: "account (accounts)" })).toBeTruthy();
  });

  it("names the recipient choice as WHO, and the lane only as an effect", async () => {
    const conn = fakeConnection({ emailRules: [], templates: [], concepts: [] });
    mount(conn, "rules");
    fireEvent.click(await screen.findByText("New rule"));

    expect(await screen.findByText("People in this cluster")).toBeTruthy();
    expect(screen.getByText("Everyone in an audience")).toBeTruthy();
    // The consequence, stated as an effect.
    expect(screen.getByText(/no unsubscribe footer, and the do-not-mail list is not consulted/)).toBeTruthy();
    expect(screen.getByText(/the do-not-mail list is checked before each message/)).toBeTruthy();
    // ...and never as a term of art.
    expect(screen.queryByText(/operational lane/i)).toBeNull();
    expect(screen.queryByText(/marketing lane/i)).toBeNull();
  });

  it("says a new rule sends nothing until it is turned on", async () => {
    const conn = fakeConnection({ emailRules: [], templates: [], concepts: [] });
    mount(conn, "rules");
    fireEvent.click(await screen.findByText("New rule"));
    expect(await screen.findByText(/A name, something to fire on/)).toBeTruthy();
  });

  it("renders the ENGINE'S OWN SENTENCE when a rule failed to arm", async () => {
    const conn = fakeConnection({
      emailRules: [
        ruleRow({
          id: "v1:campaigns:emailRule:r1",
          status: "failed",
          lastError: "bundle validation refused: condition does not parse at line 3",
        }),
      ],
      templates: [],
      concepts: [],
    });
    mount(conn, "rules");
    fireEvent.click(await screen.findByText("Tell the owner about new admins"));
    // Never a paraphrase, never a generic "something went wrong".
    expect(
      await screen.findByText("bundle validation refused: condition does not parse at line 3"),
    ).toBeTruthy();
    expect(screen.getByText("This rule is not running. The cluster refused it.")).toBeTruthy();
  });

  it("names which automation a rule is, because that is the first debugging question", async () => {
    const conn = fakeConnection({
      emailRules: [
        ruleRow({
          id: "v1:campaigns:emailRule:r1",
          status: "active",
          constructName: "emailRuleR1OnUserCreated",
          bundleId: "v1:authoring:bundle:b1",
        }),
      ],
      templates: [],
      concepts: [],
    });
    mount(conn, "rules");
    fireEvent.click(await screen.findByText("Tell the owner about new admins"));
    expect(await screen.findByText("emailRuleR1OnUserCreated")).toBeTruthy();
  });

  it("confirms before arming, and says what arming means", async () => {
    const conn = fakeConnection({
      emailRules: [ruleRow({ id: "v1:campaigns:emailRule:r1" })],
      templates: [],
      concepts: [],
    });
    mount(conn, "rules");
    fireEvent.click(await screen.findByText("Tell the owner about new admins"));
    fireEvent.click(await screen.findByText("Turn it on"));
    expect(
      await screen.findByText(/sends mail on its own every time the event happens/),
    ).toBeTruthy();
    fireEvent.click(screen.getByText("Turn it on", { selector: ".os-button" }));
    await waitFor(() =>
      expect(conn.query.campaignActivateEmailRule).toHaveBeenCalledWith({
        emailRuleId: "v1:campaigns:emailRule:r1",
      }),
    );
  });
});

// ===========================================================================
// THE CLUSTER-WIDE KILL SWITCH
// ===========================================================================
// With `authoredAutomationsEnabled` off, the scheduler's global gate refuses
// every firing on every node while every rule row still reads `active`. That
// is the exact silent failure this banner exists to remove -- and the reason
// the three SILENCES below matter more than the one appearance: a banner that
// fires on a missing answer is a scare on a cluster where nothing is wrong,
// and it would fire on every fresh cluster there is.

describe("rules halted across the whole cluster", () => {
  const RULE = ruleRow({ id: "v1:campaigns:emailRule:r1", status: "active" });

  it("says so OVER THE LIST when the switch is explicitly off", async () => {
    const conn = fakeConnection({
      emailRules: [RULE],
      templates: [],
      concepts: [],
      clusterSettings: [{ id: "cluster", authoredAutomationsEnabled: false }],
    });
    mount(conn, "rules");
    expect(
      await screen.findByText(/Rules are switched off across this whole cluster/),
    ).toBeTruthy();
    // ...and it says where to go, because a rule's own controls cannot fix it.
    expect(screen.getByText(/Settings, under Cluster/)).toBeTruthy();

    // NOT A STATUS ON EACH ROW. The row is untouched -- it still says active,
    // which is true: it is active AND inert, and only the banner can say the
    // second half.
    const row = screen.getByText("Tell the owner about new admins").closest(".os-livelist-row");
    expect(within(row as HTMLElement).getByText("active")).toBeTruthy();
  });

  it("stays SILENT when the switch is explicitly on", async () => {
    const conn = fakeConnection({
      emailRules: [RULE],
      templates: [],
      concepts: [],
      clusterSettings: [{ id: "cluster", authoredAutomationsEnabled: true }],
    });
    mount(conn, "rules");
    await screen.findByText("Tell the owner about new admins");
    expect(screen.queryByText(/switched off across this whole cluster/)).toBeNull();
  });

  it("stays SILENT when the row does not carry the field -- absent is not false", async () => {
    // The SDK's rowBool answers `false` for a missing key. Reading the switch
    // through it would render "every rule here is halted" on a cluster whose
    // shape simply stopped projecting the field.
    const conn = fakeConnection({
      emailRules: [RULE],
      templates: [],
      concepts: [],
      clusterSettings: [{ id: "cluster" }],
    });
    mount(conn, "rules");
    await screen.findByText("Tell the owner about new admins");
    expect(screen.queryByText(/switched off across this whole cluster/)).toBeNull();
  });

  it("stays SILENT when there is no settings row at all -- a fresh cluster is not halted", async () => {
    // The engine's own gate defaults to enabled for an absent row, and an
    // unadmitted read returns ZERO ROWS rather than an error, so this is also
    // the shape a future authz tier's refusal would arrive in.
    const conn = fakeConnection({
      emailRules: [RULE],
      templates: [],
      concepts: [],
      clusterSettings: [],
    });
    mount(conn, "rules");
    await screen.findByText("Tell the owner about new admins");
    expect(screen.queryByText(/switched off across this whole cluster/)).toBeNull();
  });

  it("stays SILENT on a failed read, and says quietly that it could not check", async () => {
    // A REFUSAL IS NOT A ZERO, and it is not an "off" either. It is also not a
    // clean bill of health: the caption says what this window does not know
    // rather than claiming the cluster is fine.
    const conn = fakeConnection({
      emailRules: [RULE],
      templates: [],
      concepts: [],
      clusterSettings: new Error("reading cluster settings is owner only"),
    });
    mount(conn, "rules");
    await screen.findByText("Tell the owner about new admins");
    expect(screen.queryByText(/switched off across this whole cluster/)).toBeNull();
    expect(
      await screen.findByText(/did not say whether rules are switched on globally/),
    ).toBeTruthy();
  });
});

describe("a rule that stopped itself", () => {
  it("does NOT say somebody paused it when the last run failed", async () => {
    // A rule can be paused by its operator OR stop after its runs kept
    // failing, and only one of those is somebody's decision. "You paused
    // this" over the second throws away the only diagnostic there is.
    const conn = fakeConnection({
      emailRules: [
        ruleRow({
          id: "v1:campaigns:emailRule:r1",
          status: "paused",
          constructName: "emailRuleR1OnUserCreated",
          lastError: "send refused: no sender identity is readable by this rule's author",
        }),
      ],
      templates: [],
      concepts: [],
    });
    mount(conn, "rules", fakeUploads(), { showFiled: true });
    fireEvent.click(await screen.findByText("Tell the owner about new admins"));
    expect(await screen.findByText(/Paused, and its last run failed/)).toBeTruthy();
    // The engine's own sentence is what somebody acts on.
    expect(
      screen.getByText("send refused: no sender identity is readable by this rule's author"),
    ).toBeTruthy();
  });

  it("says a clean pause was a stop button", async () => {
    const conn = fakeConnection({
      emailRules: [
        ruleRow({
          id: "v1:campaigns:emailRule:r1",
          status: "paused",
          constructName: "emailRuleR1OnUserCreated",
          lastError: "",
        }),
      ],
      templates: [],
      concepts: [],
    });
    mount(conn, "rules", fakeUploads(), { showFiled: true });
    fireEvent.click(await screen.findByText("Tell the owner about new admins"));
    expect(
      await screen.findByText(/what it generated is still there waiting/),
    ).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// The app's own settings
// ---------------------------------------------------------------------------

describe("the app's settings", () => {
  it("offers exactly the sections the manifest declares", async () => {
    const conn = fakeConnection({});
    mount(conn, "settings");
    const group = await screen.findByLabelText("Default section");
    for (const name of ["Campaigns", "Audiences", "Templates", "Senders", "Rules", "Settings"]) {
      expect(within(group).getByText(name)).toBeTruthy();
    }
  });

  it("says the tracking preference reaches no campaign that exists", async () => {
    const conn = fakeConnection({});
    mount(conn, "settings");
    expect(
      await screen.findByText(/changing this never reaches one that already exists/),
    ).toBeTruthy();
  });
});
