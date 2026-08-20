import type { ReactNode } from "react";

import { Band as UiBand, Button, PopulationMeta as UiPopulationMeta } from "../ui";

// The chrome a composed view is drawn in.
//
// Deliberately the same grammar as the predefined views: a header that names
// the population, then BANDS -- a reading, a shape, a roll. A composed view
// should feel like a sibling of the five designed ones, because it IS one:
// the same elements, the same bands, the same typography. The only
// difference is who chose the arrangement.
//
// This file used to CARRY A COPY of the views' layout primitives, because
// they lived inside the guarded predefined-view directory whose file list a
// repo-root test counts, and coupling to that list bought nothing. The
// primitives now live in src/ui -- outside the guarded tree -- so the copy's
// reason evaporated and these are thin namings of the shared vocabulary:
// what stays here is only what is composer-specific (an h3 band, the accent
// save tone, the per-concept SectionHeader).

// ComposeBand is ui/Band at the h3 level: a composed view stacks per-concept
// sections, so its bands sit one heading level below a page's.
export function ComposeBand({
  title,
  meta,
  panel = false,
  children,
}: {
  title?: string;
  meta?: ReactNode;
  panel?: boolean;
  children: ReactNode;
}): ReactNode {
  return (
    <UiBand
      {...(title === undefined ? {} : { title })}
      {...(meta === undefined ? {} : { meta })}
      panel={panel}
      headingLevel="h3"
    >
      {children}
    </UiBand>
  );
}

// ComposeButton keeps the composer's three-tone vocabulary ("accent" is the
// one commitment on the page -- save) and maps it onto ui/Button's tones.
export function ComposeButton({
  onClick,
  disabled = false,
  tone = "quiet",
  title,
  children,
}: {
  onClick: () => void;
  disabled?: boolean;
  tone?: "quiet" | "accent" | "danger";
  title?: string;
  children: ReactNode;
}): ReactNode {
  return (
    <Button
      size="xs"
      tone={tone === "accent" ? "primary" : tone}
      onClick={onClick}
      disabled={disabled}
      {...(title === undefined ? {} : { title })}
    >
      {children}
    </Button>
  );
}

// SectionHeader names one concept's section of a composed view.
//
// A composed view may stack several concepts, so unlike a predefined view the
// concept id belongs INSIDE the page rather than only in its header: it is the
// one thing that tells a reader which rows a section is about, and it is the
// address they would paste into a query. Monospace, because ids are read
// character by character.
export function SectionHeader({
  conceptId,
  entity,
  meta,
  actions,
}: {
  conceptId: string;
  entity: string;
  meta?: ReactNode;
  actions?: ReactNode;
}): ReactNode {
  return (
    <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-2 border-b border-line pb-3">
      <div className="min-w-0">
        <p className="font-mono text-xs break-all text-subtle">{conceptId}</p>
        <h2 className="mt-0.5 text-base font-semibold tracking-tight">{entity}</h2>
      </div>
      <div className="flex shrink-0 flex-col items-end gap-2">
        {actions === undefined ? null : (
          <div className="flex flex-row flex-wrap items-center justify-end gap-2">
            {actions}
          </div>
        )}
        {meta}
      </div>
    </div>
  );
}

// The composer's population meta IS the shared one.
export const PopulationMeta = UiPopulationMeta;
