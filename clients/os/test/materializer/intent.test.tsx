import { describe, it, expect } from "vitest";

import { MATERIALIZER_SECTIONS } from "../../src/apps/materializer/settings";
import { FORMATS } from "../../src/apps/materializer/words";
import {
  COMPOSE_FORMATS,
  MATERIALIZER_APP_ID,
  MATERIALIZER_COMPOSE_SECTION,
  composeIntent,
  readComposeIntent,
} from "../../src/items/materialize";

// The contract another app opens the Materializer with. It lives in one module
// under items/ so a sender never imports from apps/materializer/, and it is
// validated on the way IN: an intent is a payload some other app built, and a
// field this app cannot use is dropped rather than trusted.

describe("Materializer starting setup", () => {
  it("round-trips a valid setup", () => {
    const setup = { format: "markdown" as const, folderId: "folder-1", sources: [{ kind: "library_file" as const, ref: "f1", label: "notes.md" }], returnTo: { appId: "files", sectionId: "browse" } };
    expect(readComposeIntent(composeIntent(setup))).toEqual(setup);
  });
  it("drops unknown formats and malformed sources", () => {
    expect(readComposeIntent({ compose: { format: "exe", sources: [{ kind: "x" }, { kind: "library_file", ref: "f", label: "a" }] } }))
      .toEqual({ sources: [{ kind: "library_file", ref: "f", label: "a" }] });
  });
  it("ignores payloads without a setup", () => {
    expect(readComposeIntent({ compositionId: "c1" })).toBeNull();
  });

  it("round-trips the email template kind", () => {
    const setup = { format: "json" as const, outputKind: "email_template" as const };
    expect(readComposeIntent(composeIntent(setup))).toEqual(setup);
  });

  it("is a setup even when nothing in it can be used", () => {
    // The sender asked to compose; a setup with nothing usable in it still
    // opens the composer, blank.
    expect(readComposeIntent({ compose: {} })).toEqual({});
    expect(readComposeIntent({ compose: { format: "exe", folderId: 7, returnTo: "files" } })).toEqual({});
  });

  it.each([null, "markdown", 7, ["markdown"]])("is not a setup when `compose` is %j", (compose) => {
    expect(readComposeIntent({ compose })).toBeNull();
  });

  it("keeps a source only when its kind is one the Materializer takes and every field is a string", () => {
    const kept = [
      { kind: "concept_row", ref: "v1:x:invoice#a", label: "INV-1" },
      { kind: "library_file", ref: "f1", label: "" },
      { kind: "query", ref: "query openInvoices()", label: "invoices" },
    ];
    const dropped = [
      { kind: "folder", ref: "f", label: "a" },
      { kind: "query", ref: 7, label: "a" },
      { kind: "query", ref: "r", label: null },
      { kind: "query", ref: "   ", label: "a" },
      { kind: "query", label: "a" },
      "query",
      null,
    ];
    expect(readComposeIntent({ compose: { sources: [...dropped, ...kept] } })).toEqual({ sources: kept });
  });

  it("carries no sources key when none survive", () => {
    expect(readComposeIntent({ compose: { sources: [{ kind: "x" }] } })).toEqual({});
    expect(readComposeIntent({ compose: { sources: "f1" } })).toEqual({});
  });

  it("takes a return target only with both its app and its section", () => {
    expect(readComposeIntent({ compose: { returnTo: { appId: "files" } } })).toEqual({});
    expect(readComposeIntent({ compose: { returnTo: { sectionId: "browse" } } })).toEqual({});
    expect(readComposeIntent({ compose: { returnTo: { appId: "files", sectionId: 3 } } })).toEqual({});
    expect(readComposeIntent({ compose: { returnTo: { appId: "files", sectionId: "browse" } } }))
      .toEqual({ returnTo: { appId: "files", sectionId: "browse" } });
  });

  it("takes an output kind only if it is the one the Materializer offers", () => {
    expect(readComposeIntent({ compose: { outputKind: "newsletter" } })).toEqual({});
  });

  // An email template is a JSON document, which the cluster enforces. A setup that
  // pairs the kind with another format cannot be made, so the kind is the half
  // that goes: the sender's format is what it asked for by name.
  it("keeps the email template kind only with JSON, or with no format at all", () => {
    expect(readComposeIntent({ compose: { outputKind: "email_template" } })).toEqual({ outputKind: "email_template" });
    expect(readComposeIntent({ compose: { format: "json", outputKind: "email_template" } })).toEqual({ format: "json", outputKind: "email_template" });
    expect(readComposeIntent({ compose: { format: "html", outputKind: "email_template" } })).toEqual({ format: "html" });
    expect(readComposeIntent({ compose: { format: "markdown", outputKind: "email_template" } })).toEqual({ format: "markdown" });
    // A format this cluster does not write is dropped first, which leaves none.
    expect(readComposeIntent({ compose: { format: "exe", outputKind: "email_template" } })).toEqual({ outputKind: "email_template" });
  });

  it("takes a folder only as a non-empty string", () => {
    expect(readComposeIntent({ compose: { folderId: "" } })).toEqual({});
    expect(readComposeIntent({ compose: { folderId: "  " } })).toEqual({});
    expect(readComposeIntent({ compose: { folderId: "folder-1" } })).toEqual({ folderId: "folder-1" });
  });
});

// What a sender and the app must agree on. The names are written once, in
// items/materialize.ts, and a sender only REFERENCES them; the app's own copies
// of the same facts are what these pin. An `openApp` to a section the manifest
// does not declare moves nobody and says nothing, which is how the plan's own
// first draft of this contract ("compose" for "composer") would have shipped.
describe("what a sender and the Materializer must agree on", () => {
  it("opens a section the Materializer declares", () => {
    expect(MATERIALIZER_SECTIONS.map((section) => section.id)).toContain(MATERIALIZER_COMPOSE_SECTION);
    expect(MATERIALIZER_COMPOSE_SECTION).toBe("composer");
  });

  it("reaches the app by the id its manifest, its Logs scope and Files already use", () => {
    expect(MATERIALIZER_APP_ID).toBe("materializer");
  });

  it("offers the formats the composer offers, in the same order", () => {
    expect([...COMPOSE_FORMATS]).toEqual([...FORMATS]);
  });
});
