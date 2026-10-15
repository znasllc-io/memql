import { useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { AutoRefresh, Head } from "../../../kit";
import { AddButton } from "../../../kit/AddButton";
import { LocalTabs } from "../../../kit/LocalTabs";
import { listCount } from "../../../kit/RecordRow";
import { useLiveView } from "../../../live/liveView";
import { useAccountOptionsFeed } from "../../accounts/tie";
import { recordNamed, useOpenRequest, type OpenRequest } from "../openRequest";
import { organizationName, selectableOrganizations } from "../organizations";
import {
  layoutIsArchived,
  layoutsInView,
  templateFromRow,
  templateIsArchived,
  templateProjection,
  useProjected,
  type LayoutRow,
  type TemplateRow,
} from "../rows";
import { useEmailLayouts, type EmailFeeds } from "../useEmail";
import { CreateTemplateWizard } from "./CreateTemplateWizard";
import { LayoutPage } from "./LayoutPage";
import { EVERY_LAYOUT, LayoutsRefine, LayoutsView, layoutsAsked, type LayoutsQuestion } from "./LayoutsView";
import { TemplatePage } from "./TemplatePage";
import {
  EVERY_TEMPLATE,
  TemplatesRefine,
  TemplatesView,
  narrowsTemplates,
  organizationsOf,
  templatesAsked,
  templatesListed,
  type TemplatesQuestion,
} from "./TemplatesView";

// TEMPLATES: what an email says, and the layouts it is made from (memql#6033,
// Task 11; Ruling 24).
//
// ONE HEAD, TWO SIBLING VIEWS. Templates and layouts are two kinds of thing a
// person looks at here, so they are `LocalTabs` directly under the Head --
// views inside the page, not two app tabs. The Head's Refine and its Add follow
// the open view: templates are created through the editor (Create new
// template), and layouts are saved from it, so the Layouts view has no Add.
//
// NOTHING IS WRITTEN HERE. A template is written and published in the editor;
// its page opens the editor and archives or restores. A layout is saved from a
// finished email in the editor; its page archives or restores.
//
// THE LAYOUT READINGS ARE THE SECTION'S: one `emailLayouts` reading per
// organization (`useEmailLayouts`), which lists the Layouts view and names the
// layout each template was made from, kept current at the kit's pace while the
// section is seen.
//
// A PAGE REPLACES THE LIST (DESIGN.md rule 11), and so does the wizard. A page
// opens whichever template its id names in the WHOLE feed -- an archived one is
// hidden from the list by default and search and Ask still reach it -- and one
// the cluster does not hold for this person says so in place.

type View = "templates" | "layouts";

const VIEWS: readonly (readonly [View, string])[] = [
  ["templates", "Templates"],
  ["layouts", "Layouts"],
];

export function TemplatesSection({ feeds, includeArchived, openRequest }: {
  feeds: EmailFeeds;
  /** Settings' Include archived: archived templates and layouts are listed. */
  includeArchived: boolean;
  /** A template to open, or Create new template to start, from search, Ask or another page. */
  openRequest?: OpenRequest;
}) {
  const [view, setView] = useState<View>("templates");
  const [adding, setAdding] = useState(false);
  // The template whose page is open, and the one somebody ELSE asked for --
  // search, Ask, another page -- which alone can be called missing: one this
  // section opens itself (the wizard's arrival) is on its way.
  const [openId, setOpenId] = useState("");
  const [requested, setRequested] = useState("");
  // The same pair for a layout: a template's page asks for the layout it was made from.
  const [layoutId, setLayoutId] = useState("");
  const [layoutRequested, setLayoutRequested] = useState("");
  const [templatesQuestion, setTemplatesQuestion] = useState<TemplatesQuestion>(EVERY_TEMPLATE);
  const [layoutsQuestion, setLayoutsQuestion] = useState<LayoutsQuestion>(EVERY_LAYOUT);

  useOpenRequest(openRequest, (target) => {
    setLayoutId("");
    setLayoutRequested("");
    setView("templates");
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
  const { accounts, state: accountsState } = useAccountOptionsFeed();
  const organizations = useMemo(() => selectableOrganizations(accounts), [accounts]);
  const layouts = useEmailLayouts(useMemo(() => organizations.map((organization) => organization.id), [organizations]));
  const nameOf = (accountId: string) => organizationName(accounts, accountId);

  // ---- templates -------------------------------------------------------------
  const rows = feeds.templates.snapshot.rows;
  const allTemplates = useProjected(rows, templateProjection);
  const listedOrganizations = useMemo(() => organizationsOf(templatesListed(rows, includeArchived)), [rows, includeArchived]);
  const needle = templatesQuestion.search.trim().toLowerCase();
  const templatesKey = `${includeArchived}|${templatesQuestion.accountId}|${templatesQuestion.state}|${needle}`;
  const source = useLiveView<Row, TemplateRow>(feeds.templates.source, templatesKey, (held) => templatesAsked(held, templatesQuestion, includeArchived));
  // A row names its organization only where the rows shown are more than one organization's.
  const shownOrganizations = organizationsOf(source?.snapshot.rows ?? []);
  const openTemplate = useMemo(() => recordNamed(rows, templateFromRow, openId), [rows, openId]);

  // ---- layouts ---------------------------------------------------------------
  // Every layout the readings hold, archived ones included: a page opens
  // whichever one its id names, whatever the list shows.
  const everyLayout = useMemo(() => layoutsInView(layouts.readings, true), [layouts.readings]);
  const listedLayouts = useMemo(() => layoutsInView(layouts.readings, includeArchived), [layouts.readings, includeArchived]);
  const shownLayouts = layoutsAsked(listedLayouts, layoutsQuestion);
  const ownOf = (layout: LayoutRow) => layout.kind === "organization" && (layoutsQuestion.accountId === "" || layout.accountId === layoutsQuestion.accountId);
  const ownInView = listedLayouts.some(ownOf);
  const layoutsArchivedHidden = !includeArchived && everyLayout.some((layout) => ownOf(layout) && layoutIsArchived(layout));
  const openLayout = layoutId === "" ? null : (everyLayout.find((layout) => layout.id === layoutId) ?? null);

  function openTemplatePage(id: string) {
    setOpenId(id);
    setRequested("");
  }

  function openLayoutPage(layout: LayoutRow) {
    setLayoutId(layout.id);
    setLayoutRequested("");
  }

  const refresher = <AutoRefresh onRefresh={layouts.reload} />;

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
          onBack={() => {
            setOpenId("");
            setRequested("");
          }}
          onOpenLayout={(id) => {
            // The layout's page replaces the template's, and its way back is the layouts.
            setOpenId("");
            setRequested("");
            setView("layouts");
            setLayoutId(id);
            setLayoutRequested(id);
          }}
          onReread={feeds.templates.reseed}
        />
      </>
    );
  }
  if (openLayout !== null) {
    return (
      <>
        {refresher}
        <LayoutPage
          key={openLayout.id}
          layout={openLayout}
          layouts={layouts}
          templates={allTemplates}
          onBack={() => {
            setLayoutId("");
            setLayoutRequested("");
            setView("layouts");
          }}
        />
      </>
    );
  }

  // ---- the list --------------------------------------------------------------
  const templatesFailed = feeds.templates.snapshot.error !== "";
  // A TEMPLATE SOMEBODY ASKED FOR THAT THE CLUSTER DOES NOT HOLD, said in place --
  // only once the cluster has answered: while the feed reads, or when it could
  // not be asked, the template is not "missing", and its page opens if it arrives.
  const templateMissing = requested !== "" && requested === openId && feeds.templates.snapshot.state === "live" && !templatesFailed;
  // The same for a layout, once every organization's reading has answered without a refusal.
  const layoutMissing = layoutRequested !== "" && layoutRequested === layoutId && layouts.settled && layouts.refusals.size === 0;
  const templatesNarrowed = narrowsTemplates(templatesQuestion);
  // A list the cluster says is empty has nothing to ask a question of (rule 2).
  const templatesSettledEmpty = feeds.templates.snapshot.state === "live" && !templatesFailed && templatesListed(rows, includeArchived).length === 0;
  const layoutsNarrowed = layoutsQuestion.search.trim() !== "" || layoutsQuestion.accountId !== "";
  const layoutsCount = layouts.settled && layouts.refusals.size === 0 ? shownLayouts.length : undefined;
  const organizationChoices = (ids: readonly string[]) => ids.map((id) => ({ id, name: nameOf(id) }));

  return (
    <div className="os-app-stack">
      {refresher}
      <Head title="Templates" meta={view === "templates" ? listCount(source?.snapshot) : layoutsCount}>
        {view === "templates" ? (
          <>
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
          </>
        ) : layouts.readings.size === 0 && !layoutsNarrowed ? null : (
          <LayoutsRefine question={layoutsQuestion} onQuestion={setLayoutsQuestion} organizations={organizationChoices(layouts.accountIds)} />
        )}
      </Head>
      <LocalTabs
        label="Templates and layouts"
        value={view}
        onChange={(next) => {
          // A sentence about a record somebody asked for goes with the view it was said in.
          setRequested("");
          setLayoutRequested("");
          setView(next);
        }}
        options={VIEWS}
      />
      {view === "templates" ? (
        <TemplatesView
          source={source}
          listKey={templatesKey}
          readings={layouts.readings}
          layoutsSettled={layouts.settled}
          organizationName={(accountId) => (shownOrganizations.length > 1 ? nameOf(accountId) : "")}
          error={feeds.templates.snapshot.error}
          narrowed={templatesNarrowed}
          archivedHidden={!includeArchived && allTemplates.some(templateIsArchived)}
          missing={templateMissing}
          onRetry={feeds.templates.reseed}
          onOpen={openTemplatePage}
        />
      ) : (
        <LayoutsView
          layouts={layouts}
          shown={shownLayouts}
          ownInView={ownInView}
          archivedHidden={layoutsArchivedHidden}
          narrowed={layoutsNarrowed}
          organizationName={nameOf}
          missing={layoutMissing}
          readingOrganizations={accountsState === "seeding"}
          onOpen={openLayoutPage}
        />
      )}
    </div>
  );
}
