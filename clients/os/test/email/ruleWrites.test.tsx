import { act, render, renderHook, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";


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
