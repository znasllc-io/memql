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
const query = (reply: (args: {
    accountId: string;
    action: string;
    sessionId?: string;
    options?: Record<string, unknown>;
}) => Record<string, unknown> | Promise<Record<string, unknown>>) => {
    const call = vi.fn(async (args: Parameters<typeof reply>[0]) => rowsResult([await reply(args)]));
    h.connection = { query: { emailAzureSetup: call } };
    return call;
};
const click = async (name: string) => { const button = await screen.findByRole("button", { name }); await act(async () => { fireEvent.click(button); }); };
afterEach(() => { h.connection = null; sessionStorage.clear(); });
describe("organization Azure email setup", () => {
    it("uses Microsoft sign-in without requesting tokens and saves the reviewed plan before any provisioning", async () => {
        let saved = {};
        const call = query(args => {
            if (args.action === "status")
                return { status: "unconfigured" };
            if (args.action === "begin")
                return { status: "waiting", sessionId: "session", userCode: "ABCDEF", verificationUri: "https://microsoft.com/devicelogin", interval: 0.01 };
            if (args.action === "poll")
                return { status: "connected" };
            if (args.action === "directories")
                return { status: "connected", choices: [] };
            if (args.action === "subscriptions")
                return { status: "connected", choices: [{ id: "subscription", name: "Client subscription" }] };
            if (args.action === "resourceGroups")
                return { status: "connected", choices: [{ id: "group-id", name: "mail" }] };
            if (args.action === "locations")
                return { status: "connected", choices: [] };
            if (args.action === "prepare") {
                saved = args.options!;
                return { status: "planned", plan: saved, planId: "reviewed-plan" };
            }
            if (args.action === "provision")
                return { status: "dns", plan: saved, records: [] };
            throw Error(`Unexpected ${args.action}`);
        });
        render(withSession(<AzureEmailConnections />));
        chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Our company");
        await click("Connect email");
        await click("Sign in with Microsoft");
        expect(screen.queryByLabelText("Client secret")).toBeNull();
        expect(screen.queryByLabelText("Access token")).toBeNull();
        await screen.findByRole("combobox", { name: "Subscription" });
        chooseOption(screen.getByRole("combobox", { name: "Subscription" }), "Client subscription");
        await waitFor(() => expect(call.mock.calls.some(([args]) => args.action === "locations")).toBe(true));
        chooseOption(screen.getByRole("combobox", { name: "Resource group" }), "mail");
        fireEvent.change(screen.getByLabelText("Email domain"), { target: { value: "client.example" } });
        await click("Review resources");
        expect(call.mock.calls.filter(([args]) => args.action === "provision")).toHaveLength(0);
        expect(screen.getByText(/Azure email usage is billed/)).toBeTruthy();
        await click("Create Azure resources");
        expect(call.mock.calls.find(([args]) => args.action === "provision")?.[0]).toMatchObject({ accountId: "self", sessionId: "session", options: { confirmed: true, planId: "reviewed-plan" } });
        expect(await screen.findByText(/Merge SPF/)).toBeTruthy();
    });
    it("does not show a previous organization's late connection reply", async () => {
        let resolve: (value: Record<string, unknown>) => void = () => { };
        query(args => args.accountId === "self" ? new Promise(done => { resolve = done; }) : { status: "ready", sender: "news@client.example", plan });
        render(withSession(<AzureEmailConnections />));
        chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Our company");
        await waitFor(() => expect(screen.getByRole("combobox", { name: "Organization" })).toBeTruthy());
        chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Client");
        expect(await screen.findByText("news@client.example")).toBeTruthy();
        await act(async () => resolve({ status: "ready", sender: "owner@our-company.example", plan }));
        expect(screen.queryByText("owner@our-company.example")).toBeNull();
    });
    it("resumes DNS verification after signing in again and keeps the saved resource plan", async () => {
        const call = query(args => {
            if (args.action === "status")
                return { status: "dns", plan };
            if (args.action === "begin")
                return { status: "waiting", sessionId: "resumed", interval: 0.01 };
            if (args.action === "poll")
                return { status: "connected" };
            if (args.action === "directories")
                return { status: "connected", choices: [] };
            if (args.action === "subscriptions")
                return { status: "connected", choices: [] };
            if (args.action === "domainStatus")
                return { status: "dns", plan, records: [{ purpose: "Domain", name: "client.example", type: "TXT", value: "verification-proof", status: "NotStarted" }] };
            throw Error(`Unexpected ${args.action}`);
        });
        render(withSession(<AzureEmailConnections />));
        chooseOption(screen.getByRole("combobox", { name: "Organization" }), "Our company");
        await screen.findByText("Setup is saved. Continue when you're ready.");
        await click("Connect email");
        await click("Sign in with Microsoft");
        expect(await screen.findByText("verification-proof")).toBeTruthy();
        expect(call.mock.calls.filter(([args]) => args.action === "prepare" || args.action === "provision")).toHaveLength(0);
    });
    it("keeps connection configuration with owners and developers", async () => {
        const call = query(() => ({ status: "ready" }));
        render(withSession(<AzureEmailConnections />, { role: "writer" }));
        expect(screen.getByText(/An owner or developer/)).toBeTruthy();
        expect(call).not.toHaveBeenCalled();
    });
});
