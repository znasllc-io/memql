import { render, screen, waitFor, within } from "@testing-library/react";
import { act } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { SOURCE_CREDENTIAL_CONCEPT } from "../../src/apps/deployables/sources/rows";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { click, emit, fakeConnection, githubGrantRow, withSession, type FakeConnection, type FakeSeed } from "./harness";

// Settings -> Sources (epic memql#4885, task memql#4891, design section D):
// every credential this person holds, what fetches under it, and the two acts
// -- add, and revoke.
//
// Per-file isolation: this file mounts the app's SETTINGS section, and
// `list.test.tsx` mounts the list. Vitest isolates per FILE rather than per
// test (clients/os/README-adjacent rule), so a suite that mixed the two would
// share module state between them.

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

function mount(connection: FakeConnection | null, role = "owner", userId = "u-me") {
  h.connection = connection;
  return render(
    withSession(
      <DeployablesApp sectionId="settings" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />,
      { role, userId },
    ),
  );
}

async function fill(label: string, value: string): Promise<void> {
  const input = screen.getByLabelText(label) as HTMLInputElement;
  await act(async () => {
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
    setter.call(input, value);
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

function credential(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "u-me",
    host: "github.com",
    label: "acme deploy token",
    fingerprint: "...ab12",
    status: "active",
    lastUsedAt: "",
    revokedAt: "",
    createdAt: "2026-08-20T00:00:00Z",
    ...over,
  } as unknown as Row;
}

const ACME = credential({ id: "cred-acme" });
const OLD = credential({ id: "cred-old", label: "old laptop", fingerprint: "...77aa", status: "revoked", revokedAt: "2026-08-30T00:00:00Z" });

const PACKAGE: Row = {
  id: "pkg-acme",
  ownerUserId: "u-me",
  name: "acme",
  sourceKind: "repo",
  repoUrl: "https://github.com/acme/storefront",
  repoRef: "main",
  credentialId: "cred-acme",
  artifactId: "",
  deployedVersion: "",
  latestKnownVersion: "",
  updateAvailable: false,
  status: "active",
  createdAt: "2026-09-01T10:00:00Z",
} as unknown as Row;

const SEED: FakeSeed = { credentials: [ACME, OLD], packages: [PACKAGE] };

beforeEach(() => {
  h.connection = null;
});

describe("Settings -> Sources", () => {
  it("lists every credential with its host, its fingerprint and what fetches under it", async () => {
    mount(fakeConnection(SEED));
    const group = await screen.findByRole("list", { name: "Your source credentials" });

    // The label and the digest are two things, and read as two: the id in
    // the code face, the name somebody chose in the reading face.
    expect(within(group).getByText("acme deploy token")).toBeTruthy();
    expect(within(group).getByText("...ab12").className).toContain("os-mono");
    expect(within(group).getAllByText("github.com").length).toBeGreaterThan(0);
    // The join: the sources fetching under it, by the label a person reads a
    // source by.
    expect(within(group).getByText("acme/storefront at main")).toBeTruthy();
    // ...and one nothing fetches under says so rather than rendering blank.
    expect(within(group).getByText("nothing fetches under it")).toBeTruthy();
  });

  it("keeps a revoked credential listed, and marks it", async () => {
    mount(fakeConnection(SEED));
    const group = await screen.findByRole("list", { name: "Your source credentials" });

    // The NAME is the name; "revoked" is said once, beside it, in the warn
    // tone (DESIGN.md rule 7) -- and the row's Revoke button is gone, because
    // there is nothing left to revoke.
    expect(within(group).getByText("old laptop")).toBeTruthy();
    expect(within(group).getByText("...77aa")).toBeTruthy();
    expect(within(group).getByText("revoked").getAttribute("data-tone")).toBe("warn");
    // A revoked one is not offered a second revoke.
    expect(within(group).queryByRole("button", { name: /Revoke old laptop/ })).toBeNull();
    expect(within(group).getByRole("button", { name: /Revoke acme deploy token/ })).toBeTruthy();
  });

  it("says what a revoke does to the sources under it, in the surface, before it does it", async () => {
    const connection = fakeConnection(SEED);
    mount(connection);
    await click(await screen.findByRole("button", { name: /Revoke acme deploy token/ }));

    expect(
      screen.getByText(/Sources fetching under it will refuse at their next fetch until you switch them/),
    ).toBeTruthy();
    // No dialog anywhere: the confirmation is where the list is.
    expect(document.querySelector("dialog, [role='dialog']")).toBeNull();
    expect(connection.callsNamed("sourceCredentialRevoke")).toHaveLength(0);

    await click(screen.getByRole("button", { name: "Revoke" }));
    expect(connection.callsNamed("sourceCredentialRevoke")).toEqual([
      'builtin sourceCredentialRevoke(credentialId: "cred-acme")',
    ]);
  });

  it("inserts nothing locally: the flip arrives on the row's own broadcast", async () => {
    const connection = fakeConnection(SEED);
    mount(connection);
    await click(await screen.findByRole("button", { name: /Revoke acme deploy token/ }));
    await click(screen.getByRole("button", { name: "Revoke" }));

    // Still active on screen -- nothing was written into the feed by hand:
    // only the credential that was ALREADY revoked carries the mark.
    const group = screen.getByRole("list", { name: "Your source credentials" });
    expect(within(group).getAllByText("revoked")).toHaveLength(1);

    await emit(connection, SOURCE_CREDENTIAL_CONCEPT, credential({ id: "cred-acme", status: "revoked" }));
    await waitFor(() => expect(within(group).getAllByText("revoked")).toHaveLength(2));
  });

  it("renders the server's own sentence when a revoke is refused", async () => {
    const connection = fakeConnection({ ...SEED, credentialRevokeError: "row_not_writable: that credential is not yours" });
    mount(connection);
    await click(await screen.findByRole("button", { name: /Revoke acme deploy token/ }));
    await click(screen.getByRole("button", { name: "Revoke" }));

    expect(await screen.findByText("that credential is not yours")).toBeTruthy();
  });

  it("adds one, and the token appears in that call and no other", async () => {
    const secret = "github_pat_" + "11SETTINGS" + "0123456789";
    const connection = fakeConnection(SEED);
    mount(connection);
    await click(await screen.findByRole("button", { name: "Add a credential" }));

    // The shape of token to make is stated where the token is typed.
    expect(screen.getByText(/fine-grained personal access token/)).toBeTruthy();
    await fill("A name for this github.com credential", "work laptop");
    await fill("The github.com access token", secret);
    await click(screen.getByRole("button", { name: "Add credential" }));

    expect(connection.calls.filter((call) => call.includes(secret))).toEqual([
      `builtin sourceCredentialCreate(host: "github.com", label: "work laptop", token: ${JSON.stringify(secret)})`,
    ]);
    // The form closes; the new card arrives on its own broadcast.
    await waitFor(() => expect(screen.queryByLabelText("The github.com access token")).toBeNull());
    expect(screen.getByRole("button", { name: "Add a credential" })).toBeTruthy();
  });

  it("reads the credentials feed ONCE, and the settings section opens no second one", async () => {
    const connection = fakeConnection(SEED);
    mount(connection);
    await screen.findByRole("list", { name: "Your source credentials" });
    expect(connection.calls.filter((c) => c === "query sourceCredentialsMine()")).toHaveLength(1);
  });

  it("says what to do when there are none", async () => {
    mount(fakeConnection({ credentials: [], packages: [] }));
    expect(
      await screen.findByText(/No credentials yet. A public repository needs none/),
    ).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// The viewer's own first; other people's under an owner-view heading
// (epic memql#5289, task memql#5306)
// ---------------------------------------------------------------------------

const ADAS = credential({ id: "cred-ada", ownerUserId: "u-ada", label: "ada's token", fingerprint: "...ad4a" });
const ADA: Row = { id: "u-ada", displayName: "Ada Lovelace", primaryEmail: "ada@example.com", role: "developer", active: true } as Row;

describe("Settings -> Sources: whose credentials", () => {
  it("lists the viewer's own first, and other people's under the owner-view heading with their names", async () => {
    // A cluster owner's feed carries everybody's cards (the concept's
    // clusterOwner branch); the surface presents them apart.
    mount(fakeConnection({ credentials: [ADAS, ACME, OLD], packages: [PACKAGE], people: [ADA] }));
    const mine = await screen.findByRole("list", { name: "Your source credentials" });
    expect(within(mine).getByText("acme deploy token")).toBeTruthy();
    expect(within(mine).queryByText("ada's token")).toBeNull();

    const others = await screen.findByRole("region", { name: "Other people's connections" });
    expect(within(others).getByText("Other people's connections")).toBeTruthy();
    const list = within(others).getByRole("list", { name: "Other people's source credentials" });
    expect(within(list).getByText("ada's token")).toBeTruthy();
    await waitFor(() => {
      expect(within(list).getByText("Ada Lovelace's")).toBeTruthy();
    });
    // Metadata, never a value: the fingerprint is what tells two cards apart.
    expect(within(list).getByText("...ad4a")).toBeTruthy();
  });

  it("shows no heading when nobody else holds a credential", async () => {
    mount(fakeConnection({ credentials: [ACME], packages: [PACKAGE] }));
    await screen.findByRole("list", { name: "Your source credentials" });
    expect(screen.queryByRole("region", { name: "Other people's connections" })).toBeNull();
  });

  it("shows no heading to a person who is not a cluster owner, whatever the feed carries", async () => {
    // A developer's feed never carries another person's row; if one arrived
    // anyway, the heading is the owner's and is not drawn for them.
    mount(fakeConnection({ credentials: [ACME, ADAS], packages: [PACKAGE], people: [ADA] }), "developer");
    const mine = await screen.findByRole("list", { name: "Your source credentials" });
    expect(within(mine).getByText("acme deploy token")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Other people's connections" })).toBeNull();
  });

  it("buckets by the person, not by the id's SPELLING: a canonical session id still owns the bare rows", async () => {
    // THE REGRESSION. A deployed cluster hands the session the canonical
    // `v1:identity:user:...` id while the rows carry the bare one, and the
    // cases above cannot see it because every id in them is already bare.
    // Compared raw, NOTHING is the viewer's: their own tokens fall into the
    // owner-view list beside a colleague's, and their GitHub connection --
    // which is on the feed -- reads as "not connected".
    mount(
      fakeConnection({
        credentials: [ADAS, ACME, OLD, githubGrantRow({ id: "cred-grant" })],
        packages: [PACKAGE],
        people: [ADA],
      }),
      "owner",
      "v1:identity:user:u-me",
    );

    const mine = await screen.findByRole("list", { name: "Your source credentials" });
    expect(within(mine).getByText("acme deploy token")).toBeTruthy();
    expect(within(mine).getByText("old laptop")).toBeTruthy();
    expect(within(mine).queryByText("ada's token")).toBeNull();

    // The connection is the viewer's own, so the card is the connected one
    // and the way in is not offered a second time.
    expect((await screen.findAllByText("@octocat")).length).toBeGreaterThan(0);
    expect(screen.queryByRole("button", { name: "Connect GitHub" })).toBeNull();

    // ...and the owner view holds the colleague's row, and only that.
    const others = await screen.findByRole("region", { name: "Other people's connections" });
    const list = within(others).getByRole("list", { name: "Other people's source credentials" });
    expect(within(list).getByText("ada's token")).toBeTruthy();
    expect(within(list).queryByText("acme deploy token")).toBeNull();
    await waitFor(() => expect(within(list).getByText("Ada Lovelace's")).toBeTruthy());
  });
});
