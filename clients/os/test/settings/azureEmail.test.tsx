import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { readTheBar } from "../readTheBar";
import { chooseOption } from "../selectControl";
import { rowsResult, withSession } from "../email/harness";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("../../src/apps/accounts/tie", () => ({ useAccountOptions: () => [
  { id: "self", name: "Our company", status: "active" }, { id: "client", name: "Client", status: "active" },
] }));
import { AzureEmailConnections } from "../../src/modules/connections/AzureEmailConnections";
const plan = { subscriptionId: "subscription", resourceGroup: "mail", emailService: "client-email", communicationService: "client-delivery", domain: "client.example", dataLocation: "United States" };
const cluster = { status: "connected", subscriptionId: "subscription", resourceGroup: "mail", dataLocation: "United States" };
type Args = { accountId: string; action: string; sessionId?: string; options?: Record<string, unknown> };
const query = (reply: (args: Args) => Record<string, unknown> | Promise<Record<string, unknown>>) => {
  const call = vi.fn(async (args: Args) => rowsResult([await reply(args)]));
  h.connection = { query: { emailAzureSetup: call, packageCampaigns: vi.fn(async () => rowsResult([{status:"installed",manifest:null}])) } };
  return call;
};
/** Clicks a button; one of the bar's acts is clicked once the bar has been read, since a bar that has just changed takes no press for a moment. */
const click = async (name: string) => {
  const find = () => (vi.isFakeTimers() ? screen.getByRole("button", { name }) : screen.findByRole("button", { name }));
  if ((await find()).closest('[aria-label="What you can do with this"]') !== null) await readTheBar();
  const button = await find();
  await act(async () => { fireEvent.click(button); });
};
afterEach(() => { h.connection = null; sessionStorage.clear(); vi.useRealTimers(); });

describe("one Azure configuration per cluster", () => {
  it("connects Microsoft once before saving the cluster's subscription and resource group", async () => {
    const call = query(args => {
      if (args.action === "clusterStatus") return { status: "unconfigured", applicationReady: true, capture: true };
      if (args.action === "begin") return { status: "waiting", sessionId: "session", userCode: "ABCDEF", verificationUri: "https://microsoft.com/devicelogin", interval: 0.01 };
      if (args.action === "poll") return { status: "connected" };
      if (args.action === "subscriptions") return { status: "connected", choices: [{ id: "subscription", name: "Our subscription" }] };
      if (args.action === "directories" || args.action === "locations") return { status: "connected", choices: [] };
      if (args.action === "resourceGroups") return { status: "connected", choices: [{ id: "group", name: "mail" }] };
      if (args.action === "saveCluster") return cluster;
      if (args.action === "status") return { status: "unconfigured" };
      throw Error(`Unexpected ${args.action}`);
    });
    render(withSession(<AzureEmailConnections/>));
    await click("Set up Azure");
    await click("Sign in with Microsoft");
    expect(screen.queryByLabelText("Client secret")).toBeNull();
    expect(screen.queryByLabelText("Email domain")).toBeNull();
    await screen.findByRole("combobox", { name: "Subscription" });
    chooseOption(screen.getByRole("combobox", { name: "Subscription" }), "Our subscription");
    await waitFor(() => expect(call.mock.calls.some(([args]) => args.action === "locations")).toBe(true));
    chooseOption(screen.getByRole("combobox", { name: "Resource group" }), "mail");
    await click("Save configuration");
    expect(await screen.findByRole("button", { name: "Manage configuration" })).toBeTruthy();
    expect(call.mock.calls.find(([args]) => args.action === "saveCluster")?.[0]).toMatchObject({ accountId: "self", sessionId: "session", options: { subscriptionId: "subscription", resourceGroup: "mail" } });
    expect(call.mock.calls.some(([args]) => args.action === "provision")).toBe(false);
  });
  it("keeps cluster connection controls out of Email", async () => {
    const call = query(args => args.action === "clusterStatus" ? { status: "unconfigured", capture: true } : { status: "unconfigured" });
    render(withSession(<AzureEmailConnections manageCluster={false}/>));
    expect(await screen.findByText(/Configure Azure once in Settings/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Set up Azure" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Sign in with Microsoft" })).toBeNull();
    expect(call.mock.calls.some(([args]) => args.action === "begin")).toBe(false);
  });
  it("adds a client's domain using saved cluster settings without another Microsoft sign-in", async () => {
    const call = query(args => {
      if (args.action === "clusterStatus") return cluster;
      if (args.action === "status") return { status: "unconfigured" };
      if (args.action === "prepare") return { status: "planned", plan, planId: "reviewed" };
      if (args.action === "provision") return { status: "dns", plan, records: [] };
      throw Error(`Unexpected ${args.action}`);
    });
    render(withSession(<AzureEmailConnections/>));
    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Client");
    await click("Add sending domain");
    expect(screen.queryByRole("button", { name: "Sign in with Microsoft" })).toBeNull();
    expect(screen.queryByRole("combobox", { name: "Subscription" })).toBeNull();
    fireEvent.change(screen.getByLabelText("Email domain"), { target: { value: "client.example" } });
    await click("Review domain");
    expect(call.mock.calls.some(([args]) => args.action === "provision")).toBe(false);
    await click("Create Azure resources");
    expect(await screen.findByText(/Merge SPF/)).toBeTruthy();
    expect(call.mock.calls.find(([args]) => args.action === "prepare")?.[0]).toMatchObject({ accountId: "client", options: { domain: "client.example" } });
    expect(call.mock.calls.find(([args]) => args.action === "prepare")?.[0].options).not.toHaveProperty("subscriptionId");
    expect(call.mock.calls.find(([args]) => args.action === "provision")?.[0]).toMatchObject({ accountId: "client", sessionId: "", options: { confirmed: true, planId: "reviewed" } });
    expect(call.mock.calls.some(([args]) => args.action === "begin" || args.action === "poll")).toBe(false);
  });
  it("has one cluster configuration, with an explicit disconnect affecting all organizations", async () => {
    const call = query(args => args.action === "disconnectCluster" ? { status: "disconnected" } : args.action === "clusterStatus" ? cluster : { status: "unconfigured" });
    render(withSession(<AzureEmailConnections/>));
    await click("Manage configuration");
    await click("Disconnect");
    expect(screen.getByText(/every organization/)).toBeTruthy();
    expect(call.mock.calls.some(([args]) => args.action === "disconnectCluster")).toBe(false);
    await click("Disconnect Azure");
    expect(await screen.findByRole("button", { name: "Set up Azure" })).toBeTruthy();
    expect(call.mock.calls.find(([args]) => args.action === "disconnectCluster")?.[0]).toMatchObject({ accountId: "self", options: { confirmed: true } });
  });
  it("continues asynchronous Azure creation automatically only after the reviewed plan is approved", async () => {
    let checks = 0;
    const call = query(args => {
      if (args.action === "clusterStatus") return cluster;
      if (args.action === "status") return { status: "planned", plan, planId: "reviewed" };
      if (args.action === "provision") return ++checks < 3 ? { status: "provisioning", plan, planId: "reviewed" } : { status: "dns", plan, records: [] };
      throw Error(`Unexpected ${args.action}`);
    });
    render(withSession(<AzureEmailConnections/>));
    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Our company");
    await screen.findByRole("button", { name: "Manage email for Our company" });
    vi.useFakeTimers();
    await click("Manage email for Our company");
    await act(async () => { await vi.advanceTimersByTimeAsync(20_000); });
    expect(checks).toBe(0);
    await click("Create Azure resources");
    expect(screen.getByText("Preparing email")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Check provisioning" })).toBeNull();
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(checks).toBe(2);
    expect(screen.getByText("Preparing email")).toBeTruthy();
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(screen.getByText(/Merge SPF/)).toBeTruthy();
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(checks).toBe(3);
    for (const [args] of call.mock.calls.filter(([args]) => args.action === "provision")) expect(args).toMatchObject({ accountId: "self", options: { confirmed: true, planId: "reviewed" } });
  });
  it("resumes approved setup, stops on leaving, and never overlaps a slow request", async () => {
    let finish: (value: Record<string, unknown>) => void = () => {};
    let checks = 0;
    query(args => {
      if (args.action === "clusterStatus") return cluster;
      if (args.action === "provision") { checks++; return new Promise(done => { finish = done; }); }
      return { status: "provisioning", plan, planId: "saved" };
    });
    render(withSession(<AzureEmailConnections/>));
    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Our company");
    await screen.findByRole("button", { name: "Manage email for Our company" });
    vi.useFakeTimers();
    await click("Manage email for Our company");
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(checks).toBe(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(checks).toBe(1);
    await click("Leave");
    await act(async () => finish({ status: "provisioning", plan, planId: "saved" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(checks).toBe(1);
    await click("Manage email for Our company");
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(checks).toBe(2);
  });
  it("pauses automatic creation on an Azure error and offers an explicit continuation", async () => {
    let checks = 0;
    query(args => {
      if (args.action === "clusterStatus") return cluster;
      if (args.action === "provision") { if (++checks === 1) throw Error("Azure permission expired"); return { status: "dns", plan, records: [] }; }
      return { status: "provisioning", plan, planId: "saved" };
    });
    render(withSession(<AzureEmailConnections/>));
    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Our company");
    await screen.findByRole("button", { name: "Manage email for Our company" });
    vi.useFakeTimers();
    await click("Manage email for Our company");
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(screen.getByText("Azure permission expired")).toBeTruthy();
    expect(screen.getByText("Needs attention")).toBeTruthy();
    expect(screen.queryByText(/MemQL continues automatically/)).toBeNull();
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(checks).toBe(1);
    await click("Continue setup");
    expect(screen.getByText(/Merge SPF/)).toBeTruthy();
    expect(checks).toBe(2);
  });
  it("bounds automatic checks and preserves the approved plan for retry", async () => {
    let checks = 0;
    query(args => {
      if (args.action === "clusterStatus") return cluster;
      if (args.action === "provision") checks++;
      return { status: "provisioning", plan, planId: "saved" };
    });
    render(withSession(<AzureEmailConnections/>));
    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Our company");
    await screen.findByRole("button", { name: "Manage email for Our company" });
    vi.useFakeTimers();
    await click("Manage email for Our company");
    for (let i = 0; i < 60; i++) await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(checks).toBe(60);
    expect(screen.getByText("Setup paused")).toBeTruthy();
    expect(screen.getByText("Azure is taking longer than expected.")).toBeTruthy();
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(checks).toBe(60);
    await click("Continue setup");
    expect(checks).toBe(61);
    expect(screen.getByText("Preparing email")).toBeTruthy();
  });
  describe("a dropped connection is never the operation's failure", () => {
    const dropped = () => Object.assign(Error("stream closed"), { reason: "transport" });
    const SENTENCE = "The connection to MemQL dropped. Continue setup picks up where it left off.";
    const open = async (reply: (args: Args) => Record<string, unknown> | Promise<Record<string, unknown>>) => {
      const call = query(reply);
      render(withSession(<AzureEmailConnections/>));
      chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Our company");
      await screen.findByRole("button", { name: "Manage email for Our company" });
      vi.useFakeTimers();
      await click("Manage email for Our company");
      return call;
    };
    it("retries preparing by itself after one interruption and reaches Verify domain", async () => {
      let checks = 0;
      await open(args => {
        if (args.action === "clusterStatus") return cluster;
        if (args.action === "provision") { if (++checks === 1) throw dropped(); return { status: "dns", plan, records: [] }; }
        return { status: "provisioning", plan, planId: "saved" };
      });
      await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
      expect(screen.queryByText(/stream closed/)).toBeNull();
      expect(screen.queryByText("Needs attention")).toBeNull();
      await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
      expect(checks).toBe(2);
      expect(screen.getByText(/Merge SPF/)).toBeTruthy();
      expect(screen.queryByText(/stream closed/)).toBeNull();
    });
    it("says the connection dropped, in plain words, when interruptions persist", async () => {
      let checks = 0;
      await open(args => {
        if (args.action === "clusterStatus") return cluster;
        if (args.action === "provision") { checks++; throw dropped(); }
        return { status: "provisioning", plan, planId: "saved" };
      });
      await act(async () => { await vi.advanceTimersByTimeAsync(10_000 + 130_000); });
      expect(checks).toBeGreaterThan(4);
      expect(screen.queryByText(/stream closed/)).toBeNull();
      expect(screen.getByText(SENTENCE)).toBeTruthy();
      expect(screen.getByRole("button", { name: "Continue setup" })).toBeTruthy();
    });
    it("still shows an Azure refusal as it is", async () => {
      await open(args => {
        if (args.action === "clusterStatus") return cluster;
        if (args.action === "provision") throw Error("Azure permission expired");
        return { status: "provisioning", plan, planId: "saved" };
      });
      await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
      expect(screen.getByText("Azure permission expired")).toBeTruthy();
      expect(screen.queryByText(SENTENCE)).toBeNull();
    });
    it("retries Check domain records after an interruption", async () => {
      let checks = 0;
      await open(args => {
        if (args.action === "clusterStatus") return cluster;
        if (args.action === "verify") { if (++checks === 1) throw dropped(); return { status: "verified", plan }; }
        return { status: "dns", plan, planId: "saved", records: [] };
      });
      await click("Check domain records");
      expect(screen.queryByText(/stream closed/)).toBeNull();
      await act(async () => { await vi.advanceTimersByTimeAsync(1_100); });
      expect(checks).toBe(2);
      expect(screen.queryByText(/stream closed/)).toBeNull();
      expect(screen.queryByText("Needs attention")).toBeNull();
    });
    it("gives a plain sentence when Check domain records keeps dropping", async () => {
      await open(args => {
        if (args.action === "clusterStatus") return cluster;
        if (args.action === "verify") throw dropped();
        return { status: "dns", plan, planId: "saved", records: [] };
      });
      await click("Check domain records");
      await act(async () => { await vi.advanceTimersByTimeAsync(130_000); });
      expect(screen.queryByText(/stream closed/)).toBeNull();
      expect(screen.getByText(/The connection to MemQL dropped\./)).toBeTruthy();
      expect(screen.getByRole("button", { name: "Check domain records" })).toBeTruthy();
    });
    it("stops retrying when the page is closed", async () => {
      let checks = 0;
      await open(args => {
        if (args.action === "clusterStatus") return cluster;
        if (args.action === "verify") { checks++; throw dropped(); }
        return { status: "dns", plan, planId: "saved", records: [] };
      });
      await click("Check domain records");
      await act(async () => { await vi.advanceTimersByTimeAsync(1_100); });
      expect(checks).toBe(2);
      cleanup();
      await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
      expect(checks).toBe(2);
    });
    it("stops retrying on Leave, and the sending domain can be opened again", async () => {
      let checks = 0;
      await open(args => {
        if (args.action === "clusterStatus") return cluster;
        if (args.action === "provision") { checks++; throw dropped(); }
        return { status: "planned", plan, planId: "saved" };
      });
      await click("Create Azure resources");
      await act(async () => { await vi.advanceTimersByTimeAsync(1_100); });
      expect(checks).toBe(2);
      await click("Leave");
      await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
      expect(checks).toBe(2);
      await click("Manage email for Our company");
      await click("Create Azure resources");
      expect(checks).toBe(3);
    });
    it("retries saving the sender after a drop and saves it once", async () => {
      let tries = 0, saved = 0;
      await open(args => {
        if (args.action === "clusterStatus") return cluster;
        if (args.action === "sender") { if (++tries === 1) throw dropped(); saved++; return { status: "ready", plan, sender: "news@client.example", senders: { "news@client.example": "News" } }; }
        return { status: "verified", plan, planId: "saved", records: [] };
      });
      await click("Save sender");
      expect(screen.queryByText(/stream closed/)).toBeNull();
      await act(async () => { await vi.advanceTimersByTimeAsync(1_100); });
      expect(tries).toBe(2);
      expect(saved).toBe(1);
      expect(screen.queryByText(/stream closed/)).toBeNull();
      expect(screen.queryByText("Needs attention")).toBeNull();
    });
    it("counts the time a request itself takes toward the two minutes", async () => {
      let asks = 0;
      const slow = Object.assign(Error("stream closed"), { reason: "transport" });
      await open(args => {
        if (args.action === "clusterStatus") return cluster;
        if (args.action === "verify") { asks++; return new Promise<Record<string, unknown>>((_, no) => { setTimeout(() => no(slow), 50_000); }); }
        return { status: "dns", plan, planId: "saved", records: [] };
      });
      await click("Check domain records");
      await act(async () => { await vi.advanceTimersByTimeAsync(400_000); });
      expect(asks).toBe(3);
      expect(screen.queryByText(/stream closed/)).toBeNull();
      expect(screen.getByText(/The connection to MemQL dropped\./)).toBeTruthy();
    });
    it("shows a validation sentence from Check domain records unchanged", async () => {
      await open(args => {
        if (args.action === "clusterStatus") return cluster;
        if (args.action === "verify") throw Error("Domain is not yours");
        return { status: "dns", plan, planId: "saved", records: [] };
      });
      await click("Check domain records");
      expect(screen.getByText("Domain is not yours")).toBeTruthy();
    });
  });
  it("explains a missing publisher registration before offering sign-in", async () => {
    const call = query(() => ({ status: "unconfigured", applicationReady: false }));
    render(withSession(<AzureEmailConnections/>));
    await click("Set up Azure");
    expect(await screen.findByText("Microsoft sign-in needs a one-time MemQL app registration.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Sign in with Microsoft" })).toBeNull();
    expect(call.mock.calls.some(([args]) => args.action === "begin")).toBe(false);
  });
  it("resumes a client's DNS verification without a browser sign-in session", async () => {
    const call = query(args => args.action === "clusterStatus" ? cluster : args.action === "domainStatus" ? { status: "dns", plan, records: [{ purpose: "Domain", name: "client.example", type: "TXT", value: "verification-proof", status: "NotStarted" }] } : { status: "dns", plan, planId: "saved-plan" });
    render(withSession(<AzureEmailConnections/>));
    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Client");
    await click("Manage email for Client");
    expect(await screen.findByText("verification-proof")).toBeTruthy();
    expect(call.mock.calls.some(([args]) => ["begin", "poll", "prepare", "provision"].includes(args.action))).toBe(false);
  });
  it("spf caption names -all and records re-check while open", async () => {
    const records = [
      { purpose: "Domain", name: "client.example", type: "TXT", value: "ms-domain-verification=proof", status: "Verified" },
      { purpose: "SPF", name: "client.example", type: "TXT", value: "v=spf1 include:spf.protection.outlook.com -all", status: "VerificationInProgress" },
    ];
    const call = query(args => args.action === "clusterStatus" ? cluster : args.action === "domainStatus" ? { status: "dns", plan, records } : { status: "dns", plan, planId: "saved", records });
    render(withSession(<AzureEmailConnections/>));
    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Client");
    await screen.findByRole("button", { name: "Manage email for Client" });
    vi.useFakeTimers();
    await click("Manage email for Client");
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(screen.getByText(/one SPF record/)).toBeTruthy();
    expect(screen.getByText(/-all/, { selector: ".os-caption" })).toBeTruthy();
    expect(screen.getByText(/does not verify ~all/)).toBeTruthy();
    expect(screen.getByText(/Last checked/)).toBeTruthy();
    const checks = () => call.mock.calls.filter(([args]) => args.action === "domainStatus").length;
    const before = checks();
    // A timed re-check is quiet: while it is in flight the bar is not busy and nothing is disabled.
    const answer = vi.fn(); let release: () => void = () => {};
    call.mockImplementationOnce(async (args) => { answer(args.action); await new Promise<void>(done => { release = done; }); return rowsResult([{ status: "dns", plan, records }]); });
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(answer).toHaveBeenCalledWith("domainStatus");
    expect(screen.queryByText("Working")).toBeNull();
    expect(document.querySelector("button:disabled")).toBeNull();
    expect(screen.getByRole("button", { name: "Check domain records" })).toBeTruthy();
    await act(async () => { release(); });
    expect(checks()).toBe(before + 1);
    // A press while a timed re-check is in flight ends it: the two never run together.
    let quietSignal: AbortSignal | undefined; let releaseQuiet: () => void = () => {};
    call.mockImplementationOnce((async (_args: Args, opts?: { signal?: AbortSignal }) => { quietSignal = opts?.signal; await new Promise<void>(done => { releaseQuiet = done; }); return rowsResult([{ status: "dns", plan, records }]); }) as never);
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(quietSignal?.aborted).toBe(false);
    await click("Check domain records");
    expect(quietSignal?.aborted).toBe(true);
    await act(async () => { releaseQuiet(); });
    expect(screen.queryByText(/Still checking/)).toBeNull();
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000 * 10); });
    expect(checks()).toBeGreaterThan(before + 5);
    expect(screen.getByText("Still checking: make sure there is one SPF record and it ends in -all.")).toBeTruthy();
  });
  it("uses the Deployables record layout with exact copy values and readable verification states", async () => {
    const records = [
      { purpose: "Domain", name: "client.example", type: "TXT", value: "ms-domain-verification=proof", status: "Verified" },
      { purpose: "SPF", name: "client.example", type: "TXT", value: "v=spf1 include:spf.protection.outlook.com -all", status: "VerificationInProgress" },
      { purpose: "DKIM", name: "selector1._domainkey", type: "CNAME", value: "selector1.azurecomm.net", status: "NotStarted" },
      { purpose: "DKIM2", name: "selector2._domainkey", type: "CNAME", value: "selector2.azurecomm.net", status: "VerificationFailed" },
    ];
    query(args => args.action === "clusterStatus" ? cluster : { status: "dns", plan, planId: "saved", records });
    render(withSession(<AzureEmailConnections/>));
    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Client");
    await click("Manage email for Client");
    expect(screen.queryByRole("table")).toBeNull();
    const writeText = vi.fn().mockResolvedValue(undefined);
    const previous = Object.getOwnPropertyDescriptor(navigator, "clipboard");
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
    try {
      const ownership = screen.getByRole("group", { name: "Domain ownership" });
      expect(within(ownership).getByText("Verified")).toBeTruthy();
      expect(screen.getByText("Checking")).toBeTruthy();
      expect(screen.getByText("Not verified")).toBeTruthy();
      expect(screen.getByText("Check failed")).toBeTruthy();
      for (const record of records) {
        const copyValue = screen.getByRole("button", { name: `Copy value: ${record.value}` });
        await act(async () => { fireEvent.click(copyValue); });
        expect(writeText).toHaveBeenLastCalledWith(record.value);
        expect(copyValue.getAttribute("title")).toBe("Copied");
      }
      await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Copy name: selector1._domainkey" })); });
      expect(writeText).toHaveBeenLastCalledWith("selector1._domainkey");
      expect(screen.queryByRole("button", { name: /Copy type/ })).toBeNull();
    } finally {
      if (previous) Object.defineProperty(navigator, "clipboard", previous);
      else Reflect.deleteProperty(navigator, "clipboard");
    }
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Domain client.example" })); });
    expect(screen.queryByRole("group", { name: "Domain ownership" })).toBeNull();
    expect(screen.queryByLabelText("Email domain")).toBeNull();
    await click("Continue to verification");
    expect(screen.getByRole("group", { name: "Domain ownership" })).toBeTruthy();
  });
  it("shows organization processing receipts without claiming recipient delivery", async () => {
    const call = query(args => args.action === "clusterStatus" ? cluster : args.action === "operations" ? { status: "ok", operations: [{ operationId: "receipt", status: "succeeded", detail: "", submittedAt: "2026-10-01T12:00:00Z" }] } : { status: "ready", sender: "news@client.example", plan });
    render(withSession(<AzureEmailConnections/>));
    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Client");
    await click("Manage email for Client"); await click("Check sends");
    expect(await screen.findByText("Processing completed")).toBeTruthy();
    expect(screen.getByText("Delivery unconfirmed")).toBeTruthy();
    expect(screen.getByText("Unavailable")).toBeTruthy();
    expect(call.mock.calls.find(([args]) => args.action === "operations")?.[0].accountId).toBe("client");
  });
  it.each([
    ["ready", "Connected"], ["pending", "Preparing"], ["not-configured", "Not connected"],
  ])("reads delivery feedback readiness %s with an empty receipt list", async (deliveryFeedback, label) => {
    query(args => args.action === "clusterStatus" ? cluster : args.action === "operations" ? { status: "ok", deliveryFeedback, operations: [] } : { status: "ready", sender: "news@client.example", plan });
    render(withSession(<AzureEmailConnections/>));
    await click("Manage email for Our company"); await click("Check sends");
    const section = screen.getByRole("region", { name: "Azure send processing" });
    expect(within(section).getByText(label)).toBeTruthy();
    expect(within(section).getByText("No Azure sends recorded for this organization.")).toBeTruthy();
    expect(within(section).queryByText("Delivery unconfirmed")).toBeNull();
  });
  it("keeps provider verdicts and their timestamps separate from transport completion", async () => {
    const at = "2026-10-01T12:04:00Z";
    const verdicts = ["delivered", "bounced", "suppressed", "quarantined", "filtered-spam", "failed", "expanded"];
    query(args => args.action === "clusterStatus" ? cluster : args.action === "operations" ? { status: "ok", deliveryFeedback: "ready", operations: verdicts.map((deliveryStatus, i) => ({ operationId: String(i), status: "succeeded", detail: "", submittedAt: "2026-10-01T12:00:00Z", deliveryStatus, deliveryAt: at })) } : { status: "ready", sender: "news@client.example", plan });
    render(withSession(<AzureEmailConnections/>));
    await click("Manage email for Our company"); await click("Check sends");
    const section = screen.getByRole("region", { name: "Azure send processing" });
    expect(within(section).getAllByText("Processing completed")).toHaveLength(7);
    for (const label of ["Confirmed by recipient server", "Delivery bounced", "Delivery suppressed", "Delivery quarantined", "Filtered as spam", "Delivery failed", "Expanded to a distribution list"]) expect(within(section).getByText(label)).toBeTruthy();
    expect(within(section).getAllByText(new Date(at).toLocaleString())).toHaveLength(7);
    expect(within(section).queryByText(/inbox|delivered/i)).toBeNull();
  });
  it("does not confirm delivery from missing, zero or invalid report data", async () => {
    const reports = [{}, { deliveryStatus: "delivered" }, { deliveryStatus: "delivered", deliveryAt: "0001-01-01T00:00:00Z" }, { deliveryStatus: "delivered", deliveryAt: "invalid" }, { deliveryAt: "2026-10-01T12:04:00Z" }];
    query(args => args.action === "clusterStatus" ? cluster : args.action === "operations" ? { status: "ok", deliveryFeedback: "ready", operations: reports.map((report, i) => ({ operationId: String(i), status: "succeeded", detail: "", submittedAt: "2026-10-01T12:00:00Z", ...report })) } : { status: "ready", sender: "news@client.example", plan });
    render(withSession(<AzureEmailConnections/>));
    await click("Manage email for Our company"); await click("Check sends");
    const section = screen.getByRole("region", { name: "Azure send processing" });
    expect(within(section).getAllByText("Delivery unconfirmed")).toHaveLength(5);
    expect(within(section).queryByText("Reported")).toBeNull();
    expect(within(section).queryByText("Confirmed by recipient server")).toBeNull();
  });
  it("refreshes feedback readiness and recipient reports together without resubmitting mail", async () => {
    let reads = 0;
    const call = query(args => args.action === "clusterStatus" ? cluster : args.action === "operations" ? { status: "ok", deliveryFeedback: ++reads === 1 ? "pending" : "ready", operations: [{ operationId: "receipt", status: "succeeded", detail: "", submittedAt: "2026-10-01T12:00:00Z", ...(reads > 1 ? { deliveryStatus: "delivered", deliveryAt: "2026-10-01T12:04:00Z" } : {}) }] } : { status: "ready", sender: "news@client.example", plan });
    render(withSession(<AzureEmailConnections/>));
    await click("Manage email for Our company");
    vi.useFakeTimers();
    await click("Check sends");
    expect(screen.getByText("Preparing")).toBeTruthy();
    expect(screen.getByText("Delivery unconfirmed")).toBeTruthy();
    await act(async () => { await vi.advanceTimersByTimeAsync(15_000); });
    expect(reads).toBe(2);
    const section = screen.getByRole("region", { name: "Azure send processing" });
    expect(within(section).getByText("Connected")).toBeTruthy();
    expect(within(section).getByText("Confirmed by recipient server")).toBeTruthy();
    expect(within(section).queryByText("Delivery unconfirmed")).toBeNull();
    expect(call.mock.calls.every(([args]) => ["clusterStatus", "status", "operations"].includes(args.action))).toBe(true);
  });
  it("keeps Check sends available after an unreadable response", async () => {
    let reads = 0;
    query(args => {
      if (args.action === "clusterStatus") return cluster;
      if (args.action === "operations") {
        if (++reads === 1) throw Error("Send reports could not be read.");
        return { status: "ok", deliveryFeedback: "ready", operations: [] };
      }
      return { status: "ready", sender: "news@client.example", plan };
    });
    render(withSession(<AzureEmailConnections/>));
    await click("Manage email for Our company"); await click("Check sends");
    expect(screen.getByText("Send reports could not be read.")).toBeTruthy();
    expect(screen.queryByText("No Azure sends recorded for this organization.")).toBeNull();
    await click("Check sends");
    expect(screen.getByText("No Azure sends recorded for this organization.")).toBeTruthy();
    expect(screen.queryByText("Send reports could not be read.")).toBeNull();
  });
  it("discards a previous organization's late domain response", async () => {
    let resolve: (value: Record<string, unknown>) => void = () => {};
    query(args => args.action === "clusterStatus" ? cluster : args.accountId === "self" ? new Promise(done => { resolve = done; }) : { status: "ready", sender: "news@client.example", plan });
    render(withSession(<AzureEmailConnections/>));
    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Our company");
    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Client");
    expect(await screen.findByText("news@client.example")).toBeTruthy();
    await act(async () => resolve({ status: "ready", sender: "owner@our-company.example", plan }));
    expect(screen.queryByText("owner@our-company.example")).toBeNull();
  });
  it("keeps Azure configuration unavailable until its status is known", async () => {
    query(() => new Promise(() => {})); render(withSession(<AzureEmailConnections/>));
    expect(screen.queryByRole("button", { name: "Set up Azure" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add sending domain" })).toBeNull();
  });
  it("offers retry when the cluster status is unreadable", async () => {
    query(() => { throw Error("Connection unavailable"); }); render(withSession(<AzureEmailConnections/>));
    expect(await screen.findByText("Cluster email configuration could not be read.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Set up Azure" })).toBeNull();
    expect(within(screen.getByRole("region", { name: "Cluster email" })).getByRole("button", { name: "Try again" })).toBeTruthy();
  });
  it("keeps configuration with owners and developers", async () => {
    const call = query(() => cluster); render(withSession(<AzureEmailConnections/>, { role: "writer" }));
    expect(screen.getByText(/An owner or developer/)).toBeTruthy(); expect(call).not.toHaveBeenCalled();
  });
});
