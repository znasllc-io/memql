import { describe, expect, it } from "vitest";

import { attrsInline, conceptWord, levelWord, logRowFromRow } from "../../src/logs/rows";

// The row projection is defensive on every field (epic memql#4895): a
// synthetic node from the log store has passed through no graph admission
// and no shape, so nothing upstream vouched for it.

describe("projecting a log line", () => {
  it("reads a flat row and a payload-nested one to the same answer", () => {
    const flat = logRowFromRow({
      id: "l-1",
      occurredAt: "2026-09-03T11:30:00Z",
      nodeType: "bff",
      node: "bff-0",
      level: "warn",
      component: "packages.pipeline",
      app: "",
      message: "slow",
      attributes: { ms: 1200 },
      subject: "dep-1",
      subjectConcept: "v1:platform:packageDeployment",
      session: "",
      userId: "",
    });
    const nested = logRowFromRow({
      id: "l-1",
      payload: {
        occurredAt: "2026-09-03T11:30:00Z",
        nodeType: "bff",
        node: "bff-0",
        level: "warn",
        component: "packages.pipeline",
        message: "slow",
        attributes: { ms: 1200 },
        subject: "dep-1",
        subjectConcept: "v1:platform:packageDeployment",
      },
    });
    expect(nested).toEqual(flat);
    expect(flat.at.toISOString()).toBe("2026-09-03T11:30:00.000Z");
    expect(flat.level).toBe("warn");
  });

  it("narrows an unknown level to info rather than to the loudest or the silent reading", () => {
    expect(logRowFromRow({ id: "x", level: "fatal" }).level).toBe("info");
    expect(logRowFromRow({ id: "x", level: 3 }).level).toBe("info");
    expect(logRowFromRow({ id: "x" }).level).toBe("info");
  });

  it("reads a missing or malformed field as its empty value, never as a crash", () => {
    const row = logRowFromRow({ id: 42, attributes: "not an object", message: null, occurredAt: "garbage" });
    expect(row.id).toBe("42");
    expect(row.attributes).toEqual({});
    expect(row.message).toBe("");
    expect(row.occurredAt).toBe("garbage");
    expect(row.at.getTime()).toBe(0);
  });

  it("falls back to createdAt for the instant, which is what the wire calls occurredAt", () => {
    expect(logRowFromRow({ id: "x", createdAt: "2026-09-03T11:30:00Z" }).occurredAt).toBe("2026-09-03T11:30:00Z");
  });
});

describe("the words on a row", () => {
  it("names every level in full -- colour is never the only carrier", () => {
    expect(levelWord("debug")).toBe("Debug");
    expect(levelWord("info")).toBe("Info");
    expect(levelWord("warn")).toBe("Warning");
    expect(levelWord("error")).toBe("Error");
  });

  it("renders attributes as key=value pairs, quoting a value with a space", () => {
    expect(attrsInline({ ms: 12, host: "a b", ok: true, nested: { x: 1 } })).toBe(
      'ms=12 host="a b" ok=true nested={"x":1}',
    );
    expect(attrsInline({})).toBe("");
  });

  it("names a concept by its last segment for the subject mark, and 'subject' when there is none", () => {
    expect(conceptWord("v1:platform:packageDeployment")).toBe("packageDeployment");
    expect(conceptWord("")).toBe("subject");
  });
});
