import { useCallback, useState, type ReactNode } from "react";

import { Head } from "../../../kit";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { LocalTabs } from "../../../kit/LocalTabs";
import { useSessionIfPresent } from "../../../chrome/access";
import type { SiteRow } from "../rows";
import { ApplicationsTab } from "./ApplicationsTab";
import { ApplicationPage } from "./ApplicationPage";
import type { WholesaleFilter } from "./model";
import { WholesaleSettingsPage } from "./WholesaleSettingsPage";
import { useWholesaleSettings, useWholesaleWrites, type WholesaleScope } from "./useWholesale";

// A STORE'S WHOLESALE APPLICATIONS, as a section of its deployable's page,
// mounted the way Reviews is: the heading, then `LocalTabs`, then the view.
// Applications is what the store's people decide; Settings is how the store
// takes them. One application is a page of its own under the Applications
// tab, so the row's Back and the browser's Back both step out of it.
//
// THE CLUSTER'S OPERATORS READ EVERY STORE'S APPLICATIONS AND DECIDE NONE.
// The list says so once (an operator is the staff-wide reader the session
// names, `everyAccount`, as the Users app reads it); one application draws
// only the acts the engine answers, and an operator is answered no decision.

type Tab = "applications" | "settings";

const TABS: readonly (readonly [Tab, string])[] = [
  ["applications", "Applications"],
  ["settings", "Settings"],
];

const tabName = (tab: Tab): string => TABS.find(([id]) => id === tab)?.[1] ?? "";

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
  const [open, setOpen] = useState<{ id: string; name: string } | null>(null);
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
  const operator = useSessionIfPresent()?.access?.everyAccount === true;

  const show = (next: Tab) => {
    writes.clearRefusal();
    setOpen(null);
    setTab(next);
  };
  const toList = () => {
    writes.clearRefusal();
    setOpen(null);
  };

  if (open !== null) {
    return <ApplicationPage
      key={open.id}
      applicationId={open.id}
      listedName={open.name}
      writes={writes}
      trail={[...trail, { label: "Wholesale", onSelect: () => show("applications") }, { label: tabName(tab), onSelect: toList }]}
      back={{ label: tabName(tab), onSelect: toList }} />;
  }

  // A store that is not set up is not "closed": Settings says it is not set up.
  const closed = settings.value !== null && settings.value.exists && !settings.value.applicationsOpen;
  // The deployable's crumb leads back to it; the Wholesale crumb leads to the
  // first view, and stays plain while that view is the page.
  const heading = (actions?: ReactNode) => <>
    <Head navigationKey={`wholesale:${site.id}:${tab}`} title="Wholesale"
      meta={closed ? "Closed to new applications" : undefined}
      breadcrumbs={[...trail, { label: "Wholesale", onSelect: tab === "applications" ? undefined : () => show("applications") }, { label: tabName(tab) }]}
      back={back} />
    <LocalTabs label="Wholesale views" value={tab} onChange={show} options={TABS} actions={actions} />
  </>;

  return tab === "applications"
    ? <ApplicationsTab scope={scope} heading={heading} writes={writes} operator={operator}
        stage={stage} onStage={setStage} search={search} onSearch={setSearch} onOpen={(id, name) => setOpen({ id, name })} />
    : <WholesaleSettingsPage heading={heading()} settings={settings} writes={writes} onWritten={readAgain} />;
}
