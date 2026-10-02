import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { chooseOption } from "../selectControl";
import { rowsResult, withSession } from "../campaigns/harness";
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
  h.connection = { query: { emailAzureSetup: call } };
  return call;
};
const click = async (name: string) => { const button = await screen.findByRole("button", { name }); await act(async () => { fireEvent.click(button); }); };
afterEach(() => { h.connection = null; sessionStorage.clear(); });

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
  it("keeps cluster connection controls out of Campaigns", async () => {
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
  it("shows organization processing receipts without claiming recipient delivery", async () => {
    const call = query(args => args.action === "clusterStatus" ? cluster : args.action === "operations" ? { status: "ok", operations: [{ operationId: "receipt", status: "succeeded", detail: "", submittedAt: "2026-10-01T12:00:00Z" }] } : { status: "ready", sender: "news@client.example", plan });
    render(withSession(<AzureEmailConnections/>));
    chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Client");
    await click("Manage email for Client"); await click("Check sends");
    expect(await screen.findByText("Processing completed")).toBeTruthy();
    expect(screen.getByText(/does not confirm inbox delivery/)).toBeTruthy();
    expect(call.mock.calls.find(([args]) => args.action === "operations")?.[0].accountId).toBe("client");
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
    expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
  });
  it("keeps configuration with owners and developers", async () => {
    const call = query(() => cluster); render(withSession(<AzureEmailConnections/>, { role: "writer" }));
    expect(screen.getByText(/An owner or developer/)).toBeTruthy(); expect(call).not.toHaveBeenCalled();
  });
});
