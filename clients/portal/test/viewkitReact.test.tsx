// The VNode -> ReactNode walk (src/viewkit/react.ts) and the row projection
// it is fed. These are the two pieces of glue standing between view-kit and
// React, so they are where a reuse regression would land.

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { h, renderRowList, text } from "@znasllc-io/memql-view-kit";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

import { vnodeToReact } from "../src/viewkit/react";
import { flattenForList } from "../src/viewkit/rows";

describe("vnodeToReact", () => {
  it("renames class to className so view-kit's class contract survives", () => {
    const { container } = render(
      <>{vnodeToReact(h("span", { class: "vk-row-primary" }, [text("hello")]))}</>,
    );
    const span = container.querySelector("span");
    expect(span?.className).toBe("vk-row-primary");
    expect(span?.getAttribute("class")).toBe("vk-row-primary");
  });

  it("passes data-* attributes through verbatim -- they are view-kit's interactivity contract", () => {
    const { container } = render(
      <>{vnodeToReact(h("li", { "data-row-id": "v1:x:y:1", "data-selected": "true" }, []))}</>,
    );
    const li = container.querySelector("li");
    expect(li?.getAttribute("data-row-id")).toBe("v1:x:y:1");
    expect(li?.getAttribute("data-selected")).toBe("true");
  });

  it("creates void elements with no children argument", () => {
    // React throws "br is a void element tag and must neither have children
    // nor use dangerouslySetInnerHTML" if children are passed at all.
    expect(() => render(<>{vnodeToReact(h("br", {}, []))}</>)).not.toThrow();
  });

  it("walks nested children", () => {
    const tree = h("div", { class: "outer" }, [
      h("span", { class: "inner" }, [text("deep")]),
    ]);
    const { container } = render(<>{vnodeToReact(tree)}</>);
    expect(container.querySelector(".outer .inner")?.textContent).toBe("deep");
  });
});

describe("flattenForList", () => {
  it("merges payload fields up so a display card's slot names resolve", () => {
    const node: Row = { id: "v1:x:y:1", payload: { name: "Alpha", status: "active" } };
    expect(flattenForList(node)).toEqual({
      id: "v1:x:y:1",
      name: "Alpha",
      status: "active",
    });
  });

  it("lets the row intrinsic win over a payload field of the same name", () => {
    // The row id is what view-kit stamps on data-row-id; a payload `id`
    // winning there would make a subsequent row fetch resolve the wrong row.
    const node: Row = { id: "row-intrinsic", payload: { id: "payload-value" } };
    expect(flattenForList(node)["id"]).toBe("row-intrinsic");
  });

  it("preserves a non-object payload rather than dropping it", () => {
    const node: Row = { id: "a", payload: null };
    expect(flattenForList(node)).toEqual({ id: "a", payload: null });
  });
});

describe("renderRowList through the React adapter", () => {
  const concept: Concept = {
    id: "v1:cluster:node",
    version: "v1",
    domain: "cluster",
    entity: "node",
    description: "",
    type: "concept",
    displayCard: { primary: "name", status: "state" },
  };

  it("renders a concept's rows via its @displayCard hints, with no concept-specific code", () => {
    const rows: Row[] = [
      { id: "v1:cluster:node:bff", payload: { name: "bff", state: "healthy" } },
      { id: "v1:cluster:node:agent", payload: { name: "agent", state: "healthy" } },
    ];
    render(
      <>{vnodeToReact(renderRowList(rows.map(flattenForList), concept))}</>,
    );
    expect(screen.getByText("bff")).toBeTruthy();
    expect(screen.getByText("agent")).toBeTruthy();
    expect(screen.getAllByText("healthy")).toHaveLength(2);
  });

  it("falls back to the row id when the display-card field is absent", () => {
    const rows: Row[] = [{ id: "v1:cluster:node:orphan", payload: {} }];
    render(<>{vnodeToReact(renderRowList(rows.map(flattenForList), concept))}</>);
    expect(screen.getByText("v1:cluster:node:orphan")).toBeTruthy();
  });

  it("renders view-kit's empty state for an empty row set", () => {
    render(<>{vnodeToReact(renderRowList([], concept))}</>);
    expect(screen.getByText("No rows for node.")).toBeTruthy();
  });
});
