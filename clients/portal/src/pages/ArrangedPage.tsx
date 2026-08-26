import { useCallback, useRef, useState, type ReactNode } from "react";
import type { ConceptLike, RowLike } from "@znasllc-io/memql-view-kit";

import { AreaFrame, type AreaId } from "../app/AreaFrame";
import { Container, ErrorNotice, PageHeader } from "../ui";
import { ArrangedSection } from "./ArrangedSection";
import { RegenerateAction } from "./RegenerateAction";
import { VersionStrip } from "./VersionStrip";
import { usePageArrangements, usePageOverrideWriter } from "./usePageArrangements";
import { useRegenerate } from "./useRegenerate";
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
  // The page's OWN header actions -- a scope selector, a reload, an upload.
  // Rendered beside the regenerate affordance rather than instead of it: a
  // converged page keeps every verb it had AND becomes regenerable, and a
  // slot that held one or the other would silently make the second true only
  // for pages with no verbs.
  actions?: ReactNode;
  // Rendered directly under the header, above the sections. ROUTE-LEVEL
  // NAVIGATION goes here -- the fleet's tab bar, the Me tabs -- and it is
  // outside the arrangement deliberately: navigation is not a reading of a
  // population, and an arrangement that placed the tab bar would be an
  // arrangement a regeneration could remove, leaving a page you cannot
  // navigate away from.
  nav?: ReactNode;
  // Rendered between the header and the first section.
  notice?: ReactNode;
  // The nav AREA this page belongs to (epic memql#4654's rail work). When
  // given, the header and the area's tab strip come from AreaFrame -- which is
  // role-aware and knows which tabs a person may see, where this component's
  // `nav` slot only knows what it was handed.
  //
  // THE TWO FRAMES COMPOSE RATHER THAN COMPETE, and that is the point. Two
  // epics landed a page frame in the same release: AreaFrame owns WHERE a page
  // sits in the rail and which sibling tabs it has, and ArrangedPage owns what
  // the page IS -- its manifest, its version strip, its regenerate control.
  // Neither subsumes the other, so an area page renders one inside the other
  // and a page with no area (Me, a composed view) keeps the plain header.
  area?: AreaId;
  // Override the manifest's heading and its line of copy.
  //
  // A manifest is DATA and does not know who is reading. Most pages are named
  // for their population ("Machines", "Artifacts") and the constant is right.
  // A page named for a ROW -- your own profile is headed with your own name --
  // cannot be, and forcing it into the manifest would either put a placeholder
  // on screen or make every manifest a function.
  title?: ReactNode;
  blurb?: ReactNode;
  // Turn regeneration off for this page. The excluded surfaces (sign-in, the
  // composer's own editor, dialogs) are not arranged pages at all, so this is
  // for a page that IS one and should not be rearranged -- there are none
  // today, and the flag exists so the answer to "can I turn it off" is a
  // parameter rather than a fork of this component.
  regenerable?: boolean;
}

export function ArrangedPage({
  manifest,
  pageId,
  selectedRowId,
  onSelect,
  actions,
  area,
  nav,
  notice,
  title,
  blurb,
  regenerable = true,
}: ArrangedPageProps): ReactNode {
  const resolved = usePageArrangements(manifest, pageId);

  // What each section loaded, for the profile a regeneration is built from.
  //
  // A REF PLUS A NONCE rather than state alone: a section reports on every row
  // change, and storing that in state directly would re-render the page on
  // every page of every walk. The nonce advances only when the SET of loaded
  // sections changes, which is what the regenerate action's identity depends
  // on.
  const loaded = useRef<Record<string, { concept: ConceptLike; rows: readonly RowLike[] }>>({});
  const [loadedCount, setLoadedCount] = useState(0);
  const onLoaded = useCallback(
    (conceptId: string, concept: ConceptLike, rows: readonly RowLike[]) => {
      const had = loaded.current[conceptId] !== undefined;
      loaded.current[conceptId] = { concept, rows };
      if (!had) setLoadedCount((n) => n + 1);
    },
    [],
  );
  void loadedCount;

  const regenerate = useRegenerate(
    manifest,
    pageId,
    loaded.current,
    resolved.arrangements,
    resolved.reload,
  );

  // "Use this version" is a WRITE, not a state change: it re-writes the chosen
  // arrangement as the newest version, so the history grows and nothing is
  // destroyed. Reverting to ORIGINAL writes the manifest's own seed -- an
  // explicit "this is what I want" rather than deleting the row, which would
  // mean "I never regenerated this page" and would be silently undone by the
  // next regeneration.
  const writer = usePageOverrideWriter(pageId);
  const useVersion = useCallback(() => {
    const chosen = resolved.versions.find((v) => v.version === resolved.selected);
    if (chosen === undefined) return;
    const arrangements =
      chosen.version === 0 ? manifest.sections.map((s) => s.arrangement) : chosen.arrangements;
    void writer.write(arrangements).then(() => resolved.reload());
  }, [resolved, manifest, writer]);

  const header = (
    <>
      {actions}
      {regenerable ? (
        <RegenerateAction
          onRun={regenerate.run}
          busy={regenerate.status === "working"}
          error={regenerate.error}
          onDismiss={regenerate.dismiss}
        />
      ) : null}
    </>
  );

  // The version strip rides the header's `meta` slot, and only when there is
  // something to choose between -- a strip offering one version explains
  // nothing.
  const meta =
    resolved.versions.length > 1 ? (
      <VersionStrip
        versions={resolved.versions}
        selected={resolved.selected}
        onSelect={resolved.select}
        onUse={useVersion}
        busy={writer.busy}
      />
    ) : undefined;

  const body = (
    <>
        {/* A FAILED OVERRIDE READ LEAVES THE SEED STANDING, and says so beside
            the page rather than instead of it: the page is not broken, it is
            the page it has always been. */}
        {resolved.error !== "" ? (
          <ErrorNotice
            sentence="Could not read your saved arrangement of this page."
            next="You are looking at the page as it ships; reload to try reading it again."
            detail={resolved.error}
          />
        ) : null}
        {notice}
      {manifest.sections.map((section) => (
        <ArrangedSection
          key={section.conceptId}
          section={section}
          arrangement={resolved.arrangements[section.conceptId] ?? section.arrangement}
          selectedRowId={selectedRowId}
          onSelect={onSelect}
          onLoaded={onLoaded}
        />
      ))}
    </>
  );

  if (area !== undefined) {
    return (
      <Container>
        <AreaFrame
          area={area}
          pageId={pageId}
          title={title ?? manifest.title}
          blurb={blurb ?? manifest.blurb}
          actions={header}
          {...(meta === undefined ? {} : { meta })}
        >
          {body}
        </AreaFrame>
      </Container>
    );
  }

  return (
    <Container>
      <section className="flex min-h-full min-w-0 flex-col gap-6 pb-8">
        {/* NO CONCEPT-ID EYEBROW. memql#4657 took it off the designed views
            deliberately -- the id belongs in the area's guide, where its
            audience is, rather than above every page heading. An arranged
            page follows the same rule; ArrangedSection still shows it on a
            titled section, which is where a multi-population page needs it. */}
        <PageHeader
          pageId={pageId}
          title={title ?? manifest.title}
          blurb={blurb ?? manifest.blurb}
          actions={header}
          {...(meta === undefined ? {} : { meta })}
        />
        {nav}
        {body}
      </section>
    </Container>
  );
}
