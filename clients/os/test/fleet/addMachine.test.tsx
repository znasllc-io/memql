import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { FleetSettings, FleetSettingsStore } from "../../src/apps/fleet/settings";

const h = vi.hoisted(() => ({
  connection: null as unknown,
  mint: vi.fn(),
  revoke: vi.fn(),
  chat: vi.fn(),
}));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

// The mint and the revoke are the two calls this page makes that are NOT
// graph reads: both ride the connection's dispatcher through the SDK's
// identity surface. The chat is the round trip (D14).
vi.mock("@znasllc-io/memql-sdk-core/identity", () => ({
  createWorkerToken: (...args: unknown[]) => h.mint(...args),
  revokeWorkerToken: (...args: unknown[]) => h.revoke(...args),
}));
vi.mock("@znasllc-io/memql-sdk-core/ai", () => ({
  aiChat: (...args: unknown[]) => h.chat(...args),
}));

const { MachinesProvider, WORKER_REGISTRATION_CONCEPT } = await import("../../src/live/machines");
const { FleetApp } = await import("../../src/apps/fleet/FleetApp");
const { DEFAULT_FLEET_SETTINGS } = await import("../../src/apps/fleet/settings");
const { installCommand, uninstallCommand, workerClusterUrl, INSTALL_PLATFORMS } = await import(
  "../../src/apps/fleet/addMachine/install"
);
const { fakeConnection, machineRow, withSession } = await import("./harness");

// THE GUIDED INSTALL, through the real Fleet app (design record
// 2026-09-08-cockpit-install-wizard, section 6). Everything goes through
// `connection.query` and `connection.subscriptions` exactly as production
// does, so the real LiveCollection, the real fold and the real projections run;
// what is asserted is what a person SEES and what reached the WIRE.

// ASSEMBLED FROM PARTS, deliberately. The repo's secret scanner matches
// `mql_<kind>_<43 base64url chars>` as one literal, and a test fixture that
// happens to reach that length would red the gitleaks lane on a file that
// contains no secret. Joining at runtime means no line here can ever match.
const TOKEN = ["mql", "wkr", "notARealTokenOnlyATestFixture"].join("_");
const IDENTITY = "v1:identity:identity:tok-1";

type Conn = ReturnType<typeof fakeConnection>;

async function click(el: Element) {
  await act(async () => {
    (el as HTMLElement).click();
  });
}

async function type(el: HTMLInputElement, value: string) {
  await act(async () => {
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
    setter.call(el, value);
    el.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

async function settle() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
}

function memoryStore(initial: FleetSettings): FleetSettingsStore {
  let held = initial;
  return { load: () => held, save: (next) => void (held = next) };
}

function mount(connection: Conn, section = "machines") {
  h.connection = connection;
  const view = render(
    withSession(
      <MachinesProvider>
        <FleetApp
          sectionId={section}
          navigate={vi.fn()}
          askContext={vi.fn()}
          store={memoryStore(DEFAULT_FLEET_SETTINGS)}
        />
      </MachinesProvider>,
    ),
  );
  return view;
}

function rerenderAt(view: ReturnType<typeof render>, section: string) {
  view.rerender(
    withSession(
      <MachinesProvider>
        <FleetApp
          sectionId={section}
          navigate={vi.fn()}
          askContext={vi.fn()}
          store={memoryStore(DEFAULT_FLEET_SETTINGS)}
        />
      </MachinesProvider>,
    ),
  );
}

async function openPage() {
  await settle();
  await click(screen.getByRole("button", { name: "Add a machine" }));
}

async function describeAndMint(name = "studio-mac-mini", opts: { computerUse?: boolean; inference?: boolean; linux?: boolean } = {}) {
  await openPage();
  await type(screen.getByLabelText("Machine name") as HTMLInputElement, name);
  if (opts.linux) await click(screen.getByRole("radio", { name: "Linux" }));
  if (opts.computerUse) await click(screen.getByLabelText(/^Computer use$/));
  if (opts.inference) await click(screen.getByLabelText(/Run local models/));
  await click(screen.getByRole("button", { name: "Mint a token" }));
  await settle();
}

/** The registration the cockpit writes when the minted token connects. */
function arrival(over: Record<string, unknown> = {}) {
  return machineRow({
    id: "v1:worker:registration:mini",
    identityId: IDENTITY,
    name: "mini.local",
    displayName: "",
    platformInfo: { os: "darwin", arch: "arm64", hostname: "mini.local" },
    capabilities: ["HEADLESS"],
    buildTag: "headless",
    version: "v2026.9.1",
    lastSeenAt: new Date().toISOString(),
    ...over,
  });
}

function emit(connection: Conn, row: ReturnType<typeof machineRow>, kind = "NODE_CREATED") {
  act(() => {
    connection.subscriptions.emit(WORKER_REGISTRATION_CONCEPT, row, kind);
  });
}

async function beat(connection: Conn, row: ReturnType<typeof machineRow>, secondsLater: number) {
  const at = new Date(Date.now() + secondsLater * 1000).toISOString();
  emit(connection, { ...row, lastSeenAt: at }, "NODE_UPDATED");
  await settle();
}

const bar = () => screen.getByRole("group", { name: "What you can do with this" });

/** The page's four stops, in order -- the rail's OWN items, not the lists a
 *  stop body holds (the numbered steps, the checks' own rail). */
function stopStates(): (string | null)[] {
  const rail = screen.getByRole("list", { name: "Adding a machine" });
  return Array.from(rail.querySelectorAll(":scope > li")).map((li) => li.getAttribute("data-state"));
}

beforeEach(() => {
  h.connection = null;
  h.mint.mockReset();
  h.revoke.mockReset();
  h.chat.mockReset();
  h.mint.mockResolvedValue({
    success: true,
    plainToken: TOKEN,
    identityId: IDENTITY,
    ownerUserId: "v1:identity:user:me",
    errorCode: "",
    errorMessage: "",
  });
  h.revoke.mockResolvedValue({ success: true, errorCode: "", errorMessage: "" });
  globalThis.localStorage.clear();
  globalThis.sessionStorage.clear();
});

afterEach(cleanup);

describe("the page replaces the list", () => {
  it("opens from the Head's one act, with its own Head and a way back, and no list beneath it", async () => {
    mount(fakeConnection({ myWorkersWithStatus: [arrival({ id: "v1:worker:registration:other", identityId: "tok-9" })] }));
    await openPage();
    expect(screen.getByRole("region", { name: "Add a machine" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Back to Machines" })).toBeTruthy();
    // ONE Head in the scroller (interface rule 11): the list's is gone.
    expect(screen.queryByRole("heading", { name: "Machines" })).toBeNull();
    expect(screen.queryByRole("list", { name: "Your machines" })).toBeNull();
    // The rail: four stops, the first open, the rest ahead.
    expect(stopStates()).toEqual(["open", "ahead", "ahead", "ahead"]);
  });

  it("offers Mint only once a name is typed, and Cancel before that leaves with nothing created", async () => {
    mount(fakeConnection());
    await openPage();
    expect(within(bar()).queryByRole("button", { name: "Mint a token" })).toBeNull();
    await type(screen.getByLabelText("Machine name") as HTMLInputElement, "box");
    expect(within(bar()).getByRole("button", { name: "Mint a token" })).toBeTruthy();
    await click(within(bar()).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("region", { name: "Add a machine" })).toBeNull();
    expect(screen.getByRole("heading", { name: "Machines" })).toBeTruthy();
    expect(h.mint).not.toHaveBeenCalled();
  });
});

describe("minting", () => {
  it("mints once with the typed name and shows the token in a copy field with the one-time warning", async () => {
    mount(fakeConnection());
    await describeAndMint();
    expect(h.mint).toHaveBeenCalledTimes(1);
    expect(h.mint.mock.calls[0]?.[1]).toEqual({ name: "studio-mac-mini" });
    const token = screen.getByLabelText("the worker token") as HTMLInputElement;
    expect(token.value).toBe(TOKEN);
    expect(token.readOnly).toBe(true);
    expect(screen.getByRole("button", { name: "Copy the worker token" })).toBeTruthy();
    expect(screen.getByText(/The token is shown only once/)).toBeTruthy();
  });

  it("NEVER writes the token to browser storage or a URL", async () => {
    mount(fakeConnection());
    await describeAndMint();
    const dump = [
      JSON.stringify(globalThis.localStorage),
      JSON.stringify(globalThis.sessionStorage),
      globalThis.location?.href ?? "",
      document.cookie,
    ].join("|");
    expect(dump).not.toContain(TOKEN);
    expect(dump).not.toContain("mql_wkr_");
  });

  it("composes the documented one-liner against this cluster's api host, with the flags asked for", async () => {
    mount(fakeConnection());
    await describeAndMint("box", { linux: true, computerUse: true, inference: true });
    const command = (screen.getByLabelText("the install command") as HTMLInputElement).value;
    expect(command).toBe(
      installCommand({
        platform: "linux",
        clusterUrl: "https://api.memql.example.com",
        token: TOKEN,
        computerUse: true,
        inference: true,
      }),
    );
    expect(command).toContain("install-linux.sh");
    expect(command).toContain("--cluster https://api.memql.example.com --computeruse --inference");
  });

  it("lights two marks while waiting -- Install open for the person, Connect current for the cluster", async () => {
    mount(fakeConnection());
    await describeAndMint();
    expect(stopStates()).toEqual(["done", "open", "current", "ahead"]);
    expect(within(bar()).getByText("Waiting for studio-mac-mini")).toBeTruthy();
    // The Connect stop's line says what the cluster is doing; its body is
    // one click away, behind the person's own stop.
    expect(screen.getByText("Listening for the machine")).toBeTruthy();
  });

  it("states the second command up front when local models were asked for", async () => {
    mount(fakeConnection());
    await describeAndMint("box", { inference: true });
    expect((screen.getByLabelText("the local models setup command") as HTMLInputElement).value).toBe(
      "/usr/local/bin/memql worker setup --inference",
    );
    expect(screen.getByText(/once the installer prints SUCCESS/)).toBeTruthy();
  });

  it("renders a refused mint in surface, creates nothing, and offers Mint again", async () => {
    h.mint.mockResolvedValue({
      success: false,
      plainToken: "",
      identityId: "",
      ownerUserId: "",
      errorCode: "forbidden",
      errorMessage: "this account may not mint worker tokens",
    });
    mount(fakeConnection());
    await describeAndMint();
    await waitFor(() => expect(screen.getByText("this account may not mint worker tokens")).toBeTruthy());
    expect(screen.getByText("The connection token could not be created.")).toBeTruthy();
    expect(screen.queryByLabelText("the worker token")).toBeNull();
    expect(within(bar()).getByRole("button", { name: "Mint a token" })).toBeTruthy();
  });
});

describe("the registration is matched, never counted", () => {
  it("settles Install and Connect when the registration carrying the minted identity arrives", async () => {
    const connection = fakeConnection();
    mount(connection);
    await describeAndMint();
    emit(connection, arrival());
    await settle();
    expect(stopStates()).toEqual(["done", "done", "done", "current"]);
    expect(screen.getByText("Connected as mini.local -- darwin/arm64 -- cockpit v2026.9.1")).toBeTruthy();
    expect(within(bar()).getByText("Connected")).toBeTruthy();
  });

  it("settles nothing when a DIFFERENT machine arrives, however many do", async () => {
    const connection = fakeConnection();
    mount(connection);
    await describeAndMint();
    emit(connection, arrival({ id: "v1:worker:registration:a", identityId: "tok-9" }));
    emit(connection, arrival({ id: "v1:worker:registration:b", identityId: "tok-8" }));
    await settle();
    expect(within(bar()).getByText("Waiting for studio-mac-mini")).toBeTruthy();
    expect(stopStates()).toEqual(["done", "open", "current", "ahead"]);
  });

  it("matches a bare identity id, which is what the wire delivers", async () => {
    const connection = fakeConnection();
    mount(connection);
    await describeAndMint();
    emit(connection, arrival({ identityId: "tok-1" }));
    await settle();
    expect(within(bar()).getByText("Connected")).toBeTruthy();
  });

  it("puts the typed name on the machine, once, and never again on a heartbeat", async () => {
    const connection = fakeConnection();
    mount(connection);
    await describeAndMint("studio-mac-mini");
    const row = arrival();
    emit(connection, row);
    await settle();
    expect(connection.query.renameWorker).toHaveBeenCalledExactlyOnceWith({
      registrationId: "v1:worker:registration:mini",
      displayName: "studio-mac-mini",
    });
    await beat(connection, row, 15);
    await beat(connection, row, 30);
    expect(connection.query.renameWorker).toHaveBeenCalledTimes(1);
  });
});

describe("the checks", () => {
  it("draws the connection moving until two heartbeats past the registration, then steady and Ready", async () => {
    const connection = fakeConnection();
    mount(connection);
    await describeAndMint();
    const row = arrival();
    emit(connection, row);
    await settle();
    const checks = () => screen.getByRole("list", { name: "Checks on this machine" });
    expect(within(checks()).getAllByRole("listitem")[0]?.getAttribute("data-state")).toBe("current");
    expect(within(checks()).getByText(/Listening for its first heartbeat/)).toBeTruthy();

    await beat(connection, row, 15);
    expect(within(checks()).getByText(/Heartbeat 1 of 2/)).toBeTruthy();
    await beat(connection, row, 30);
    expect(within(checks()).getAllByRole("listitem")[0]?.getAttribute("data-state")).toBe("done");
    expect(within(checks()).getByText(/Online and steady/)).toBeTruthy();
    expect(within(bar()).getByText("Ready")).toBeTruthy();
    // Done is primary now; Open names the machine.
    expect(within(bar()).getByRole("button", { name: "Open mini.local" })).toBeTruthy();
    expect(within(bar()).getByRole("button", { name: "Done" }).getAttribute("data-tone")).toBe("primary");
  });

  it("names the missing macOS permission with its repair and settles from a measured heartbeat without rerunning setup", async () => {
    const connection = fakeConnection();
    mount(connection);
    await describeAndMint("mini", { computerUse: true });
    const row = arrival({
      buildTag: "computeruse",
      capabilities: ["HEADLESS", "COMPUTERUSE"],
      permissions: { accessibility: true, screen_recording: false, x11_display: false, detail: "" },
    });
    emit(connection, row);
    await settle();
    expect(screen.getByText("Screen Recording not granted to the running worker.")).toBeTruthy();
    expect(screen.getByText(/System Settings → Privacy & Security → Screen Recording/)).toBeTruthy();
    expect(screen.queryByLabelText("the macos permissions command")).toBeNull();
    const permissionResults = screen.getByRole("list", { name: "Permission results" });
    expect(within(permissionResults).getByText("Accessibility").closest("li")?.getAttribute("data-state")).toBe("done");
    expect(within(permissionResults).getByText("Screen Recording").closest("li")?.getAttribute("data-state")).toBe("open");

    emit(
      connection,
      { ...row, permissions: { accessibility_state: "granted", screen_recording_state: "granted", x11_display_state: "unknown", checked_at: new Date().toISOString(), probe_context: "worker-process" } },
      "NODE_UPDATED",
    );
    await settle();
    expect(screen.getByText("Accessibility and Screen Recording granted.")).toBeTruthy();
    emit(connection, { ...row, permissions: { accessibility_state: "granted", screen_recording_state: "unknown", probe_context: "worker-process" } }, "NODE_UPDATED");
    await settle();
    expect(screen.queryByText("Accessibility and Screen Recording granted.")).toBeNull();
    expect(within(screen.getByRole("list", { name: "Permission results" })).getByText("Screen Recording").closest("li")?.getAttribute("data-state")).toBe("unknown");
  });

  it("offers the recommended pull once a runtime is reported, and shows a refusal in surface", async () => {
    const connection = fakeConnection();
    connection.query.fleetPullRecommended.mockRejectedValueOnce(new Error("machine is under the floor for local models"));
    mount(connection);
    await describeAndMint("mini", { inference: true });
    emit(connection, arrival({ hardware: { chip: "M2", memoryBytes: 1, runtimes: [{ name: "ollama", version: "0.11" }] } }));
    await settle();
    expect(screen.getByText("No models yet.")).toBeTruthy();
    await click(screen.getByRole("button", { name: "Pull the recommended models" }));
    await settle();
    expect(connection.query.fleetPullRecommended).toHaveBeenCalledExactlyOnceWith({
      registrationId: "v1:worker:registration:mini",
    });
    expect(screen.getByText("machine is under the floor for local models")).toBeTruthy();
  });

  it("automatically checks the served model without a chat button or reply transcript", async () => {
    h.chat.mockResolvedValue({ message: { role: "assistant", content: "hello" }, provider: "fleet:llama3.1:8b" });
    const connection = fakeConnection();
    mount(connection);
    await describeAndMint("mini", { inference: true });
    emit(
      connection,
      arrival({
        hardware: { chip: "M2", memoryBytes: 1, runtimes: [{ name: "ollama", version: "0.11" }] },
        labels: { "model:llama3.1:8b": "ctx=131072,structured=1,max=2" },
      }),
    );
    await settle();
    expect(screen.getByText("Serving one model: llama3.1:8b.")).toBeTruthy();
    await waitFor(() => expect(h.chat).toHaveBeenCalledTimes(1));
    expect(screen.queryByRole("button", { name: "Ask it something" })).toBeNull();
    expect(h.chat.mock.calls[0]?.[2]).toMatchObject({ provider: "fleet:llama3.1:8b", fleetRegistrationId: "v1:worker:registration:mini" });
    await waitFor(() => expect(screen.getByText("Response verified on this machine.")).toBeTruthy());
    expect(screen.queryByText("hello")).toBeNull();
  });
});

describe("cancel after a mint asks which of two things", () => {
  it("keeps setup incomplete during the automatic check and retains it across navigation", async () => {
    let answer!: (value: unknown) => void;
    h.chat.mockImplementationOnce(() => new Promise(resolve => { answer = resolve; }));
    const connection = fakeConnection();
    const view = mount(connection);
    await describeAndMint("mini", { inference: true });
    const row = arrival({ labels: { "model:a-embed": "embeddings=1", "model:z-chat": "tools=1" } });
    emit(connection, row);
    await beat(connection, row, 15);
    await beat(connection, row, 30);
    await waitFor(() => expect(h.chat).toHaveBeenCalledTimes(1));
    expect(h.chat.mock.calls[0]?.[2]).toMatchObject({ provider: "fleet:z-chat" });
    expect(within(bar()).queryByText("Ready")).toBeNull();
    expect(within(bar()).queryByRole("button", { name: "Done" })).toBeNull();
    expect(stopStates().at(-1)).toBe("current");
    rerenderAt(view, "policies");
    await act(async () => answer({ message: { content: "hello" } }));
    rerenderAt(view, "machines");
    await waitFor(() => expect(within(bar()).getByText("Ready")).toBeTruthy());
    expect(within(bar()).getByRole("button", { name: "Done" })).toBeTruthy();
    expect(screen.queryByText("hello")).toBeNull();
    expect(h.chat).toHaveBeenCalledTimes(1);
  });

  it("shows a retry after a failed response and never marks the failed setup ready", async () => {
    h.chat.mockRejectedValueOnce(new Error("Runtime unavailable"));
    h.chat.mockResolvedValueOnce({ message: { content: "hello" } });
    const connection = fakeConnection();
    mount(connection);
    await describeAndMint("mini", { inference: true });
    const row = arrival({ labels: { "model:z-chat": "tools=1" } });
    emit(connection, row);
    await beat(connection, row, 15);
    await beat(connection, row, 30);
    await waitFor(() => expect(screen.getByRole("button", { name: "Retry check" })).toBeTruthy());
    expect(within(bar()).queryByText("Ready")).toBeNull();
    expect(within(bar()).queryByRole("button", { name: "Done" })).toBeNull();
    await click(screen.getByRole("button", { name: "Retry check" }));
    await waitFor(() => expect(within(bar()).getByText("Ready")).toBeTruthy());
    expect(h.chat).toHaveBeenCalledTimes(2);
  });

  it("keeps the token and leaves without revoking", async () => {
    mount(fakeConnection());
    await describeAndMint();
    await click(within(bar()).getByRole("button", { name: "Cancel" }));
    expect(within(bar()).getByText("Leave?")).toBeTruthy();
    expect(screen.getByText(/still works/)).toBeTruthy();
    // The uninstall line is offered right here, for a person who already ran the install.
    expect((screen.getByLabelText("the uninstall command") as HTMLInputElement).value).toBe(uninstallCommand("mac"));
    await click(within(bar()).getByRole("button", { name: "Leave, keep the token" }));
    expect(h.revoke).not.toHaveBeenCalled();
    expect(screen.getByRole("heading", { name: "Machines" })).toBeTruthy();
  });

  it("revokes the minted identity and leaves on success", async () => {
    mount(fakeConnection());
    await describeAndMint();
    await click(within(bar()).getByRole("button", { name: "Cancel" }));
    await click(within(bar()).getByRole("button", { name: "Revoke the token and leave" }));
    await settle();
    expect(h.revoke).toHaveBeenCalledTimes(1);
    expect(h.revoke.mock.calls[0]?.[1]).toBe(IDENTITY);
    expect(screen.getByRole("heading", { name: "Machines" })).toBeTruthy();
  });

  it("stays, with the refusal, when the revoke is refused -- and still offers Keep", async () => {
    h.revoke.mockResolvedValue({ success: false, errorCode: "not_found", errorMessage: "identity not found" });
    mount(fakeConnection());
    await describeAndMint();
    await click(within(bar()).getByRole("button", { name: "Cancel" }));
    await click(within(bar()).getByRole("button", { name: "Revoke the token and leave" }));
    await settle();
    expect(screen.getByText(/identity not found/)).toBeTruthy();
    expect(within(bar()).getByRole("button", { name: "Leave, keep the token" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "Add a machine" })).toBeTruthy();
  });

  it("goes back to waiting on Keep waiting, and the Head's arrow asks the same question", async () => {
    mount(fakeConnection());
    await describeAndMint();
    await click(screen.getByRole("button", { name: "Back to Machines" }));
    expect(within(bar()).getByText("Leave?")).toBeTruthy();
    await click(within(bar()).getByRole("button", { name: "Keep waiting" }));
    expect(within(bar()).getByText("Waiting for studio-mac-mini")).toBeTruthy();
  });

  it("drops Cancel once the machine has connected", async () => {
    const connection = fakeConnection();
    mount(connection);
    await describeAndMint();
    emit(connection, arrival());
    await settle();
    expect(within(bar()).queryByRole("button", { name: "Cancel" })).toBeNull();
    await click(within(bar()).getByRole("button", { name: "Leave setup" }));
    expect(screen.getByRole("heading", { name: "Machines" })).toBeTruthy();
  });
});

describe("the flow survives the window's own navigation", () => {
  it("keeps the token on screen across Machines -> Routing -> Machines", async () => {
    const view = mount(fakeConnection());
    await describeAndMint();
    rerenderAt(view, "routing");
    expect(screen.getByLabelText("the worker token").closest("[hidden]")).not.toBeNull();
    rerenderAt(view, "machines");
    await settle();
    expect((screen.getByLabelText("the worker token") as HTMLInputElement).value).toBe(TOKEN);
    expect(within(bar()).getByText("Waiting for studio-mac-mini")).toBeTruthy();
  });
});

describe("the install and uninstall lines", () => {
  it("renders a placeholder rather than half a URL when no domain is published", () => {
    expect(workerClusterUrl("")).toBe("");
    expect(
      installCommand({ platform: "linux", clusterUrl: "", token: "tok", computerUse: false, inference: false }),
    ).toContain("<your cluster URL>");
  });

  it("strips a scheme and trailing slashes off the configured domain", () => {
    expect(workerClusterUrl("https://memql.example.com/")).toBe("https://api.memql.example.com");
  });

  it("is ONE physical line, whatever the inputs", () => {
    for (const platform of INSTALL_PLATFORMS) {
      for (const computerUse of [false, true]) {
        for (const inference of [false, true]) {
          for (const clusterUrl of ["https://api.example.com", ""]) {
            const command = installCommand({ platform, clusterUrl, token: TOKEN, computerUse, inference });
            expect(command).not.toContain("\n");
            expect(command).not.toContain("\\");
          }
        }
      }
    }
  });

  it("pins the exact composed shape", () => {
    expect(
      installCommand({ platform: "mac", clusterUrl: "https://api.example.com", token: TOKEN, computerUse: true, inference: false }),
    ).toBe(
      "curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install/install-mac.sh" +
        ` | bash -s -- --token ${TOKEN} --cluster https://api.example.com --computeruse`,
    );
    expect(
      installCommand({ platform: "linux", clusterUrl: "https://api.example.com", token: TOKEN, computerUse: true, inference: true }),
    ).toBe(
      "curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install/install-linux.sh" +
        ` | bash -s -- --token ${TOKEN} --cluster https://api.example.com --computeruse --inference`,
    );
  });

  it("composes the uninstall line the same way, with its two flags", () => {
    expect(uninstallCommand("linux")).toBe(
      "curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install/uninstall-linux.sh | bash -s --",
    );
    expect(uninstallCommand("mac", { purge: true, userLocal: true })).toBe(
      "curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install/uninstall-mac.sh | bash -s -- --purge --user-local",
    );
    for (const platform of INSTALL_PLATFORMS) {
      expect(uninstallCommand(platform, { purge: true })).not.toContain("\n");
    }
  });
});
