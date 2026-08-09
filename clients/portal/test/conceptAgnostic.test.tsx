// The rule that must not bend (memql#3316): NO CONCEPT-SPECIFIC RENDERING
// CODE, EVER.
//
// ===========================================================================
// THIS IS THE BEHAVIOURAL HALF OF A TWO-PART GUARD. Read both.
// ===========================================================================
//
// What THIS test proves: the renderers work generically. Several concepts with
// genuinely different shapes -- different declared cards, no card at all,
// different payload fields, different nesting -- go through the SAME
// components, and each is projected through its own declaration.
//
// What this test CANNOT prove: that no special case exists. A branch on a
// concept this file does not happen to use passes it silently, and the one
// somebody adds tomorrow will be for a concept nobody added a fixture for.
// That absence is proved by the other half, portal_render_path_test.go in the
// repo root: a text scan for concept-id literals across the whole browse path,
// which lives outside clients/portal precisely so the change that adds a
// branch cannot also delete the guard forbidding it.
//
// Neither half subsumes the other. Keep both.

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

import { RowDetail } from "../src/components/RowDetail";
import { RowList } from "../src/components/RowList";
import { describeConceptSchema } from "../src/concepts/schema";

// Four concepts, chosen for STRUCTURAL difference rather than for coverage of
// any particular domain:
//
//   1. a full card (primary + secondary + status)
//   2. a primary-only card, over a payload with a boolean flag
//   3. NO card at all -- view-kit's documented inference has to carry it
//   4. no card AND no inferable name field -- the row id is the only honest
//      answer, and the list must still be usable
interface Fixture {
  concept: Concept;
  rows: Row[];
  // What the primary slot must read for the first row.
  expectPrimary: string;
}

function concept(id: string, extra: Partial<Concept> = {}): Concept {
  const [version = "", domain = "", entity = ""] = id.split(":");
  return { id, version, domain, entity, description: "", type: "concept", ...extra };
}

const FIXTURES: Record<string, Fixture> = {
  "full card": {
    concept: concept("v1:cognition:space", {
      displayCard: { primary: "title", secondary: "kind", status: "status" },
    }),
    rows: [
      {
        id: "space-1",
        concept: "v1:cognition:space",
        createdAt: "2026-08-08T10:00:00Z",
        payload: { title: "Daily standup", kind: "daily", status: "active" },
      },
    ],
    expectPrimary: "Daily standup",
  },
  "primary-only card over a boolean": {
    concept: concept("v1:agents:agent", {
      displayCard: { primary: "name", status: "active" },
    }),
    rows: [
      {
        id: "agent-1",
        concept: "v1:agents:agent",
        payload: { name: "Sofia", active: false, role: "specialist" },
      },
    ],
    expectPrimary: "Sofia",
  },
  "no card, inferable name": {
    concept: concept("v1:identity:user"),
    rows: [
      {
        id: "user-1",
        concept: "v1:identity:user",
        payload: { displayName: "Ada", description: "cluster owner", enabled: true },
      },
    ],
    // Inference picks `displayName` off PRIMARY_NAME_FIELDS -- no per-concept
    // rule anywhere.
    expectPrimary: "Ada",
  },
  "no card, nothing inferable": {
    concept: concept("v1:observability:invocation"),
    rows: [
      {
        id: "inv-1",
        concept: "v1:observability:invocation",
        payload: { fqn: "component/memql.Execute", durationMs: 12 },
      },
    ],
    // The documented degradation: the row id, which is always identifying and
    // always clickable.
    expectPrimary: "inv-1",
  },
};

describe("the row list renders any concept through one component", () => {
  for (const [label, fixture] of Object.entries(FIXTURES)) {
    it(`projects a ${label} through its own declaration`, () => {
      render(<RowList rows={fixture.rows} concept={fixture.concept} />);
      expect(screen.getByText(fixture.expectPrimary)).toBeTruthy();
    });
  }

  it("shows a declared card's secondary and status slots, and only those", () => {
    const { concept: c, rows } = FIXTURES["full card"] as Fixture;
    const { container } = render(<RowList rows={rows} concept={c} />);
    expect(container.querySelector(".vk-row-secondary")?.textContent).toBe("daily");
    expect(container.querySelector(".vk-row-status")?.textContent).toBe("active");
    // Tertiary was not declared, so no tertiary is invented for it.
    expect(container.querySelector(".vk-row-tertiary")).toBeNull();
  });

  it("renders a boolean status as its field name, asserted or negated", () => {
    // memql#3303: `active: false` reads "not active", never the literal
    // "false" -- a boolean is not a label, the field name is.
    const { concept: c, rows } = FIXTURES["primary-only card over a boolean"] as Fixture;
    render(<RowList rows={rows} concept={c} />);
    expect(screen.getByText("not active")).toBeTruthy();
    expect(screen.queryByText("false")).toBeNull();
  });

  it("stamps the row id on every row so selection is attribute-driven", () => {
    const { container } = render(
      <RowList
        rows={(FIXTURES["no card, nothing inferable"] as Fixture).rows}
        concept={(FIXTURES["no card, nothing inferable"] as Fixture).concept}
      />,
    );
    expect(container.querySelector("[data-row-id='inv-1']")).toBeTruthy();
  });

  it("makes rows keyboard-operable when a selection handler is supplied", () => {
    const picked: string[] = [];
    render(
      <RowList
        rows={(FIXTURES["full card"] as Fixture).rows}
        concept={(FIXTURES["full card"] as Fixture).concept}
        onSelect={(id) => picked.push(id)}
      />,
    );
    const row = screen.getByRole("button");
    row.focus();
    row.click();
    expect(picked).toEqual(["space-1"]);
  });

  it("renders view-kit's empty state naming the concept's entity, whatever it is", () => {
    render(<RowList rows={[]} concept={concept("v1:planner:task")} />);
    expect(screen.getByText("No rows for task.")).toBeTruthy();
  });
});

describe("the row detail renders any concept through one component", () => {
  const NESTED: Row = {
    id: "row-1",
    concept: "v1:planner:plan",
    type: "concept",
    createdBy: "user-1",
    createdAt: "2026-08-08T10:00:00Z",
    payload: {
      goal: "ship it",
      phases: [{ name: "one" }, { name: "two" }],
      estimate: null,
    },
    provenance: { kind: "mutation", name: "createPlan" },
  };

  it("preserves the wire's nesting rather than flattening it", () => {
    // Flattening is what the row LIST does, because a card has one line to
    // spend. The detail pane is the opposite case: the intrinsics and the
    // provenance are exactly what an operator opened it to read, and a
    // flattened payload field named `type` would silently replace the row's.
    const { container } = render(<RowDetail row={NESTED} />);
    // Read the KEY cells specifically: "concept" is both a key here and the
    // value of the `type` intrinsic, and an unscoped text query cannot tell
    // those apart -- which is itself the distinction the pane exists to keep.
    const keys = [...container.querySelectorAll(".vk-key")].map((el) => el.textContent);
    for (const key of ["id", "concept", "type", "createdBy", "createdAt", "payload", "provenance"]) {
      expect(keys).toContain(key);
    }
    // The payload's own keys are nested UNDER payload, not hoisted next to it.
    expect(screen.getByText("goal")).toBeTruthy();
    expect(screen.getByText("ship it")).toBeTruthy();
    // Arrays keep their indices; nulls are shown as null rather than blank.
    expect(screen.getByText("[0]")).toBeTruthy();
    expect(screen.getByText("null")).toBeTruthy();
  });

  it("renders a structurally different row with no change of component", () => {
    render(
      <RowDetail
        row={{ id: "x", payload: { deeply: { nested: { value: 42 } } } }}
      />,
    );
    expect(screen.getByText("deeply")).toBeTruthy();
    expect(screen.getByText("42")).toBeTruthy();
  });
});

describe("the schema view describes any concept through one function", () => {
  it("derives fields from the declaration, never from the concept's identity", () => {
    // The SAME schema document under two different concept ids must produce
    // the same description -- which is only true if nothing keys off the id.
    const document = {
      type: "object",
      required: ["a"],
      properties: { a: { type: "string" }, b: { type: "number" } },
    };
    const first = describeConceptSchema([{ id: "1", concept: "v1:x:one", schema: document }]);
    const second = describeConceptSchema([{ id: "2", concept: "v1:y:two", schema: document }]);
    expect(first).toEqual(second);
    expect(first.fields.map((f) => f.name)).toEqual(["a", "b"]);
  });

  it("describes an undeclared concept from what its rows carry", () => {
    const view = describeConceptSchema([
      { id: "1", payload: { alpha: "x" } },
      { id: "2", payload: { alpha: "y", beta: 1 } },
    ]);
    expect(view.source).toBe("observed");
    expect(view.fields.map((f) => f.name)).toEqual(["alpha", "beta"]);
  });
});
