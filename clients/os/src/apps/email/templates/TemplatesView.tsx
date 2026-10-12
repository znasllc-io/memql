import { FileText } from "lucide-react";
import { refusalSentence, type Row } from "@znasllc-io/memql-sdk-core/client";

import { Button, EmptyState, LiveList, Notice, RecordRow, Refine, Select, type RefineChip } from "../../../kit";
import { InlineSkeleton } from "../../../kit/ContentSkeleton";
import type { LiveListSource } from "../../../live/LiveList";
import {
  isPublished,
  layoutName,
  templateFingerprint,
  templateFromRow,
  templateIsArchived,
  templateName,
  type LayoutRow,
  type TemplateRow,
} from "../rows";
import { WORDS, templateState } from "../words";

// THE TEMPLATES VIEW: every template, with the layout it was made from and its
// state in one word (Ruling 6: Draft, Published, Archived).
//
// THE LIST IS LIVE (`v1:campaigns:template` broadcasts) and seeded unfiltered:
// every question here is asked of rows already in the browser, so asking one
// never reads again, and the arrival cue starts again from whatever the
// question shows. A row is a SUMMARY -- no body, no document (memql#6032) --
// which is all a list needs; a template's page reads it whole.
//
// THE LAYOUT'S NAME comes from the organizations' layout readings the section
// holds (`useEmailLayouts`), the same readings the Layouts view lists: a
// template carries the opaque id of the layout it was made from, and only the
// reading can name it. A template made without one is "Custom".

/** The states a question can ask for; archived only where archived templates are listed. */
export type StateFacet = "" | "draft" | "ready" | "archived";

/** What is being asked of the templates list. */
export interface TemplatesQuestion {
  search: string;
  /** The organization asked about, or "" for every one. */
  accountId: string;
  state: StateFacet;
}

export const EVERY_TEMPLATE: TemplatesQuestion = { search: "", accountId: "", state: "" };

/** Whether a question narrows the list at all. */
export function narrowsTemplates(question: TemplatesQuestion): boolean {
  return question.search.trim() !== "" || question.accountId !== "" || question.state !== "";
}

/** The templates listed before any question: every one, archived ones only when Settings says so. */
export function templatesListed(rows: readonly Row[], includeArchived: boolean): TemplateRow[] {
  return rows.map(templateFromRow).filter((t) => t.id !== "" && (includeArchived || !templateIsArchived(t)));
}

/** The templates a question asks for, from the rows the feed holds: by name and subject, organization and state. */
export function templatesAsked(rows: readonly Row[], question: TemplatesQuestion, includeArchived: boolean): TemplateRow[] {
  const needle = question.search.trim().toLowerCase();
  return templatesListed(rows, includeArchived).filter(
    (t) =>
      (question.accountId === "" || t.accountId === question.accountId) &&
      (question.state === "" || (question.state === "ready" ? isPublished(t) : t.status === question.state)) &&
      (needle === "" || templateName(t).toLowerCase().includes(needle) || t.subject.toLowerCase().includes(needle)),
  );
}

/** The organizations a list of templates belongs to, each once, in the order they come. */
export function organizationsOf(templates: readonly { accountId: string }[]): string[] {
  return [...new Set(templates.map((t) => t.accountId).filter((id) => id !== ""))];
}

/** The question's controls, behind the Head's one Refine (DESIGN.md rule 2). */
export function TemplatesRefine({ question, onQuestion, organizations, includeArchived }: {
  question: TemplatesQuestion;
  onQuestion: (next: TemplatesQuestion) => void;
  /** The organizations listed, by id and name. A facet only when there is more than one. */
  organizations: readonly { id: string; name: string }[];
  includeArchived: boolean;
}) {
  const states: readonly { value: StateFacet; label: string }[] = [
    { value: "", label: "Any state" },
    { value: "draft", label: WORDS.template.draft },
    { value: "ready", label: WORDS.template.ready },
    ...(includeArchived ? [{ value: "archived" as const, label: WORDS.template.archived }] : []),
  ];
  const chips: RefineChip[] = [];
  if (question.accountId !== "") {
    const name = organizations.find((organization) => organization.id === question.accountId)?.name ?? "";
    chips.push({ id: "organization", label: name || "Organization", onRemove: () => onQuestion({ ...question, accountId: "" }) });
  }
  if (question.state !== "") {
    chips.push({ id: "state", label: templateState(question.state), onRemove: () => onQuestion({ ...question, state: "" }) });
  }
  return (
    <Refine iconOnly search={question.search} onSearch={(search) => onQuestion({ ...question, search })} chips={chips} label="Refine templates">
      {organizations.length > 1 ? (
        <Select id="email-templates-organization" label="Organization" value={question.accountId} onChange={(accountId) => onQuestion({ ...question, accountId })}>
          <option value="">Any organization</option>
          {organizations.map((organization) => (
            <option key={organization.id} value={organization.id}>
              {organization.name}
            </option>
          ))}
        </Select>
      ) : null}
      <Select id="email-templates-state" label="State" value={question.state} onChange={(state) => onQuestion({ ...question, state: state as StateFacet })}>
        {states.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </Select>
    </Refine>
  );
}

export function TemplatesView({ source, listKey, readings, layoutsSettled, organizationName, error, narrowed, archivedHidden, missing, onRetry, onOpen }: {
  source: LiveListSource<TemplateRow> | null;
  /** The question, written down: a new one re-baselines the arrival cue. */
  listKey: string;
  /** The organizations' layout readings, which name each template's layout. */
  readings: ReadonlyMap<string, readonly LayoutRow[]>;
  /** Every organization's layouts have been read once: a name not found now is not coming. */
  layoutsSettled: boolean;
  /** An organization's name, or "" where the rows shown are all one organization's (said once, by the chip or not at all). */
  organizationName: (accountId: string) => string;
  /** The engine's sentence when the templates could not be read. */
  error: string;
  narrowed: boolean;
  /** Templates the cluster holds are archived and Settings hides them: an empty list says so, and where. */
  archivedHidden: boolean;
  /** A template somebody asked for that the cluster does not hold for this person. */
  missing: boolean;
  onRetry: () => void;
  onOpen: (templateId: string) => void;
}) {
  return (
    <>
      {missing ? <Notice tone="info" sentence={WORDS.unavailable.template} /> : null}
      {/* Keyed on the question, so asking another one RE-BASELINES the arrival
          cues: rows the browser already had are not the cluster sending them. */}
      <LiveList<TemplateRow>
        key={listKey}
        source={source}
        rowId={(t) => t.id}
        fingerprint={templateFingerprint}
        label="Templates"
        errorContent={
          <Notice tone="error" sentence="This cluster did not return your templates." next="Nothing below is current." detail={refusalSentence(error, "templates")}>
            <Button onClick={onRetry}>Try again</Button>
          </Notice>
        }
        emptyContent={
          error !== "" ? null : narrowed ? (
            <EmptyState icon={FileText} title="No matching templates">
              Try another name, organization or state.
            </EmptyState>
          ) : archivedHidden ? (
            // DESIGN.md rule 4: hiding is why it is empty, so the setting is named.
            <EmptyState icon={FileText} title="No templates in use">
              Archived templates are hidden. Turn on Include archived in Settings to see them.
            </EmptyState>
          ) : (
            <EmptyState icon={FileText} title="No templates yet">
              Templates are written in the editor and appear here when they're published.
            </EmptyState>
          )
        }
        renderRow={(template, tick) => {
          const archived = templateIsArchived(template);
          const layout = template.layoutId === "" ? WORDS.customLayout : layoutName(readings, template.layoutId);
          const organization = organizationName(template.accountId);
          return (
            <RecordRow
              icon={<FileText size={16} aria-hidden />}
              name={templateName(template)}
              secondary={template.subject}
              state={templateState(template.status)}
              current={!archived}
              dim={archived}
              onOpen={() => onOpen(template.id)}
              // On arrival only: an edit pulses the row and is not "new".
              stateExtra={tick === "added" ? <span className="os-livelist-tick">new</span> : null}
            >
              {layout !== "" ? <span className="os-caption">{layout}</span> : layoutsSettled ? null : <InlineSkeleton label="Loading the layout" />}
              {organization === "" ? null : <span className="os-caption">{organization}</span>}
            </RecordRow>
          );
        }}
      />
    </>
  );
}
