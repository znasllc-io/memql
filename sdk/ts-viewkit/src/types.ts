// The data contract view-kit renders. Deliberately structural rather than
// imported from the SDK: view-kit must stay consumable by any caller that can
// produce these shapes, including the portal, without taking a dependency on
// the SDK's wire types.

// DisplayCardHints mirrors the per-concept rendering hints MemQL publishes on
// ConceptInfo.display_card, declared in the DSL via `@displayCard(...)`. Each
// value NAMES A FIELD on the row, it is not the value itself.
export interface DisplayCardHints {
  primary?: string;
  secondary?: string;
  tertiary?: string;
  status?: string;
}

// DeclaredFieldKind is the DSL's own vocabulary for a field's type, as MemQL
// publishes it on ConceptInfo.fields (epic memql#4661). It is NOT view-kit's
// FieldKind: that one is "what shape of thing is in this cell", coarse on
// purpose, and it is what an element's requirements match against. This one is
// what the author WROTE, which carries two distinctions the rendering kinds
// deliberately drop -- an `enum` is a text field with a known member list, and
// an `integer` is a number with no fractional part.
//
// A consumer must DEGRADE on an unrecognised value rather than drop the field:
// the set can grow, and a field rendered as text is a worse outcome than a
// field that vanished.
export type DeclaredFieldKind =
  | "string"
  | "boolean"
  | "integer"
  | "number"
  | "datetime"
  | "enum"
  | "array"
  | "object";

// ConceptFieldLike is one declared field. Structurally identical to the SDK's
// `ConceptField`, restated here for the reason at the top of this file.
export interface ConceptFieldLike {
  name: string;
  kind: DeclaredFieldKind | string;
  required?: boolean;
  // Declared members, for kind === "enum". Empty or absent otherwise.
  enumValues?: readonly string[];
  description?: string;
}

// ConceptRelationshipLike is one declared edge, carrying BOTH axes
// (memql#3652). `type` is the closed engine set and drives traversal; `as` is
// the open domain label and is the only one of the two a person should ever
// read. `as` is absent on every declaration predating the split -- fall back
// to `field`, never to `type`.
export interface ConceptRelationshipLike {
  type: string;
  as?: string;
  // The field holding the pointer. May be a dotted path when the pointer
  // sits inside a nested block.
  field: string;
  target: string;
  direction?: string;
}

export interface ConceptLike {
  id: string;
  entity: string;
  displayCard?: DisplayCardHints;
  // The DECLARED SHAPE, when the cluster published one (epic memql#4661).
  //
  // UNDEFINED AND EMPTY MEAN THE SAME THING HERE and both mean "no shape
  // published" -- either the server predates the fields or the concept's
  // definition schema did not parse. profileConcept falls back to profiling
  // the loaded rows, which is what it did exclusively before these arrived,
  // so a concept with no published shape renders exactly as it used to.
  fields?: readonly ConceptFieldLike[];
  relationships?: readonly ConceptRelationshipLike[];
}

export type RowLike = Record<string, unknown>;
