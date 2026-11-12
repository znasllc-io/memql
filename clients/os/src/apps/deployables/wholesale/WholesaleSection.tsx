import { useCallback, useState, type ReactNode } from "react";

import { Head } from "../../../kit";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { LocalTabs } from "../../../kit/LocalTabs";
import type { SiteRow } from "../rows";
import { AddBuyerPage } from "./AddBuyerPage";
import { ApplicationsTab } from "./ApplicationsTab";
import { ApplicationPage } from "./ApplicationPage";
import { BuyersTab } from "./BuyersTab";
import { DocumentsTab } from "./DocumentsTab";
import { DocumentVersionPage } from "./DocumentVersionPage";
import { type DocumentKind, type WholesaleFilter } from "./model";
import { PublishDocumentPage } from "./PublishDocumentPage";
import { WholesaleSettingsPage } from "./WholesaleSettingsPage";
import { useWholesaleSettings, useWholesaleWrites, type WholesaleScope } from "./useWholesale";
import "./wholesale.css";

// A STORE'S WHOLESALE PROGRAM, as a section of its deployable's page, mounted
// the way Reviews is: the heading, then `LocalTabs`, then the view. The tabs
// are the program's nouns: the Applications the store's people decide, the
// Buyers they approved, the Documents an applicant accepts, and the Settings
// the store takes them by. One application, a new buyer, a new version of a
// document and an earlier version are each a page of their own under the tab
// they were opened from, so the row's Back and the browser's Back both step
// out of them.
//
// THE CLUSTER'S OPERATORS READ EVERY STORE'S APPLICATIONS AND DECIDE NONE.
// The list says so once, from the engine's own answer (`operator` on the
// list's reply, and on the buyers'); one application draws only the acts the
// engine answers, and an operator is answered no decision and adds no buyer.

type Tab = "applications" | "buyers" | "documents" | "settings";

const TABS: readonly (readonly [Tab, string])[] = [
  ["applications", "Applications"],
  ["buyers", "Buyers"],
  ["documents", "Documents"],
  ["settings", "Settings"],
];

const tabName = (tab: Tab): string => TABS.find(([id]) => id === tab)?.[1] ?? "";

/** A page opened from a tab: it stands in the tab's place under the trail. */
type Page =
  | { kind: "application"; id: string; name: string }
  | { kind: "addBuyer" }
  | { kind: "publish"; document: DocumentKind }
  | { kind: "version"; id: string; name: string };

export interface WholesaleSectionProps {
  site: SiteRow;
  storeId: string;
  /** The trail down to the deployable: Wholesale is added after it. */
  trail: readonly Breadcrumb[];
  /** Back out of Wholesale, to the deployable. */
  back: { label: string; onSelect: () => void };
}

export function WholesaleSection({ site, storeId, trail, back }: WholesaleSectionProps) {
  const scope: WholesaleScope = { siteId: site.id, storeId };
  const [tab, setTab] = useState<Tab>("applications");
  const [page, setPage] = useState<Page | null>(null);
  // What the list is asked survives a visit to one application and back.
  const [stage, setStage] = useState<WholesaleFilter>("readyToReview");
  const [search, setSearch] = useState("");
  const writes = useWholesaleWrites(scope);
  // THE SETTINGS ARE READ AGAIN AFTER A SETTINGS WRITE THAT WAS NOT REFUSED,
  // and when the person asks: a save refused because the settings changed
  // meanwhile keeps the draft on screen until they reload.
  const [settingsRevision, setSettingsRevision] = useState(0);
  const settings = useWholesaleSettings(scope, settingsRevision);
  const readAgain = useCallback(() => setSettingsRevision((n) => n + 1), []);

  const show = (next: Tab) => {
    writes.clearRefusal();
    setPage(null);
    setTab(next);
  };
  const toTab = () => {
    writes.clearRefusal();
    setPage(null);
  };
  const open = (next: Page) => {
    writes.clearRefusal();
    setPage(next);
  };

  // The trail down to the tab a page was opened from.
  const tabTrail: Breadcrumb[] = [...trail, { label: "Wholesale", onSelect: () => show("applications") }, { label: tabName(tab), onSelect: toTab }];
  const toTabBack = { label: tabName(tab), onSelect: toTab };

  if (page?.kind === "application") {
    return <ApplicationPage
      key={page.id}
      applicationId={page.id}
      listedName={page.name}
      writes={writes}
      trail={tabTrail}
      back={toTabBack} />;
  }
  // Add buyer is offered only once the settings are read, so they are here.
  if (page?.kind === "addBuyer" && settings.value !== null) {
    return <AddBuyerPage scope={scope} settings={settings.value} writes={writes} trail={tabTrail} back={toTabBack}
      onAdded={(id, name) => open({ kind: "application", id, name })} />;
  }
  if (page?.kind === "publish") {
    return <PublishDocumentPage scope={scope} kind={page.document} writes={writes} trail={tabTrail} back={toTabBack}
      onPublished={toTab} />;
  }
  if (page?.kind === "version") {
    return <DocumentVersionPage scope={scope} versionId={page.id} listedName={page.name} trail={tabTrail} back={toTabBack} />;
  }

  // A store that is not set up is not "closed": Settings says it is not set up.
  const closed = settings.value !== null && settings.value.exists && !settings.value.applicationsOpen;
  const termsRequired = settings.value?.termsRequired === true;
  // The deployable's crumb leads back to it; the Wholesale crumb leads to the
  // first view, and stays plain while that view is the page. `add` is the
  // Head's one creation control; `actions` stand beside the tabs.
  // THE HEAD KEEPS ONE HEIGHT, its Add control or none (`os-wholesale-head`),
  // so the tabs beneath it stand still from one tab to the next.
  const heading = (actions?: ReactNode, add?: ReactNode) => <>
    <div className="os-wholesale-head">
      <Head navigationKey={`wholesale:${site.id}:${tab}`} title="Wholesale"
        meta={closed ? "Closed to new applications" : undefined}
        breadcrumbs={[...trail, { label: "Wholesale", onSelect: tab === "applications" ? undefined : () => show("applications") }, { label: tabName(tab) }]}
        back={back}>{add}</Head>
    </div>
    <LocalTabs label="Wholesale views" value={tab} onChange={show} options={TABS} actions={actions} />
  </>;
  const openApplication = (id: string, name: string) => open({ kind: "application", id, name });

  switch (tab) {
    case "applications":
      return <ApplicationsTab scope={scope} heading={heading} writes={writes} termsRequired={termsRequired}
        stage={stage} onStage={setStage} search={search} onSearch={setSearch} onOpen={openApplication} />;
    case "buyers":
      return <BuyersTab scope={scope} heading={heading} writes={writes} settings={settings.value}
        onOpen={openApplication} onAdd={() => open({ kind: "addBuyer" })} />;
    case "documents":
      return <DocumentsTab scope={scope} heading={heading()} writes={writes} settings={settings.value}
        onPublish={(document) => open({ kind: "publish", document })} onOpenVersion={(id, name) => open({ kind: "version", id, name })} />;
    default:
      return <WholesaleSettingsPage heading={heading()} settings={settings} writes={writes} onWritten={readAgain} />;
  }
}
