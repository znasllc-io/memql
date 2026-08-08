// The context a concept's panes share.
//
// A tiny module of its own so the parent (ConceptPage) and the panes it
// renders through <Outlet> can both import the type without importing each
// other -- the circular import that otherwise appears the moment a child
// needs the parent's shape.
//
// The PARENT owns the row walk, and that is load-bearing rather than
// incidental: the rows pane is mounted at two routes (with and without a
// selected row), so holding the walk in the pane would restart paging every
// time an operator clicked a row. Owned above the <Outlet>, the walk is
// untouched by navigation between a concept's panes.

import { useOutletContext } from "react-router-dom";
import type { Concept } from "@znasllc-io/memql-sdk-core/client";

import type { ConceptRowsState } from "../cluster/useConceptRows";

export interface ConceptPaneContext {
  concept: Concept;
  rows: ConceptRowsState;
}

export function useConceptPane(): ConceptPaneContext {
  return useOutletContext<ConceptPaneContext>();
}
