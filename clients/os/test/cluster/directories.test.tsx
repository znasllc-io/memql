import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { AgentsSection } from "../../src/apps/cluster/agents/AgentsSection";
import { AuditSection } from "../../src/apps/cluster/audit/AuditSection";
import { DeadLetterBand } from "../../src/apps/cluster/origins/DeadLetters";
import { fakeConnection, rowsResult, withSession, agentRow, auditEventRow } from "./harness";

const cases = [
  { name: "agents", method: "allAgents" as const, list: "Agents in this cluster", search: "Search agents", placeholder: "Search agents", row: (n: number): Row => agentRow({ id: `agent-${n}`, name: `Resource ${n}` }), view: () => <AgentsSection showInactive={false} /> },
  { name: "audit", method: "auditEventsBrowse" as const, list: "Audit events", search: "Refine the audit trail", placeholder: "Search audit events", row: (n: number): Row => auditEventRow({ id: `audit-${n}`, action: `Resource ${n}` }), view: () => <AuditSection /> },
  { name: "dead letters", method: "outboxDeadLettersBrowse" as const, list: "shopify dead letters", search: "Search dead letters", placeholder: "Search dead letters", row: (n: number): Row => ({ id: `dead-${n}`, conceptId: `Resource ${n}`, target: "shopify", status: "dead" }), view: () => <DeadLetterBand connector="shopify" /> },
];
for (const fixture of cases) it(`${fixture.name} appends tens, retains pages and searches older records`, async () => {
  const connection = fakeConnection();
  const all = Array.from({ length: 23 }, (_, n) => fixture.row(n));
  const read = connection.query[fixture.method].mockImplementation(async (args: { search?: string }, opts: { cursor?: string }) => {
    const matching = args.search ? all.filter(row => JSON.stringify(row).includes(args.search!)) : all;
    const offset = Number(opts.cursor || 0);
    return rowsResult(matching.slice(offset, offset + 10), offset + 10 < matching.length ? String(offset + 10) : "");
  });
  h.connection = connection;
  const view = render(withSession(fixture.view()));
  await screen.findByText("Resource 9");
  expect(screen.queryByText("Resource 10")).toBeNull();
  expect(read).toHaveBeenCalledTimes(1);
  fireEvent.click(screen.getByRole("button", { name: "Load more" }));
  await screen.findByText("Resource 19");
  view.unmount();
  const reopened = render(withSession(fixture.view()));
  await screen.findByText("Resource 19");
  expect(read).toHaveBeenCalledTimes(2);
  fireEvent.click(screen.getByRole("button", { name: fixture.search }));
  fireEvent.change(screen.getByPlaceholderText(fixture.placeholder), { target: { value: "Resource 22" } });
  await waitFor(() => expect(read).toHaveBeenCalledTimes(3));
  expect(await within(screen.getByRole("list", { name: fixture.list })).findByText("Resource 22")).toBeTruthy();
  expect(screen.queryByText("Resource 0")).toBeNull();
  reopened.unmount();
});
