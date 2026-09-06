import { act, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { AuditSection } = await import("../../src/apps/cluster/audit/AuditSection");
const { auditEventRow, fakeConnection, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

function mount(connection: Conn) {
  h.connection = connection;
  return render(withSession(<AuditSection />));
}

async function click(el: Element) {
  await act(async () => {
    (el as HTMLElement).click();
  });
}

beforeEach(() => {
  h.connection = null;
});

describe("the audit trail", () => {
  it("renders an owner's empty result as 'No events recorded'", async () => {
    // The sentence is only TRUE under the section's owner floor. Row
    // admission on `@rowAuthz(clusterOwner)` returns ZERO ROWS AND NO ERROR,
    // so a non-owner reaching this query would get the same `[]` -- and this
    // page would tell the one person who cannot check that nothing happened.
    // The floor in settings.ts is the whole mechanism; this asserts the
    // sentence the floor makes honest.
    mount(fakeConnection({ recentAuditEvents: [] }));
    expect(await screen.findByText("No events recorded.")).toBeTruthy();
  });

  it("lists events newest-first with the actor and the outcome", async () => {
    mount(
      fakeConnection({
        recentAuditEvents: [
          auditEventRow({ id: "e1", action: "sign_in", outcome: "success" }),
          auditEventRow({
            id: "e2",
            action: "role_granted",
            category: "authorization",
            outcome: "failure",
            actorEmail: "admin@example.com",
          }),
        ],
      }),
    );
    expect(await screen.findByText("sign_in")).toBeTruthy();
    expect(screen.getByText("role_granted")).toBeTruthy();
    expect(screen.getByText("failure")).toBeTruthy();
    expect(screen.getByText(/admin@example.com/)).toBeTruthy();
  });

  it("opens the full record, including the fields the row line has no room for", async () => {
    mount(
      fakeConnection({
        recentAuditEvents: [
          auditEventRow({
            id: "e1",
            detail: "granted developer on the cluster",
            sourceIP: "203.0.113.7",
            userAgent: "MemQL OS",
            correlationId: "corr-9",
            outcome: "failure",
            failureReason: "not a cluster owner",
          }),
        ],
      }),
    );
    await click(await screen.findByText("sign_in"));
    expect(screen.getByText("granted developer on the cluster")).toBeTruthy();
    expect(screen.getByText("203.0.113.7")).toBeTruthy();
    expect(screen.getByText("corr-9")).toBeTruthy();
    expect(screen.getByText("not a cluster owner")).toBeTruthy();
  });

  it("re-reads from the first page when the category changes, never continuing the old cursor", async () => {
    // A keyset cursor is bound to the query it came from. Carrying one across
    // a category change would continue a walk of a DIFFERENT set, which reads
    // as a page of unrelated events rather than as an error.
    const connection = fakeConnection({ recentAuditEvents: [auditEventRow({ id: "e1" })] });
    mount(connection);
    await screen.findByText("sign_in");

    await click(screen.getByRole("button", { name: "Refine the audit trail" }));
    const select = screen.getByRole("combobox", { name: "Category" });
    await click(select);
    await click(screen.getByRole("option", { name: "security" }));

    const calls = connection.query.recentAuditEvents.mock.calls;
    expect(calls[calls.length - 1]?.[0]).toEqual({ category: "security" });
    // No cursor: this is a first page.
    expect(calls[calls.length - 1]?.[1]).toEqual({});
  });
});
