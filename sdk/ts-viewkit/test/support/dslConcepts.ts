// A minimal reader for the repo's real concept definitions.
//
// WHY THIS EXISTS. "The element renders against a real concept in the tree,
// not a fixture" is only true if something checks it. A hand-written row
// object drifts silently: someone renames `startsAt`, the calendar test keeps
// passing against the old name, and the element is now built against a
// concept that no longer exists.
//
// So the element tests read dsl/<domain>/concepts.memql, assert their sample
// rows use FIELDS THE CONCEPT ACTUALLY DECLARES with values of the DECLARED
// TYPE, and then check fitness and rendering. The rows are still hand-written
// -- a unit test has no database -- but they can no longer describe a concept
// the tree does not have.
//
// TEST-ONLY, deliberately. The shipped package stays DOM-free, dependency-free
// and filesystem-free; a DSL parser in src/ would be a second, client-side
// copy of the loader's type table.

import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

// Compiled tests live at dist-test/test/support/, so the repo root is six
// levels up from this file.
const REPO_ROOT = path.resolve(fileURLToPath(import.meta.url), "../../../../../..");
const DSL_ROOT = path.join(REPO_ROOT, "dsl");

export interface DslConcept {
  readonly domain: string;
  readonly entity: string;
  readonly id: string;
  readonly displayCard?: Record<string, string>;
  // field name -> the DSL type token, e.g. "string", "datetime", "bool",
  // "int", "float", "enum(...)", "object", "[]string".
  readonly fields: ReadonlyMap<string, string>;
}

// Row intrinsics the engine puts on every row. Not declared by any concept,
// but legitimately present in a wire row and therefore usable by an element.
export const ROW_INTRINSICS: readonly string[] = [
  "id",
  "concept",
  "type",
  "schema",
  "createdAt",
  "createdBy",
  "version",
  "provenance",
];

// Strings are blanked before any brace scanning: a field description
// legitimately contains `{` and `}`, and counting those would end the concept
// body in the middle of a doc comment.
//
// LINE BY LINE, and the line count is preserved. A whole-file regex can match
// across a newline the moment a quote is unbalanced anywhere in the file,
// which silently shifts every line index after it -- the masked view has to
// stay index-aligned with the raw one.
function blankStrings(source: string): string {
  return source
    .split("\n")
    .map((line) =>
      line.replace(/"(?:\\.|[^"\\])*"/g, (m) => `"${" ".repeat(Math.max(0, m.length - 2))}"`),
    )
    .join("\n");
}

function parseDisplayCard(annotation: string): Record<string, string> {
  const card: Record<string, string> = {};
  for (const m of annotation.matchAll(/(\w+)\s*=\s*"([^"]*)"/g)) {
    card[m[1]] = m[2];
  }
  return card;
}

function parseFile(domain: string, file: string): DslConcept[] {
  const raw = fs.readFileSync(file, "utf8");
  const masked = blankStrings(raw);
  const lines = raw.split("\n");
  const maskedLines = masked.split("\n");

  const out: DslConcept[] = [];
  for (let i = 0; i < maskedLines.length; i += 1) {
    const start = /^concept\s+(\w+)\s*\{/.exec(maskedLines[i]);
    if (!start) continue;
    const entity = start[1];

    // The display card is the nearest @displayCard above the declaration,
    // before any blank line separates it from a previous construct.
    let displayCard: Record<string, string> | undefined;
    for (let j = i - 1; j >= 0; j -= 1) {
      const line = lines[j].trim();
      if (line === "") break;
      const m = /^@displayCard\((.*)\)$/.exec(line);
      if (m) {
        displayCard = parseDisplayCard(m[1]);
        break;
      }
    }

    const fields = new Map<string, string>();
    let depth = 1;
    for (let j = i + 1; j < maskedLines.length && depth > 0; j += 1) {
      const line = maskedLines[j];
      const trimmed = line.trim();
      if (depth === 1 && !trimmed.startsWith("@") && !trimmed.startsWith("//")) {
        // `name  type[!]  @annotations...` -- or `name {` for a nested block,
        // which is recorded as an object field. Read off the RAW line: an
        // enum's members are part of its type and the mask blanks them.
        const rawTrimmed = lines[j].trim();
        const nested = /^(\w+)\s*\{\s*$/.exec(rawTrimmed);
        const scalar = /^(\w+)\s+(\[\]\w+|enum\([^)]*\)|\w+)!?/.exec(rawTrimmed);
        if (nested) {
          fields.set(nested[1], "object");
        } else if (scalar) {
          fields.set(scalar[1], scalar[2]);
        }
      }
      for (const ch of line) {
        if (ch === "{") depth += 1;
        else if (ch === "}") depth -= 1;
      }
    }

    out.push({
      domain,
      entity,
      id: `v1:${domain}:${entity}`,
      displayCard,
      fields,
    });
  }
  return out;
}

let cache: DslConcept[] | undefined;

export function loadAllDslConcepts(): readonly DslConcept[] {
  if (cache) return cache;
  const found: DslConcept[] = [];
  for (const entry of fs.readdirSync(DSL_ROOT, { withFileTypes: true })) {
    if (!entry.isDirectory() || entry.name.startsWith("_") || entry.name.startsWith(".")) {
      continue;
    }
    const file = path.join(DSL_ROOT, entry.name, "concepts.memql");
    if (!fs.existsSync(file)) continue;
    found.push(...parseFile(entry.name, file));
  }
  cache = found;
  return cache;
}

export function dslConcept(id: string): DslConcept {
  const found = loadAllDslConcepts().find((c) => c.id === id);
  if (!found) {
    throw new Error(
      `${id} is not declared in the DSL tree. The element tests bind to real ` +
        `concepts on purpose -- if this concept was renamed or moved, point the ` +
        `test at its new id rather than inventing a fixture.`,
    );
  }
  return found;
}

// conceptLike projects a parsed concept into the shape view-kit consumes --
// the same three pieces the wire's ConceptInfo carries.
export function conceptLike(concept: DslConcept): {
  id: string;
  entity: string;
  displayCard?: Record<string, string>;
} {
  return concept.displayCard
    ? { id: concept.id, entity: concept.entity, displayCard: concept.displayCard }
    : { id: concept.id, entity: concept.entity };
}

// The JS types a DSL type may legitimately arrive as on the wire.
const TYPE_EXPECTATIONS: Record<string, readonly string[]> = {
  string: ["string"],
  datetime: ["string"],
  date: ["string"],
  bool: ["boolean"],
  int: ["number"],
  float: ["number"],
  object: ["object"],
};

// assertRowsMatchConcept is the guard that makes a sample row "real": every
// key is a declared field or a row intrinsic, and every value has the JS type
// the declared DSL type produces on the wire.
export function assertRowsMatchConcept(
  concept: DslConcept,
  rows: readonly Record<string, unknown>[],
  assert: {
    ok(value: unknown, message?: string): void;
    equal(actual: unknown, expected: unknown, message?: string): void;
  },
): void {
  for (const row of rows) {
    for (const [field, value] of Object.entries(row)) {
      if (ROW_INTRINSICS.includes(field)) continue;
      const declared = concept.fields.get(field);
      assert.ok(
        declared !== undefined,
        `${concept.id} does not declare a field named "${field}". The sample row ` +
          `is describing a concept that does not exist; fix the row, or the ` +
          `element was built against the wrong concept.`,
      );
      if (declared === undefined || value === null) continue;

      const enumMatch = /^enum\(/.test(declared);
      const listMatch = /^\[\]/.test(declared);
      const expected = enumMatch
        ? ["string"]
        : listMatch
          ? ["object"]
          : TYPE_EXPECTATIONS[declared];
      if (!expected) continue;
      assert.ok(
        expected.includes(typeof value),
        `${concept.id}.${field} is declared ${declared}, so a wire value is ` +
          `${expected.join(" or ")}, but the sample row carries a ${typeof value}.`,
      );

      if (enumMatch) {
        const members = [...declared.matchAll(/"([^"]*)"/g)].map((m) => m[1]);
        assert.ok(
          members.includes(String(value)),
          `${concept.id}.${field} is ${declared}; "${String(value)}" is not one of ` +
            `its members.`,
        );
      }
    }
  }
}

// syntheticRow builds one wire-shaped row from a concept's DECLARED fields.
//
// It exists for the sweeps that have to cover every concept in the tree at
// once ("no element throws on any concept", "nothing satisfies the map's
// coordinate requirement yet"). Those cannot hand-write a hundred fixtures,
// and they do not need real values -- they need real FIELD NAMES and real
// TYPES, which is exactly what the declaration carries.
export function syntheticRow(concept: DslConcept): Record<string, unknown> {
  const row: Record<string, unknown> = { id: `${concept.entity}:sample` };
  for (const [field, declared] of concept.fields) {
    if (/^\[\]/.test(declared)) {
      row[field] = [];
    } else if (/^enum\(/.test(declared)) {
      const members = [...declared.matchAll(/"([^"]*)"/g)].map((m) => m[1]);
      row[field] = members[0] ?? "";
    } else if (declared === "datetime") {
      row[field] = "2026-08-08T12:00:00Z";
    } else if (declared === "bool") {
      row[field] = true;
    } else if (declared === "int" || declared === "float") {
      row[field] = 1;
    } else if (declared === "object") {
      row[field] = {};
    } else {
      row[field] = field;
    }
  }
  return row;
}
