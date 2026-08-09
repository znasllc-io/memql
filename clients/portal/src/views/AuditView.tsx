import type { ReactNode } from "react";
import {
  PROPORTION_BAR_ELEMENT,
  STAT_TILE_ELEMENT,
  TIMELINE_ELEMENT,
} from "@znasllc-io/memql-view-kit";

import { ViewElement } from "./ViewElement";
import { Band, type ViewProps } from "./ViewLayout";

// Audit: what happened, in order.
//
// LAYOUT RATIONALE. The only one of the five whose roll is a TIMELINE, and
// the reason is the concept: v1:identity:auditEvent is append-only and keyed
// on occurredAt, so its rows have an intrinsic order that a sortable grid
// would throw away and then offer back as a column header. An operator
// reading an audit trail is reconstructing a sequence -- "the login failed,
// then the role changed" -- and the dated rail with newest first is that
// sequence.
//
// TWO SHAPE BANDS instead of one, which no other view gets. An audit trail
// divides two ways that matter equally: by CATEGORY (which subsystem is
// generating this traffic) and by OUTCOME (how much of it failed or was
// blocked). Collapsing them would mean choosing, and the choice differs by
// why you opened the page -- routine review reads categories, an incident
// reads outcomes.

export function AuditView({
  concept,
  rows,
  selectedRowId,
  onSelect,
}: ViewProps): ReactNode {
  const selection = selectedRowId ? { selectedRowId } : {};

  return (
    <>
      <Band>
        <ViewElement
          element={STAT_TILE_ELEMENT}
          rows={rows}
          concept={concept}
          options={{ bindings: { metric: [] } }}
        />
      </Band>

      <Band title="By outcome" meta="blocked means denied at the policy layer">
        <ViewElement
          element={PROPORTION_BAR_ELEMENT}
          rows={rows}
          concept={concept}
          // The concept declares status="outcome", so the rail finds it
          // through the display card without this view naming it.
          options={{ bindings: { value: [] } }}
        />
      </Band>

      <Band title="By category">
        <ViewElement
          element={PROPORTION_BAR_ELEMENT}
          rows={rows}
          concept={concept}
          options={{ bindings: { category: "category", value: [] } }}
        />
      </Band>

      <Band title="The trail" meta="Newest first, by when the event happened" panel>
        <ViewElement
          element={TIMELINE_ELEMENT}
          rows={rows}
          concept={concept}
          // The timeline's label / detail / status slots are explicitOnly:
          // guessing which text field names an event would be confidently
          // wrong most of the time. Named here from the concept's own card
          // ordering -- the action is what happened, the actor is who, the
          // outcome is how it ended.
          options={{
            ...selection,
            bindings: {
              at: "occurredAt",
              label: "action",
              detail: "actorEmail",
              status: "outcome",
            },
          }}
          onSelect={onSelect}
        />
      </Band>
    </>
  );
}
