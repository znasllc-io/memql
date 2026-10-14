import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { readTheBar, useFakeDate } from "../readTheBar";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { DomainSetup } from "../../src/apps/accounts/DomainSetup";
import { accountFromRow } from "../../src/apps/accounts/rows";
import { fakeConnection, withSession } from "./harness";
afterEach(cleanup);
describe("account domain setup", () => {
  useFakeDate();
  it("reviews normalized domains and writes once, without enabling access", async () => {
    const connection = fakeConnection(); h.connection = connection;
    const onSaved = vi.fn();
    render(withSession(<DomainSetup account={accountFromRow({ id: "acme", name: "Acme", status: "active" })} onBack={vi.fn()} onSaved={onSaved} />));
    fireEvent.change(screen.getByLabelText("Domain", { selector: "input" }), { target: { value: " EXAMPLE.com. " } });
    for (let i = 0; i < 3; i++) { await readTheBar(); fireEvent.click(screen.getByRole("button", { name: "Next" })); }
    expect(connection.query.configureAccountDomains).not.toHaveBeenCalled();
    expect(screen.getAllByText("example.com").length).toBeGreaterThan(0);
    await readTheBar(); fireEvent.click(screen.getByRole("button", { name: "Save defaults" }));
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(connection.query.configureAccountDomains).toHaveBeenCalledExactlyOnceWith({ accountId: "acme", domains: ["example.com"], defaultDomain: "example.com", defaultSenderIdentityId: "" });
  });
  it("keeps the wizard and draft when the server refuses the write", async () => {
    const connection = fakeConnection(); h.connection = connection;
    connection.query.configureAccountDomains.mockRejectedValue(new Error("account_domain_default: changed elsewhere"));
    const onSaved = vi.fn();
    render(withSession(<DomainSetup account={accountFromRow({ id: "acme", name: "Acme", domain: "example.com", status: "active" })} onBack={vi.fn()} onSaved={onSaved} />));
    for (let i = 0; i < 3; i++) { await readTheBar(); fireEvent.click(screen.getByRole("button", { name: "Next" })); }
    await readTheBar(); fireEvent.click(screen.getByRole("button", { name: "Save defaults" }));
    expect(await screen.findByText("account_domain_default: changed elsewhere")).toBeTruthy();
    expect(onSaved).not.toHaveBeenCalled();
    expect(screen.getAllByText("example.com").length).toBeGreaterThan(0);
  });
});
