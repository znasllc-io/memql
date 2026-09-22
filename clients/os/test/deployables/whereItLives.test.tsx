import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { WhereItLivesStop } = await import(
  "../../src/apps/deployables/page/stops/WhereItLives"
);
const { accountFromRow } = await import("../../src/apps/accounts/rows");
const { withSession } = await import("../accounts/harness");
const { accountRow, fakeConnection } = await import("../accounts/harness");
const { siteRow } = await import("./harness");

// THE ONE SENTENCE UNDER THE CLIENT PICKER (epic memql#5167, section D).
//
// The tie decides more than filing now: a group tied to a client is what lets
// that client's people reach the rows tied to it, so "which client is this
// for" and "who can see it" became the same question.

function mount(connection: unknown, accountId: string) {
  h.connection = connection;
  return render(
    withSession(
      <WhereItLivesStop
        site={{ ...siteRow({ id: "s1", hostname: "store.example.com" }), accountId } as never}
        accounts={[accountFromRow(accountRow({ id: "acct-acme", name: "Acme" }))]}
        canBindDomain={false}
        clusterDomain="memql.example.com"
        onOpenDomain={() => {}}
        onAddDomain={() => {}}
      />,
    ),
  );
}

describe("who can see this deployable", () => {
  it("names the client and how many of their people", async () => {
    const view = mount(
      fakeConnection({
        groupsForAccount: [{ id: "g1", name: "Acme" }],
        membersOfGroup: {
          g1: [
            { id: "m1", groupId: "g1", userId: "u1", status: "active" },
            { id: "m2", groupId: "g1", userId: "u2", status: "active" },
          ],
        },
      }),
      "acct-acme",
    );
    expect(await screen.findByText("Visible to Acme's 2 people.")).toBeTruthy();
    view.unmount();
  });

  it("says nobody else yet when the client has no members", async () => {
    const view = mount(
      fakeConnection({ groupsForAccount: [{ id: "g1", name: "Acme" }], membersOfGroup: { g1: [] } }),
      "acct-acme",
    );
    expect(await screen.findByText("Visible to nobody else yet.")).toBeTruthy();
    view.unmount();
  });

  it("says NOTHING when the deployable has no client", async () => {
    // Not "visible to nobody else": that would be a claim about a tie which
    // does not exist, and an untied deployable is the ordinary case.
    const view = mount(fakeConnection({}), "");
    await screen.findByText("Organization");
    expect(screen.queryByText(/Visible to/)).toBeNull();
    view.unmount();
  });
});
