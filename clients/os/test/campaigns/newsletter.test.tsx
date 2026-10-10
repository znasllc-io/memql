import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { chooseOption, openSelect } from "../selectControl";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { NewsletterPanel, newsletterFormHTML } from "../../src/apps/campaigns/NewsletterPanel";
import { fakeConnection, withSession } from "./harness";
import type { LiveCollectionHandle } from "../../src/live/useLiveCollection";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

const rows = (data: Row[]): LiveCollectionHandle<Row> => ({ source: null, snapshot: { rows: data, state: "live", error: "", version: 1 }, reseed: () => {} });
const resources = { templates: rows([{ id: "t1", accountId: "client", name: "Welcome", status: "ready" }, { id: "t2", accountId: "other", name: "Other client's welcome", status: "ready" }]), senders: rows([{ id: "s1", accountId: "client", address: "hello@client.test", status: "active" }]) };
const binding = { id: "n1", siteId: "site", accountId: "client", audienceId: "audience", templateId: "t1", senderIdentityId: "s1", enabled: true, consentText: "Email me the client newsletter.", createdAt: "2026-10-01T12:00:00Z" };
const site = { id: "site", accountId: "client", title: "Client storefront", status: "live" };

describe("newsletter connection", () => {
  it("offers the audience organization's resources in the opened setup", async () => {
    const conn = fakeConnection({ sites: [site, { id: "foreign", accountId: "other", title: "Other storefront", status: "live" }] }); h.connection = conn;
    render(withSession(<NewsletterPanel audienceId="audience" accountId="client" resources={resources} />));
    const add = await screen.findByRole("button", { name: "Connect a signup form" });
    await waitFor(() => expect(add.hasAttribute("disabled")).toBe(false));
    fireEvent.click(add);
    expect(within(openSelect(screen.getByRole("combobox", { name: "Newsletter deployable" }))).queryByRole("option", { name: "Other storefront" })).toBeNull();
    chooseOption(screen.getByRole("combobox", { name: "Newsletter deployable" }), "Client storefront");
    expect(within(openSelect(screen.getByRole("combobox", { name: "Welcome template" }))).queryByRole("option", { name: "Other client's welcome" })).toBeNull();
    chooseOption(screen.getByRole("combobox", { name: "Welcome template" }), "Welcome");
    chooseOption(screen.getByLabelText("Welcome sender"), "hello@client.test");
    fireEvent.change(screen.getByLabelText("Signup consent sentence"), { target: { value: binding.consentText } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(conn.query.campaignConfigureNewsletter).toHaveBeenCalledWith({ siteId: "site", audienceId: "audience", templateId: "t1", senderIdentityId: "s1", consentText: binding.consentText, enabled: true, expectedRevision: undefined }));
  });
  it("uses the saved revision to disable a connection and recheck a blocked welcome", async () => {
    const welcome = { id: "signup", status: "blocked", requestedAt: "2026-10-01T12:01:00Z", createdAt: "2026-10-01T12:02:00Z", lastError: "Sender needs attention." };
    const conn = fakeConnection({ sites: [site], newsletters: [binding], welcomes: [welcome] }); h.connection = conn;
    render(withSession(<NewsletterPanel audienceId="audience" accountId="client" resources={resources} />));
    fireEvent.click(await screen.findByRole("button", { name: /Client storefront/ }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Accept signups and send a welcome" }));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(conn.query.campaignConfigureNewsletter).toHaveBeenCalledWith(expect.objectContaining({ enabled: false, expectedRevision: binding.createdAt })));
    fireEvent.click(await screen.findByRole("button", { name: "Check again" }));
    await waitFor(() => expect(conn.query.campaignRetryNewsletterWelcome).toHaveBeenCalledWith({ signupId: "signup", expectedRevision: welcome.createdAt }));
  });
  it("copies an escaped affirmative-consent form without exposing organization or sender choices", () => {
    const form = newsletterFormHTML({ ...binding, consentText: '<img src=x onerror="bad()"> & newsletter' });
    expect(form).toContain('action="/_memql/forms/campaigns/subscribe"');
    expect(form).toContain('name="consent" value="yes" required');
    expect(form).toContain(`name="consentRevision" value="${binding.createdAt}"`);
    expect(form).toContain("&lt;img");
    expect(form).not.toContain("<img");
    expect(form).not.toContain("senderIdentityId");
    expect(form).not.toContain("accountId");
  });
});
