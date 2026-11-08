import { useEmailCatalogs } from "../catalogs";
import { useEmailBrowse, useEmailRecord } from "../useBrowse";
import { LoadMore } from "../../../kit/LoadMore";
import { useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { AutoRefresh, Head } from "../../../kit";
import { AddButton } from "../../../kit/AddButton";
import { listCount } from "../../../kit/RecordRow";
import { useLiveView } from "../../../live/liveView";
import { useAccountOptionsFeed } from "../../accounts/tie";
import { recordNamed, useOpenRequest, type OpenRequest } from "../openRequest";
import { organizationName, selectableOrganizations } from "../organizations";
import {
  emailRuleFromRow,
  isWriting,
  journeyFromRow,
  ruleName,
  templateFromRow,
  templateProjection,
  useProjected,
  type TemplateRow,
} from "../rows";
import { useEmailLayouts, type EmailFeeds } from "../useEmail";
import { CreateTemplateWizard } from "./CreateTemplateWizard";
import { TemplatePage } from "./TemplatePage";
import {
  EVERY_TEMPLATE,
  TemplatesRefine,
  TemplatesView,
  narrowsTemplates,
  organizationsOf,
  templatesListed,
  type TemplatesQuestion,
} from "./TemplatesView";

// TEMPLATES: what an email says (memql#6033, Task 11; Ruling 24).
//
// ONE VIEW, SO NO TAB BAR. People no longer make layouts, so the page is the
// templates and nothing beside them. The Head's Refine and Add are the
// templates'; templates are created through the editor (Create new template),
// or written by MemQL in the background.
//
// NOTHING IS WRITTEN HERE. A template is written and published in the editor;
// its page opens the editor, archives or restores, and stops or starts again
// an email MemQL is writing.
//
// THE LAYOUT READINGS ARE THE SECTION'S, and only name things: one
// `emailLayouts` reading per organization (`useEmailLayouts`) names the layout
// each template was made from, kept current at the kit's pace while the
// section is seen.
//
// AN EMAIL BEING WRITTEN IS LIVE NEWS. The templates feed is live, so a row
// that finishes writing changes on the list and on an open page by itself; as
// a safety net, while any template is being written the section reads the
// feed again every few seconds, and stops when none is.
//
// A PAGE REPLACES THE LIST (DESIGN.md rule 11), and so does the wizard. A page
// opens whichever template its id names in the WHOLE feed -- an archived one is
// hidden from the list by default and search and Ask still reach it -- and one
// the cluster does not hold for this person says so in place.

/** How often a template being written is read again, as a safety net beside the live feed. */
export const WRITING_REREAD_MS = 2_000;

export function TemplatesSection({ feeds, includeArchived, openRequest }: {
  feeds: EmailFeeds;
  /** Settings' Include archived: archived templates and layouts are listed. */
  includeArchived: boolean;
  /** A template to open, or Create new template to start, from search, Ask or another page. */
  openRequest?: OpenRequest;
}) {
  const [adding, setAdding] = useState(false);
  // The template whose page is open, and the one somebody ELSE asked for --
  // search, Ask, another page -- which alone can be called missing: one this
  // section opens itself (the wizard's arrival) is on its way.
  const [openId, setOpenId] = useState("");
  const [requested, setRequested] = useState("");
  const [templatesQuestion, setTemplatesQuestion] = useState<TemplatesQuestion>(EVERY_TEMPLATE);
  useEmailCatalogs(adding ? ["templates"] : ["journeys", "rules"], adding || openId !== "");
  const browse = useEmailBrowse("templates", { search: templatesQuestion.search, accountId: templatesQuestion.accountId || undefined, status: templatesQuestion.state || undefined, includeArchived });
  const detail = useEmailRecord("templates", openId);
  // Only an empty unfiltered directory needs this small existence read.
  const archived = useEmailBrowse("templates", { includeArchived: true, status: "archived" }, !includeArchived && !narrowsTemplates(templatesQuestion) && browse.snapshot.state === "live" && !browse.snapshot.error && browse.snapshot.rows.length === 0);

  useOpenRequest(openRequest, (target) => {
    if ("add" in target) {
      setOpenId("");
      setRequested("");
      setAdding(true);
    } else {
      setAdding(false);
      setOpenId(target.id);
      setRequested(target.id);
    }
  });

  // ---- the organizations in view -------------------------------------------
  const { accounts } = useAccountOptionsFeed();
  const organizations = useMemo(() => selectableOrganizations(accounts), [accounts]);
  const layouts = useEmailLayouts(useMemo(() => [...new Set([...browse.snapshot.rows, ...detail.snapshot.rows].map(templateFromRow).map(row => row.accountId).filter(Boolean))], [browse.snapshot.rows, detail.snapshot.rows]));
  const nameOf = (accountId: string) => organizationName(accounts, accountId);

  // ---- templates -------------------------------------------------------------
  const rows = browse.snapshot.rows;
  const allTemplates = useProjected(rows, templateProjection);
  const listedOrganizations = organizations.map(organization => organization.id);
  const needle = templatesQuestion.search.trim().toLowerCase();
  const templatesKey = `${includeArchived}|${templatesQuestion.accountId}|${templatesQuestion.state}|${needle}`;
  const source = useLiveView<Row, TemplateRow>(browse.source, templatesKey, held => held.map(templateFromRow));
  // A row names its organization only where the rows shown are more than one organization's.
  const shownOrganizations = organizationsOf(source?.snapshot.rows ?? []);
  const openTemplate = useMemo(() => recordNamed(detail.snapshot.rows, templateFromRow, openId) ?? recordNamed(rows, templateFromRow, openId), [detail.snapshot.rows, rows, openId]);

  // The active automations that send the open template. Editing it makes it a
  // draft: a series holds its emails until it is published again, and an event
  // automation sends none of its emails meanwhile (they are not sent later).
  const journeyRows = feeds.journeys.snapshot.rows;
  const ruleRows = feeds.rules.snapshot.rows;
  const sendingAutomations = useMemo(
    () => openTemplate === null
      ? undefined
      : {
          held: journeyRows.map(journeyFromRow).filter((journey) => journey.status === "active" && journey.steps.some((step) => step.templateId === openTemplate.id)).map((journey) => journey.name),
          unsent: ruleRows.map(emailRuleFromRow).filter((rule) => rule.status === "active" && rule.templateId === openTemplate.id).map(ruleName),
        },
    [journeyRows, ruleRows, openTemplate],
  );

  function openTemplatePage(id: string) {
    setOpenId(id);
    setRequested("");
  }

  const refresher = (
    <>
      <AutoRefresh onRefresh={layouts.reload} />
      {/* One read in flight at a time (AutoRefresh waits for it); gone when nothing is being written. */}
      <AutoRefresh onRefresh={browse.reseed} resume={false} enabled={allTemplates.some(isWriting)} busy={browse.snapshot.state !== "live"} intervalMs={WRITING_REREAD_MS} />
    </>
  );

  if (adding) {
    return (
      <>
        {refresher}
        <CreateTemplateWizard
          feeds={feeds}
          onBack={() => setAdding(false)}
          onDone={(id) => {
            setAdding(false);
            openTemplatePage(id);
          }}
        />
      </>
    );
  }
  if (openTemplate !== null) {
    return (
      <>
        {refresher}
        <TemplatePage
          key={openTemplate.id}
          template={openTemplate}
          layouts={layouts}
          automations={sendingAutomations}
          onBack={() => {
            setOpenId("");
            setRequested("");
          }}
          onReread={detail.reseed}
        />
      </>
    );
  }
  // ---- the list --------------------------------------------------------------
  const templatesFailed = browse.snapshot.error !== "";
  // A TEMPLATE SOMEBODY ASKED FOR THAT THE CLUSTER DOES NOT HOLD, said in place --
  // only once the cluster has answered: while the feed reads, or when it could
  // not be asked, the template is not "missing", and its page opens if it arrives.
  const templateMissing = requested !== "" && requested === openId && detail.snapshot.state === "live" && !detail.snapshot.error && !openTemplate;
  const templatesNarrowed = narrowsTemplates(templatesQuestion);
  // A list the cluster says is empty has nothing to ask a question of (rule 2).
  const templatesSettledEmpty = browse.snapshot.state === "live" && !templatesFailed && templatesListed(rows, includeArchived).length === 0;
  const organizationChoices = (ids: readonly string[]) => ids.map((id) => ({ id, name: nameOf(id) }));

  return (
    <div className="os-app-stack" data-os-dividers="none">
      {refresher}
      <Head title="Templates" meta={listCount(source?.snapshot)}>
        {templatesSettledEmpty && !templatesNarrowed ? null : (
          <TemplatesRefine
            question={templatesQuestion}
            onQuestion={setTemplatesQuestion}
            organizations={organizationChoices(listedOrganizations)}
            includeArchived={includeArchived}
          />
        )}
        <AddButton
          label="Create new template"
          onClick={() => {
            setRequested("");
            setAdding(true);
          }}
        />
      </Head>
      <TemplatesView
        source={source}
        listKey={templatesKey}
        readings={layouts.readings}
        layoutsSettled={layouts.settled}
        organizationName={(accountId) => (shownOrganizations.length > 1 ? nameOf(accountId) : "")}
        error={browse.snapshot.error}
        narrowed={templatesNarrowed}
        archivedHidden={!includeArchived && archived.snapshot.rows.length > 0}
        missing={templateMissing}
        onRetry={browse.reseed}
        onOpen={openTemplatePage}
      />
      <LoadMore feed={browse} />
    </div>
  );
}
