// The data contract view-kit renders. Deliberately structural rather than
// imported from the SDK: view-kit must stay consumable by any caller that can
// produce these shapes, including the portal, without taking a dependency on
// the SDK's wire types.

// DisplayCardHints mirrors the per-concept rendering hints memQL publishes on
// ConceptInfo.display_card, declared in the DSL via `@displayCard(...)`. Each
// value NAMES A FIELD on the row, it is not the value itself.
export interface DisplayCardHints {
  primary?: string;
  secondary?: string;
  tertiary?: string;
  status?: string;
}

export interface ConceptLike {
  id: string;
  entity: string;
  displayCard?: DisplayCardHints;
}

export type RowLike = Record<string, unknown>;
