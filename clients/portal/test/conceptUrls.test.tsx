// Deep-linkable addresses (src/concepts/urls.ts).
//
// memql#3316 asks that "a concept, and a row within it, should each have an
// address someone can paste into chat". memQL ids are colon-delimited, and a
// colon is legal-but-escapable in a URL path segment, so the encoding is a
// real decision with a real failure mode: escape too little and an id with a
// slash breaks the router; escape too much and every link is a %3A thicket.
//
// These tests pin BOTH halves -- the encoding table, and the round trip
// through a real react-router match, which is the only thing that proves the
// param comes back byte-identical to what went in.

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useParams } from "react-router-dom";

import {
  conceptPath,
  conceptRowPath,
  conceptSchemaPath,
  conceptsPath,
  decodeSegment,
  encodeSegment,
} from "../src/concepts/urls";

// Structurally different ids on purpose: the common colon-delimited form, one
// carrying characters a path segment cannot hold, and one whose text contains
// an escape sequence that a naive "put the colons back" would corrupt.
const IDS = [
  "v1:cognition:space",
  "v1:identity:authSession",
  "row-with/slash",
  "row with space",
  "already%3Aescaped",
  "unicode-café",
  "v1:cluster:node:bff-local",
];

describe("concept URL encoding", () => {
  it("keeps colons readable rather than escaping them", () => {
    expect(encodeSegment("v1:cognition:space")).toBe("v1:cognition:space");
    expect(conceptPath("v1:cognition:space")).toBe("/concepts/v1:cognition:space");
    expect(conceptRowPath("v1:cluster:node", "bff-local")).toBe(
      "/concepts/v1:cluster:node/rows/bff-local",
    );
    expect(conceptSchemaPath("v1:cognition:space")).toBe(
      "/concepts/v1:cognition:space/schema",
    );
  });

  it("escapes everything a single path segment cannot carry", () => {
    // A slash would split the segment and hand the router a route it does not
    // have; a space would be mangled by anything that re-parses the URL.
    expect(encodeSegment("row-with/slash")).toBe("row-with%2Fslash");
    expect(encodeSegment("row with space")).toBe("row%20with%20space");
    expect(encodeSegment("a?b#c")).toBe("a%3Fb%23c");
  });

  it("does not mistake a literal %3A in an id for an escaped colon", () => {
    // The reason the restoration replaces the ESCAPE SEQUENCE rather than
    // excluding a character class: encodeURIComponent turns the id's own `%`
    // into `%25` first, so the `%3A` we put back is only ever one we escaped.
    expect(encodeSegment("already%3Aescaped")).toBe("already%253Aescaped");
    expect(decodeSegment(encodeSegment("already%3Aescaped"))).toBe("already%3Aescaped");
  });

  it("round-trips every id shape through encode/decode", () => {
    for (const id of IDS) {
      expect(decodeSegment(encodeSegment(id))).toBe(id);
    }
  });

  it("survives a malformed escape instead of throwing", () => {
    // A pasted-and-mangled link is a realistic way to reach this; showing the
    // raw text beats a blank screen from an uncaught URIError.
    expect(decodeSegment("%zz")).toBe("%zz");
  });

  it("preserves the registry's filter across a link", () => {
    expect(conceptsPath("q=session&domain=identity")).toBe(
      "/concepts?q=session&domain=identity",
    );
    expect(conceptPath("v1:x:y", "q=a")).toBe("/concepts/v1:x:y?q=a");
    // No search means no stray "?" -- a bare "?" in a pasted link looks broken.
    expect(conceptPath("v1:x:y")).toBe("/concepts/v1:x:y");
  });
});

// EchoParams is the whole point of the round-trip test: it renders what the
// ROUTER decoded, so the assertion is about the router's behaviour and not
// about our own decodeSegment agreeing with our own encodeSegment.
function EchoParams(): React.ReactElement {
  const { conceptId = "", rowId = "" } = useParams<{
    conceptId: string;
    rowId: string;
  }>();
  return (
    <>
      <span data-testid="conceptId">{conceptId}</span>
      <span data-testid="rowId">{rowId}</span>
    </>
  );
}

describe("concept URLs through a real router", () => {
  it("hands the router the exact id back, for every id shape", () => {
    for (const conceptId of IDS) {
      for (const rowId of IDS) {
        const path = conceptRowPath(conceptId, rowId);
        const { unmount } = render(
          <MemoryRouter initialEntries={[path]}>
            <Routes>
              <Route path="concepts/:conceptId">
                <Route path="rows/:rowId" element={<EchoParams />} />
              </Route>
            </Routes>
          </MemoryRouter>,
        );
        expect(screen.getByTestId("conceptId").textContent).toBe(conceptId);
        expect(screen.getByTestId("rowId").textContent).toBe(rowId);
        unmount();
      }
    }
  });

  it("resolves a pasted address -- the deep-link case -- without a prior navigation", () => {
    // What actually happens when someone opens a link out of a chat message:
    // the router boots straight at that location, with no history behind it.
    render(
      <MemoryRouter initialEntries={["/concepts/v1:cluster:node/rows/bff-local"]}>
        <Routes>
          <Route path="concepts/:conceptId">
            <Route path="rows/:rowId" element={<EchoParams />} />
          </Route>
        </Routes>
      </MemoryRouter>,
    );
    expect(screen.getByTestId("conceptId").textContent).toBe("v1:cluster:node");
    expect(screen.getByTestId("rowId").textContent).toBe("bff-local");
  });
});
