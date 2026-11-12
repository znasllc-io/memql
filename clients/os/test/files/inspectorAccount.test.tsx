import { screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { chooseOption } from "../selectControl";

// A file names ONE account (account row ownership, section G): the inspector
// uses the single picker and sends a one-element `accountIds` list, or an
// empty list for none, because the index still declares the list.
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { artifactRow, click, fakeConnection, renderFiles } from "./harness";

beforeEach(() => {
  h.connection = null;
});

const ACCOUNTS = [
  { id: "acme", name: "Acme Consulting", status: "active", createdAt: "2026-08-01T00:00:00Z" },
  { id: "globex", name: "Globex", status: "active", createdAt: "2026-08-02T00:00:00Z" },
];

async function openInspector(conn: ReturnType<typeof fakeConnection>) {
  h.connection = conn;
  await renderFiles();
  await click(screen.getByRole("button", { name: /brief\.pdf/ }));
  return screen.getByRole("region", { name: "File details" });
}

describe("the inspector's account", () => {
  it("is a single choice, sent as a one-element list", async () => {
    const conn = fakeConnection({ artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })], accounts: ACCOUNTS });
    const inspector = await openInspector(conn);
    const picker = within(inspector).getByLabelText("The client this file is about");
    // The choices arrive from their own read; wait for them before choosing.
    await waitFor(() => chooseOption(picker, "Globex"));
    await waitFor(() =>
      expect(conn.callsNamed("setArtifactAccounts")).toEqual([
        'mutation setArtifactAccounts(artifactId: "a-1", accountIds: ["globex"])',
      ]),
    );
  });

  it("sends an empty list to clear the account", async () => {
    const conn = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "brief.pdf", accountIds: ["acme"] })],
      accounts: ACCOUNTS,
    });
    const inspector = await openInspector(conn);
    const picker = () => within(inspector).getByLabelText("The client this file is about");
    await waitFor(() => expect(picker().textContent).toContain("Acme Consulting"));
    chooseOption(picker(), "No client");
    await waitFor(() =>
      expect(conn.callsNamed("setArtifactAccounts")).toEqual([
        'mutation setArtifactAccounts(artifactId: "a-1", accountIds: [])',
      ]),
    );
  });
});
