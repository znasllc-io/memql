import { describe, expect, it, vi } from "vitest";

import { toKitActs, type ActSpec } from "../../src/apps/email/acts";
import type { SenderIdentityRow } from "../../src/apps/email/rows";
import { senderActs } from "../../src/apps/email/settings/senderActs";

// THE ONE SHAPE EVERY EMAIL ACTS TABLE RETURNS (DESIGN.md rule 12). A record's
// bar is a pure table from its state to the acts legal in it, so what is
// offered can be asserted without a DOM -- and so that "an act that is not
// legal is ABSENT, never disabled" is a property of a list that simply does not
// contain it, rather than of a button somebody remembered to hide.

describe("toKitActs", () => {
  const table: ActSpec[] = [
    { id: "archive", label: "Archive", text: true },
    { id: "edit", label: "Edit", text: true },
    { id: "send", label: "Send now", tone: "primary" },
  ];

  it("keeps the table's order, tone and text dress, and marks only the busy act", () => {
    const run = vi.fn();
    const acts = toKitActs(table, run, "edit");

    expect(acts.map((act) => act.label)).toEqual(["Archive", "Edit", "Send now"]);
    expect(acts.map((act) => act.tone)).toEqual([undefined, undefined, "primary"]);
    expect(acts.map((act) => act.text)).toEqual([true, true, undefined]);
    expect(acts.map((act) => act.busy)).toEqual([false, true, false]);

    // Nothing is busy unless the caller says which act is.
    expect(toKitActs(table, run).every((act) => act.busy === false)).toBe(true);
  });

  it("runs the act by its id, and a danger act keeps its tone", () => {
    const run = vi.fn();
    const acts = toKitActs([{ id: "confirm", label: "Archive", tone: "danger" }, ...table], run);
    expect(acts[0]?.tone).toBe("danger");

    acts[0]?.onAct();
    acts[3]?.onAct();
    expect(run.mock.calls).toEqual([["confirm"], ["send"]]);
  });

  it("draws nothing for a state that offers nothing", () => {
    expect(toKitActs([], vi.fn())).toEqual([]);
  });
});

function sender(over: Partial<SenderIdentityRow> = {}): SenderIdentityRow {
  return {
    id: "s1",
    ownerUserId: "v1:identity:user:me",
    address: "news@acme.com",
    fromName: "Acme News",
    replyTo: "",
    accountId: "self",
    status: "active",
    notes: "",
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

const dress = (specs: readonly ActSpec[]) => specs.map((spec) => [spec.label, spec.tone ?? (spec.text ? "text" : "quiet")]);

describe("senderActs", () => {
  it("offers an active sender Archive and Edit, both quiet", () => {
    expect(dress(senderActs(sender(), false, ""))).toEqual([["Archive", "text"], ["Edit", "text"]]);
  });

  it("asks before archiving: Keep is the way out, Archive the one button", () => {
    const specs = senderActs(sender(), false, "archive");
    expect(dress(specs)).toEqual([["Keep", "text"], ["Archive", "danger"]]);
    // The act that asks and the act that does are different acts.
    expect(specs[1]?.id).not.toBe(senderActs(sender(), false, "")[0]?.id);
  });

  it("offers an archived sender only Restore, whatever else is going on", () => {
    const archived = sender({ status: "disabled" });
    expect(dress(senderActs(archived, false, ""))).toEqual([["Restore", "primary"]]);
    // A stale edit or a stale question never brings back a legal-looking act.
    expect(dress(senderActs(archived, true, ""))).toEqual([["Restore", "primary"]]);
    expect(dress(senderActs(archived, false, "archive"))).toEqual([["Restore", "primary"]]);
  });

  it("edits with Cancel and Save, and nothing else", () => {
    expect(dress(senderActs(sender(), true, ""))).toEqual([["Cancel", "text"], ["Save", "primary"]]);
  });

  it("never offers more than three acts, and the primary is last", () => {
    for (const status of ["active", "disabled"]) {
      for (const editing of [false, true]) {
        for (const asking of ["", "archive"] as const) {
          const specs = senderActs(sender({ status }), editing, asking);
          expect(specs.length).toBeLessThanOrEqual(3);
          const primary = specs.findIndex((spec) => spec.tone === "primary" || spec.tone === "danger");
          if (primary >= 0) expect(primary).toBe(specs.length - 1);
          expect(new Set(specs.map((spec) => spec.id)).size).toBe(specs.length);
        }
      }
    }
  });
});
