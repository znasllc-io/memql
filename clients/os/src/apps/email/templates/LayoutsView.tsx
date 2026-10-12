import { LayoutTemplate } from "lucide-react";

import { Button, EmptyState, Notice, RecordList, RecordRow, Refine, Select, type RefineChip } from "../../../kit";
import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import { layoutIsArchived, layoutTitle, type LayoutRow } from "../rows";
import type { EmailLayouts } from "../useEmail";
import { WORDS } from "../words";

// THE LAYOUTS VIEW (Ruling 24): what an email can be made from. The four
// built-ins ship with the renderer and every organization starts from them, so
// they are listed once, first; then each organization's own, saved from a
// finished email in the editor.
//
// NOT A LIVE FEED. Layouts come from one `emailLayouts` reading per
// organization, kept current by the section's `AutoRefresh`; this view draws
// what the readings know, and says so when one could not be read -- a refusal
// is never drawn as a list with nothing in it.
//
// There is NO ADD here: a layout is saved in the editor ("Save as layout"), and
// the sentence under the built-ins says where, for an organization that has
// none of its own.

/** What is being asked of the layouts list. */
export interface LayoutsQuestion {
  search: string;
  /** The organization asked about, or "" for every one. The built-ins are every organization's. */
  accountId: string;
}

export const EVERY_LAYOUT: LayoutsQuestion = { search: "", accountId: "" };

/** The layouts a question asks for: by name and description, and by organization. */
export function layoutsAsked(layouts: readonly LayoutRow[], question: LayoutsQuestion): LayoutRow[] {
  const needle = question.search.trim().toLowerCase();
  return layouts.filter(
    (layout) =>
      (question.accountId === "" || layout.kind === "builtin" || layout.accountId === question.accountId) &&
      (needle === "" || layoutTitle(layout).toLowerCase().includes(needle) || layout.description.toLowerCase().includes(needle)),
  );
}

export function LayoutsRefine({ question, onQuestion, organizations }: {
  question: LayoutsQuestion;
  onQuestion: (next: LayoutsQuestion) => void;
  /** The organizations in view, by id and name. A facet only when there is more than one. */
  organizations: readonly { id: string; name: string }[];
}) {
  const chips: RefineChip[] =
    question.accountId === ""
      ? []
      : [
          {
            id: "organization",
            label: organizations.find((organization) => organization.id === question.accountId)?.name || "Organization",
            onRemove: () => onQuestion({ ...question, accountId: "" }),
          },
        ];
  return (
    <Refine iconOnly search={question.search} onSearch={(search) => onQuestion({ ...question, search })} chips={chips} label="Refine layouts">
      {organizations.length > 1 ? (
        <Select id="email-layouts-organization" label="Organization" value={question.accountId} onChange={(accountId) => onQuestion({ ...question, accountId })}>
          <option value="">Any organization</option>
          {organizations.map((organization) => (
            <option key={organization.id} value={organization.id}>
              {organization.name}
            </option>
          ))}
        </Select>
      ) : null}
    </Refine>
  );
}

export function LayoutsView({ layouts, shown, ownInView, archivedHidden, narrowed, organizationName, missing, readingOrganizations, onOpen }: {
  layouts: EmailLayouts;
  /** The layouts the question asks for, built-ins first. */
  shown: readonly LayoutRow[];
  /** Whether any organization in view has a layout of its own, whatever is being searched. */
  ownInView: boolean;
  /** An organization's own layouts are archived and Settings hides them. */
  archivedHidden: boolean;
  narrowed: boolean;
  organizationName: (accountId: string) => string;
  /** A layout somebody asked for that no reading holds. */
  missing: boolean;
  /** The organizations are still being read, so there is nothing to ask about yet. */
  readingOrganizations: boolean;
  onOpen: (layout: LayoutRow) => void;
}) {
  const refused = [...layouts.refusals];
  const notices = (
    <>
      {missing ? <Notice tone="info" sentence={WORDS.unavailable.layout} /> : null}
      {refused.map(([accountId, refusal]) => (
        <Notice key={accountId} tone="error" sentence={`This cluster did not return the layouts for ${organizationName(accountId)}.`} detail={refusal}>
          <Button onClick={() => void layouts.reload()}>Try again</Button>
        </Notice>
      ))}
    </>
  );

  // Nothing read yet: the shape of a list, and no sentence about what it holds.
  if (layouts.readings.size === 0 && refused.length === 0) {
    if (!layouts.settled && (readingOrganizations || layouts.accountIds.length > 0)) {
      return <RecordListSkeleton label="Loading layouts" />;
    }
    return (
      <EmptyState icon={LayoutTemplate} title="No layouts yet">
        Layouts you save from a finished email in the editor appear here.
      </EmptyState>
    );
  }

  return (
    <>
      {notices}
      {shown.length === 0 ? (
        narrowed ? (
          <EmptyState icon={LayoutTemplate} title="No matching layouts">
            Try another name or organization.
          </EmptyState>
        ) : null
      ) : (
        <RecordList as="ul" label="Layouts">
          {shown.map((layout) => (
            <LayoutLine key={layout.id} layout={layout} organization={layout.kind === "builtin" ? WORDS.builtIn : organizationName(layout.accountId)} onOpen={() => onOpen(layout)} />
          ))}
        </RecordList>
      )}
      {/* Said once, under the built-ins, and only when it is true of what could be
          read; when the setting is why none is listed, the setting is named (rule 4). */}
      {!ownInView && layouts.settled && refused.length === 0 ? (
        <p className="os-caption">
          {archivedHidden ? "Archived layouts are hidden. Turn on Include archived in Settings to see them." : "Layouts you save from a finished email in the editor appear here."}
        </p>
      ) : null}
    </>
  );
}

function LayoutLine({ layout, organization, onOpen }: { layout: LayoutRow; organization: string; onOpen: () => void }) {
  const archived = layoutIsArchived(layout);
  return (
    <RecordRow
      icon={<LayoutTemplate size={16} aria-hidden />}
      name={layoutTitle(layout)}
      secondary={layout.description}
      // A built-in has no lifecycle, so no state word; its summary says where it comes from.
      state={layout.kind === "builtin" ? undefined : archived ? WORDS.archived : WORDS.active}
      current={!archived}
      dim={archived}
      onOpen={onOpen}
    >
      <span className="os-caption">{organization}</span>
    </RecordRow>
  );
}
