import { act, fireEvent, render, renderHook, screen, waitFor, within } from "@testing-library/react";
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

const { EmailApp } = await import("../../src/apps/email/EmailApp");
const { useCreateRule, useRuleArming, useUpdateRule } = await import("../../src/apps/email/actions");
const { LocalEmailSettingsStore } = await import("../../src/apps/email/settings");
const {
  audienceRow,
  fakeConnection,
  recipientRow,
  rowsResult,
  ruleRow,
  senderRow,
  templateRow,
  withSession,
} = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

function memoryStore(over: Record<string, unknown> = {}) {
  const bag = new Map<string, string>();
  const store = new LocalEmailSettingsStore({
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
      <EmailApp
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
// Audiences and the import
// ---------------------------------------------------------------------------

describe("audiences", () => {
  it.each(["acme", "other"])("keeps the selected organization %s when adding the first member", async (accountId) => {
    const conn = fakeConnection({
      accounts: [
        { id: "self", name: "Operator organization", status: "active" },
        { id: "acme", name: "Acme", status: "active" },
        { id: "other", name: "Other client", status: "active" },
      ],
    });
    conn.query.createAudience.mockImplementation(async (args) => {
      conn.subscriptions.emit("v1:campaigns:audience", audienceRow({
        id: String(args.audienceId), name: String(args.name), accountId: String(args.accountId),
      }), "NODE_CREATED");
      return rowsResult([]);
    });
    conn.query.addRecipient.mockImplementation(async (args) => {
      // The engine requires an explicit organization from operators and
      // rejects references to an audience belonging to a different one.
      if (!args.accountId) throw new Error("organization_required: select the organization this record belongs to");
      if (args.accountId !== accountId) throw new Error("organization_mismatch");
      const row = recipientRow({ id: String(args.recipientId), ...args });
      conn.query.recipientsForAudience.mockResolvedValue(rowsResult([row]));
      return rowsResult([row]);
    });
    mount(conn, "audiences");
    fireEvent.click(await screen.findByRole("button", { name: "New audience" }));
    await waitFor(() => expect(conn.query.clientAccountsAll).toHaveBeenCalled());
    chooseOption(screen.getByLabelText("Organization this audience is for"), accountId === "acme" ? "Acme" : "Other client");
    fireEvent.change(screen.getByLabelText("Audience name"), { target: { value: "Client newsletter" } });
    fireEvent.click(screen.getByRole("button", { name: "Create audience" }));
    fireEvent.change(await screen.findByLabelText("Address to add"), { target: { value: " subscriber@example.test " } });
    fireEvent.change(screen.getByLabelText("Their name"), { target: { value: " Subscriber " } });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));

    expect(await screen.findByText("subscriber@example.test")).toBeTruthy();
    const created = conn.query.createAudience.mock.calls[0]![0];
    expect(created.accountId).toBe(accountId);
    expect(conn.query.addRecipient).toHaveBeenCalledWith(expect.objectContaining({
      audienceId: created.audienceId, accountId, email: "subscriber@example.test", displayName: "Subscriber", source: "manual",
    }));
    expect(conn.query.addRecipient.mock.calls[0]![0]).not.toHaveProperty("ownerUserId");
    expect((screen.getByLabelText("Address to add") as HTMLInputElement).value).toBe("");
    expect(screen.queryByText("That address was not added.")).toBeNull();
  });

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
    expect(screen.getByText("Subscribed recipients")).toBeTruthy();
    expect(screen.getByText(/1 of these cannot be mailed/)).toBeTruthy();
  });

  it("updates the roster automatically when another session adds or unsubscribes a recipient", async () => {
    const conn = fakeConnection({
      audiences: [audienceRow({ id: "v1:campaigns:audience:a1", name: "Newsletter" })],
      recipientsForAudience: [recipientRow({ id: "r1" })],
    });
    mount(conn, "audiences");
    fireEvent.click(await screen.findByText("Newsletter"));
    await screen.findByText(/Updates automatically/);
    vi.useFakeTimers();
    try {
      await act(async () => window.dispatchEvent(new Event("focus")));
      conn.query.recipientsForAudience.mockResolvedValue(rowsResult([recipientRow({ id: "r2", email: "reviewer@example.test", subscriptionStatus: "unsubscribed" })]));
      await act(async () => { await vi.advanceTimersByTimeAsync(5_000); });
      expect(screen.getByText("reviewer@example.test")).toBeTruthy();
      expect(screen.getByText("unsubscribed")).toBeTruthy();
      expect(screen.queryByRole("button", { name: "Read again" })).toBeNull();
    } finally { vi.useRealTimers(); }
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

  it("replaces resource lists with their detail and restores the list with one back control", async () => {
    mount(fakeConnection({ audiences: [audienceRow({ id: "a1" })] }), "audiences");
    fireEvent.click(await screen.findByText("Newsletter"));
    expect(screen.queryByRole("list", { name: "Your audiences" })).toBeNull();
    expect(screen.getAllByRole("navigation", { name: "Breadcrumbs" })).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "Back to Audiences" }));
    expect(await screen.findByText("Newsletter")).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// Senders
// ---------------------------------------------------------------------------

describe("sender identities", () => {
  it("are not a tab: they are set up in Settings", async () => {
    const conn = fakeConnection({ senderIdentities: [senderRow({ id: "v1:campaigns:senderIdentity:s1" })] });
    mount(conn, "senders");
    // A section the app does not declare reads as the campaigns list.
    expect(await screen.findByRole("heading", { level: 3, name: "Campaigns" })).toBeTruthy();
    expect(screen.queryByText("news@acme.com")).toBeNull();
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
    mount(conn, "automations");
    fireEvent.click(await screen.findByRole("button", { name: "New rule" }));

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
    mount(conn, "automations");
    fireEvent.click(await screen.findByRole("button", { name: "New rule" }));

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
    mount(conn, "automations");
    fireEvent.click(await screen.findByRole("button", { name: "New rule" }));
    expect(await screen.findByText(/A name, something to fire on/)).toBeTruthy();
  });

  // THE ONE PLACE A PERSON WRITES AN EXPRESSION. The whole caption is asserted,
  // not a substring: a copy defect is a sentence that reads wrong, and a
  // substring passes either way.
  const conditionHelp = (rowIs: string) =>
    `Leave the condition empty and the rule fires every time. row is ${rowIs}. ` +
    'For example, row.status in ["active", "trial"] && row.plan != "free" means the status is ' +
    "active or trial and the plan is not free. Use == (is), != (is not), in (is one of), " +
    "&& (and), || (or) and ! (not). A condition with a mistake in it is refused when you turn " +
    "the rule on, not later when it fires.";

  it("teaches the condition under its field: what row is, one example, the operators", async () => {
    const conn = fakeConnection({
      emailRules: [],
      templates: [],
      concepts: [{ id: "v1:identity:user", domain: "identity", entity: "user" }],
    });
    mount(conn, "automations");
    fireEvent.click(await screen.findByRole("button", { name: "New rule" }));
    fireEvent.click(screen.getByText("Only sometimes, and other details"));

    const field = screen.getByLabelText("Condition that must hold for the rule to fire");
    expect(field.getAttribute("placeholder")).toBe('row.role == "admin"');
    // A phone that capitalises `row` into `Row` turns a working condition into
    // a refused one the person never typed.
    expect(field.getAttribute("spellcheck")).toBe("false");
    expect(field.getAttribute("autocapitalize")).toBe("off");
    expect(field.getAttribute("autocorrect")).toBe("off");

    // Directly under the field it explains, not three fields further down.
    const help = field.closest(".os-campaign-condition")?.querySelector(".os-caption");
    expect(help?.textContent).toBe(conditionHelp("the record that was created"));
    // What the person would type is set as code: `row`, the example, and the
    // six operators, in a sentence rather than a table.
    expect([...(help?.querySelectorAll("code") ?? [])].map((code) => code.textContent)).toEqual([
      "row",
      'row.status in ["active", "trial"] && row.plan != "free"',
      "==",
      "!=",
      "in",
      "&&",
      "||",
      "!",
    ]);

    // `row` is named in the words of the sentence above it.
    chooseOption(screen.getByLabelText("The kind of thing that fires this rule"), "user (identity)");
    chooseOption(screen.getByLabelText("Which event fires this rule"), "changes");
    expect(help?.textContent).toBe(conditionHelp("the user that changed"));
  });

  it("shows a rule's condition as it was typed, in the code face, wherever it is shown", async () => {
    const conn = fakeConnection({
      emailRules: [ruleRow({ id: "v1:campaigns:emailRule:r1", condition: 'row.role == "admin"' })],
      templates: [templateRow({ id: "v1:campaigns:template:t1", name: "Welcome" })],
      concepts: [],
    });
    mount(conn, "automations");

    const sentence =
      'When a user is created, email Welcome to the cluster owner, but only when row.role == "admin".';
    const line = await screen.findByTitle(sentence);
    expect(line.textContent).toBe(sentence);
    expect(within(line).getByText('row.role == "admin"').tagName).toBe("CODE");

    fireEvent.click(screen.getByText("Tell the owner about new admins"));
    // The detail replaces the list and quotes it as code; the Only when
    // fact carries it in the data voice. Nowhere is it reworded.
    await waitFor(() =>
      expect(
        screen
          .getAllByText('row.role == "admin"')
          .map((node) => node.tagName)
          .sort(),
      ).toEqual(["CODE", "DD"]),
    );
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
    mount(conn, "automations");
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
    mount(conn, "automations");
    fireEvent.click(await screen.findByText("Tell the owner about new admins"));
    expect(await screen.findByText("emailRuleR1OnUserCreated")).toBeTruthy();
  });

  it("confirms before arming, and says what arming means", async () => {
    const conn = fakeConnection({
      emailRules: [ruleRow({ id: "v1:campaigns:emailRule:r1" })],
      templates: [],
      concepts: [],
    });
    mount(conn, "automations");
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
// A REFUSAL, SAID ONCE
// ===========================================================================
// A refused activation reaches the page twice: the engine records its
// sentence on the rule (`lastError`, the rule's notice) and the call fails
// with the same sentence inside two transport frames -- the SDK's call name
// and the server's "MemQL engine failed to execute query. Details:". Measured
// in a browser against a real engine, the page showed both. The two orders the
// record and the error can arrive in are both pinned, because neither is
// guaranteed.

describe("a refused activation", () => {
  const RULE_CONCEPT = "v1:campaigns:emailRule";
  const RULE = ruleRow({
    id: "v1:campaigns:emailRule:r1",
    condition: "row.primaryEmail.includes(@acme.com)",
  });
  const REFUSED =
    'Braces, @ and semicolons can only appear inside quotes, like row.email == "boss@acme.com".';
  const CALL_ERROR = `campaignActivateEmailRule: MemQL engine failed to execute query. Details: ${REFUSED}`;

  /** Once, and without the frames. Counted over the page's text rather than
   *  matched as a node, so a copy inside its wrapping counts too. */
  function expectSaidOnce() {
    const text = document.body.textContent ?? "";
    expect(text.split(REFUSED).length - 1).toBe(1);
    expect(text).not.toContain("MemQL engine failed to execute query");
    expect(text).not.toContain("campaignActivateEmailRule");
  }

  async function turnOn(conn: Conn) {
    mount(conn, "automations");
    fireEvent.click(await screen.findByText("Tell the owner about new admins"));
    fireEvent.click(await screen.findByText("Turn it on"));
    await act(async () => {
      fireEvent.click(screen.getByText("Turn it on", { selector: ".os-button" }));
    });
  }

  it("is said once, in the rule's notice, when the engine has recorded it", async () => {
    const conn = fakeConnection({ emailRules: [RULE], templates: [], concepts: [] });
    conn.query.campaignActivateEmailRule.mockImplementation(async () => {
      // What the engine does: record the refusal on the rule, then refuse.
      conn.subscriptions.emit(RULE_CONCEPT, { ...RULE, status: "failed", lastError: REFUSED });
      throw new Error(CALL_ERROR);
    });
    await turnOn(conn);

    expect(await screen.findByText("This rule is not running. The cluster refused it.")).toBeTruthy();
    expectSaidOnce();
    // The panel the person clicked in says where the reason is, not what it is.
    expect(screen.getByText("Not running -- the cluster refused it. The reason is above.")).toBeTruthy();
  });

  it("is said once, unframed, where it was asked -- and steps aside when the record lands", async () => {
    const conn = fakeConnection({ emailRules: [RULE], templates: [], concepts: [] });
    conn.query.campaignActivateEmailRule.mockImplementation(async () => {
      throw new Error(CALL_ERROR);
    });
    await turnOn(conn);

    // The rule does not carry it yet, so the panel says it -- once, plainly.
    await waitFor(() => expect(document.body.textContent).toContain(REFUSED));
    expectSaidOnce();
    expect(screen.getByText("The cluster refused that.")).toBeTruthy();

    // The record arrives: the rule's notice says it, and the panel's copy goes.
    await act(async () => {
      conn.subscriptions.emit(RULE_CONCEPT, { ...RULE, status: "failed", lastError: REFUSED });
    });
    expectSaidOnce();
    expect(screen.getByText("This rule is not running. The cluster refused it.")).toBeTruthy();
    expect(screen.queryByText("The cluster refused that.")).toBeNull();
  });

  it("is said once when the engine's own sentence opens with its package name", async () => {
    // The engine records the sentence on the rule exactly as it wrote it, and the
    // call fails with the same words inside two transport frames. The package
    // name is the engine's, not the person's: it comes off the call's sentence,
    // so the two have to be compared the same way or the page says it twice.
    const PACKAGED = "emailrules: arming an event-email rule is owner or developer only -- it authors an automation that sends mail on this cluster's behalf";
    const SAID = PACKAGED.replace("emailrules: ", "");
    const conn = fakeConnection({ emailRules: [RULE], templates: [], concepts: [] });
    conn.query.campaignActivateEmailRule.mockImplementation(async () => {
      conn.subscriptions.emit(RULE_CONCEPT, { ...RULE, status: "failed", lastError: PACKAGED });
      throw new Error(`campaignActivateEmailRule: MemQL engine failed to execute query. Details: ${PACKAGED}`);
    });
    await turnOn(conn);

    expect(await screen.findByText("This rule is not running. The cluster refused it.")).toBeTruthy();
    const text = document.body.textContent ?? "";
    expect(text.split(SAID).length - 1).toBe(1);
    expect(text).not.toContain("MemQL engine failed to execute query");
    expect(text).not.toContain("campaignActivateEmailRule");
    expect(screen.getByText("Not running -- the cluster refused it. The reason is above.")).toBeTruthy();
  });

  it("shows the sentence without the engine's package name where the rule does not carry it yet", async () => {
    const PACKAGED = "emailrules: arming an event-email rule is owner or developer only";
    const conn = fakeConnection({ emailRules: [RULE], templates: [], concepts: [] });
    conn.query.campaignActivateEmailRule.mockImplementation(async () => {
      throw new Error(`campaignActivateEmailRule: MemQL engine failed to execute query. Details: ${PACKAGED}`);
    });
    await turnOn(conn);

    await waitFor(() => expect(screen.getByText("The cluster refused that.")).toBeTruthy());
    const text = document.body.textContent ?? "";
    expect(text).toContain("arming an event-email rule is owner or developer only");
    expect(text).not.toContain("emailrules:");

    // The record arrives with the sentence as the engine wrote it: the panel's copy goes.
    await act(async () => {
      conn.subscriptions.emit(RULE_CONCEPT, { ...RULE, status: "failed", lastError: PACKAGED });
    });
    expect(screen.queryByText("The cluster refused that.")).toBeNull();
    expect((document.body.textContent ?? "").split("arming an event-email rule is owner or developer only").length - 1).toBe(1);
  });

  it("frames nothing on a refused save either", async () => {
    const conn = fakeConnection({
      emailRules: [],
      templates: [templateRow({ id: "v1:campaigns:template:t1", name: "Welcome" })],
      concepts: [{ id: "v1:identity:user", domain: "identity", entity: "user" }],
    });
    const said = "recipientRoles must be a list of role slugs";
    conn.query.createEmailRule.mockImplementation(async () => {
      throw new Error(`createEmailRule: MemQL engine failed to execute query. Details: ${said}`);
    });
    mount(conn, "automations");
    fireEvent.click(await screen.findByRole("button", { name: "New rule" }));
    fireEvent.change(screen.getByLabelText("Rule name"), { target: { value: "Welcome new users" } });
    chooseOption(screen.getByLabelText("The kind of thing that fires this rule"), "user (identity)");
    await waitFor(() => expect(screen.getByLabelText("Organization this rule is for").textContent).toContain("Operator organization"));
    chooseOption(screen.getByLabelText("Template to send"), "Welcome");
    await act(async () => {
      fireEvent.click(screen.getByText("Create rule"));
    });

    expect(await screen.findByText(said)).toBeTruthy();
    expect(screen.getByText("This rule was not created.")).toBeTruthy();
    const text = document.body.textContent ?? "";
    expect(text).not.toContain("MemQL engine failed to execute query");
    expect(text).not.toContain("createEmailRule");
  });
});

// What a rules write puts in its error slot is the SDK's refusalSentence of the
// refused call: the call's name, the server's frame and a leading package name
// come off, once each, and the rest is the cluster's own words.
describe("the sentence a refused rules write carries", () => {
  const FRAME = "MemQL engine failed to execute query. Details: ";
  const REFUSED = 'Braces, @ and semicolons can only appear inside quotes, like row.email == "boss@acme.com".';
  const PACKAGED = "emailrules: arming an event-email rule is owner or developer only";
  const FACTS = {
    name: "Tell the owner",
    description: "",
    triggerConcept: "v1:identity:user",
    eventKind: "created",
    condition: "",
    templateId: "t1",
    recipientMode: "cluster_roles" as const,
    recipientRoles: ["owner"],
    audienceId: "",
    recipientField: "",
    accountId: "self",
    senderIdentityId: "",
  };

  type Call = "activate" | "retire" | "setStatus" | "create" | "update";
  const NAME: Record<Call, string> = {
    activate: "campaignActivateEmailRule",
    retire: "campaignRetireEmailRule",
    setStatus: "setEmailRuleStatus",
    create: "createEmailRule",
    update: "updateEmailRule",
  };
  const CALLS = Object.keys(NAME) as Call[];

  /** The error slot of one rules write after the cluster refused it with `message`. */
  async function refusedWith(call: Call, message: string): Promise<string> {
    const conn = fakeConnection({});
    conn.query[NAME[call] as "createEmailRule"].mockRejectedValue(new Error(message));
    h.connection = conn;
    if (call === "create") {
      const { result } = renderHook(() => useCreateRule());
      await act(async () => { await result.current.create(FACTS); });
      return result.current.error;
    }
    if (call === "update") {
      const { result } = renderHook(() => useUpdateRule());
      await act(async () => { await result.current.update("r1", FACTS); });
      return result.current.error;
    }
    const { result } = renderHook(() => useRuleArming());
    await act(async () => {
      if (call === "setStatus") await result.current.setStatus("r1", "paused");
      else await result.current[call]("r1");
    });
    return result.current.error;
  }

  it.each(CALLS)("takes off the call's name and the server's frame, and nothing else: %s", async (call) => {
    expect(await refusedWith(call, `${NAME[call]}: ${FRAME}${REFUSED}`)).toBe(REFUSED);
  });

  it.each(CALLS)("takes off the call's name where the server added no frame: %s", async (call) => {
    expect(await refusedWith(call, `${NAME[call]}: ${REFUSED}`)).toBe(REFUSED);
  });

  it("takes off the engine's own package name as well, once", async () => {
    expect(await refusedWith("activate", `campaignActivateEmailRule: ${FRAME}${PACKAGED}`)).toBe(
      "arming an event-email rule is owner or developer only",
    );
    expect(await refusedWith("retire", "authoring: actor x cannot activate bundle owned by y")).toBe(
      "actor x cannot activate bundle owned by y",
    );
  });

  it("keeps a server frame with nothing in it, which is then all the cluster said", async () => {
    expect(await refusedWith("setStatus", "setEmailRuleStatus: MemQL engine failed to execute query.")).toBe(
      "MemQL engine failed to execute query.",
    );
  });

  it("leaves its own words alone where there is no connection", async () => {
    h.connection = null;
    const { result } = renderHook(() => useRuleArming());
    await act(async () => { await result.current.activate("r1"); });
    expect(result.current.error).toBe("Not connected to the cluster, so nothing was written.");
  });

  it("answers the first refused call's sentence from the three arming calls, in its own name", async () => {
    // The one slot is shared by activate, retire and pause/resume; the name that comes off is the failing call's.
    const conn = fakeConnection({});
    conn.query.campaignRetireEmailRule.mockRejectedValue(new Error(`campaignRetireEmailRule: ${REFUSED}`));
    h.connection = conn;
    const { result } = renderHook(() => useRuleArming());
    await act(async () => { await result.current.retire("r1"); });
    expect(result.current.error).toBe(REFUSED);
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
    mount(conn, "automations");
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
    mount(conn, "automations");
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
    mount(conn, "automations");
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
    mount(conn, "automations");
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
    mount(conn, "automations");
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
    mount(conn, "automations", fakeUploads(), { includeArchived: true });
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
    mount(conn, "automations", fakeUploads(), { includeArchived: true });
    fireEvent.click(await screen.findByText("Tell the owner about new admins"));
    expect(
      await screen.findByText(/what it generated is still there waiting/),
    ).toBeTruthy();
  });
});

describe("record list counts", () => {
  const lists = [
    { name: "audiences", section: "audiences", query: "audiences", row: audienceRow({ id: "count-a", name: "Counted audience" }) },
    { name: "templates", section: "templates", query: "templates", row: templateRow({ id: "count-t", name: "Counted template" }) },
    { name: "automations", section: "automations", query: "emailRules", row: ruleRow({ id: "count-r", name: "Counted rule" }) },
  ] as const;

  it.each(lists)("counts $name only after its authorized read settles", async ({ section, query, row }) => {
    const conn = fakeConnection({ [query]: [row] });
    const { view } = mount(conn, section);
    expect(view.container.querySelector(".os-head-meta")).toBeNull();
    await waitFor(() => expect(view.container.querySelector(".os-head-meta")?.textContent).toBe("1"));
    expect(view.container.querySelectorAll(".os-record-list .os-record-row")).toHaveLength(1);
  });

  it.each(lists)("does not turn a denied $name read into zero", async ({ section, query }) => {
    const conn = fakeConnection();
    conn.query[query].mockRejectedValue(new Error("permission denied"));
    const { view } = mount(conn, section);
    await screen.findByText(/permission denied/);
    expect(view.container.querySelector(".os-head-meta")).toBeNull();
  });
});
