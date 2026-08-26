import { type ReactNode } from "react";

import { Container, PageHeader } from "../ui";
import { ArrangedSection } from "./ArrangedSection";
import type { PageManifest } from "./manifest";

// AN ARRANGED PAGE: the one page component the portal has (epic memql#4661).
//
// ===========================================================================
// WHAT MAKES THIS THE PAGE SYSTEM RATHER THAN A VIEW SYSTEM
// ===========================================================================
// A page is a manifest -- a title, a line of copy, and a list of sections,
// each of which is one concept, one arrangement and its required entries. That
// is ALL a page is, and the claim of the epic's D6 is that it is enough for
// every page in the console: forms and editors are widgets, three-dimensional
// readings are scenes, and layout is a value in the arrangement.
//
// The five predefined views, the composed views, and the converged fleet /
// artifacts / deployables / profile pages all render through this. There is no
// branch here for which kind a page is, because there is no longer a
// difference to branch on.
//
// ===========================================================================
// RESOLUTION HAPPENS ABOVE THE SECTIONS
// ===========================================================================
// Which arrangement a section renders -- the caller's override version, or the
// manifest's seed -- is decided once per page, in usePageArrangements, and
// handed down. Deciding it per section would run the override read once per
// section and would let two sections of one page disagree about which version
// they are showing.

export interface ArrangedPageProps {
  manifest: PageManifest;
  // The page id an override row is keyed on. Usually manifest.pageId; passed
  // separately so a caller that derives it (a view slug, a fleet surface) has
  // one place that does so rather than restating it in the manifest.
  pageId: string;
  selectedRowId: string;
  onSelect: (rowId: string) => void;
  // Rendered in the page header, right-aligned. Where the regenerate
  // affordance and the version strip go (task memql#4669).
  actions?: ReactNode;
  // Rendered between the header and the first section.
  notice?: ReactNode;
  // The arrangement to render per section concept id. Absent falls back to the
  // manifest's own seed, which is what a page with no override shows -- and
  // what EVERY page shows until somebody regenerates it.
  arrangements?: Readonly<Record<string, import("@znasllc-io/memql-view-kit").Arrangement>>;
}

export function ArrangedPage({
  manifest,
  pageId,
  selectedRowId,
  onSelect,
  actions,
  notice,
  arrangements,
}: ArrangedPageProps): ReactNode {
  void pageId;
  return (
    <Container>
      <section className="flex min-h-full min-w-0 flex-col gap-6 pb-8">
        <PageHeader
          eyebrow={manifest.sections[0]?.conceptId ?? ""}
          title={manifest.title}
          blurb={manifest.blurb}
          {...(actions === undefined ? {} : { actions })}
        />
        {notice}
        {manifest.sections.map((section) => (
          <ArrangedSection
            key={section.conceptId}
            section={section}
            arrangement={arrangements?.[section.conceptId] ?? section.arrangement}
            selectedRowId={selectedRowId}
            onSelect={onSelect}
          />
        ))}
      </section>
    </Container>
  );
}
