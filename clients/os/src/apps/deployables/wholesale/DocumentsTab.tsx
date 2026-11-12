import type { ReactNode } from "react";
import { FileText } from "lucide-react";

import { Button, ContentSkeleton, Details, DetailsSection, Fact, Facts, Notice } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import { AddButton } from "../../../kit/AddButton";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { ReviewsPane } from "../reviews/ReviewsPane";
import { dayWords, DOCUMENT_KINDS, fingerprint, isNotSetUp, NOT_SET_UP, type DocumentKind, type DocumentKindView, type DocumentVersion, type WholesaleSettings } from "./model";
import { useWholesaleDocuments, type WholesaleScope, type WholesaleWrites } from "./useWholesale";

// THE STORE'S THREE DOCUMENTS -- its terms, its MAP policy and its price sheet
// (wholesaleDocuments) -- each a section: the current version's label, when
// and by whom it was published and the first eight characters of its
// fingerprint, then every earlier version beneath, newest first, each opening
// read-only. APPEND-ONLY: a document is changed by publishing a new version,
// the section heading's one Add control, and nothing here edits a version.
//
// WHO MAY PUBLISH IS THE ENGINE'S: the settings' `may.update`, the engine's
// answer for who changes this store's program. The documents' own read
// answers no `may`.

export function DocumentsTab({ scope, heading, writes, settings, onPublish, onOpenVersion }: {
  scope: WholesaleScope;
  /** The Wholesale heading and its tabs. */
  heading: ReactNode;
  writes: WholesaleWrites;
  /** The store's settings as last read: who may publish. */
  settings: WholesaleSettings | null;
  onPublish: (kind: DocumentKind) => void;
  onOpenVersion: (versionId: string, label: string) => void;
}) {
  const reading = useWholesaleDocuments(scope, writes.revision);
  const context = { page: "Wholesale", view: "Documents", siteId: scope.siteId, storeId: scope.storeId };

  if (reading.state === "failed" && isNotSetUp(reading.error)) {
    return (
      <ReviewsPane heading={heading} bar={<ActionBar state="Not set up" tone="none" />} context={context}>
        <Notice sentence={NOT_SET_UP} />
      </ReviewsPane>
    );
  }

  const kinds = reading.value;
  if (kinds === null) {
    return (
      <ReviewsPane heading={heading} bar={<ActionBar state="" tone={reading.state === "failed" ? "none" : "busy"} />} context={context}>
        {reading.state === "failed" ? (
          <Notice tone="error" sentence={reading.error}><Button onClick={reading.reread}>Try again</Button></Notice>
        ) : (
          <ContentSkeleton label="Reading the documents" kind="detail" />
        )}
      </ReviewsPane>
    );
  }

  const mayPublish = settings?.may.update === true;
  const published = kinds.filter((k) => k.current !== null).length;
  const bar = <ActionBar state={published === kinds.length ? "Every document published" : `${published} of ${kinds.length} published`} tone={published === kinds.length ? "live" : "paused"} />;

  return (
    <ReviewsPane heading={heading} bar={bar} context={context}>
      <Details>
        {kinds.map((view) => (
          <DocumentSection key={view.kind} view={view} title={DOCUMENT_KINDS.find(([k]) => k === view.kind)?.[1] ?? view.kind}
            onPublish={mayPublish ? () => onPublish(view.kind) : undefined} onOpenVersion={onOpenVersion} />
        ))}
      </Details>
    </ReviewsPane>
  );
}

function DocumentSection({ view, title, onPublish, onOpenVersion }: {
  view: DocumentKindView;
  title: string;
  /** Absent for a person the engine lets publish nothing. */
  onPublish: (() => void) | undefined;
  onOpenVersion: (versionId: string, label: string) => void;
}) {
  const { current } = view;
  const earlier = current === null ? view.history : view.history.filter((v) => v.id !== current.id);
  return (
    <DetailsSection title={title} actions={onPublish ? <AddButton label="Publish a new version" onClick={onPublish} /> : undefined}>
      {current === null ? (
        <p className="os-wholesale-line">Not published. Applications cannot open while terms are required.</p>
      ) : (
        <VersionFacts version={current} />
      )}
      {earlier.length > 0 ? (
        <RecordList as="ul" density="compact" label="Earlier versions">
          {earlier.map((v) => (
            <RecordRow key={v.id} icon={<FileText size={18} aria-hidden />} name={v.label} label={`Open ${v.label}`}
              trailing={<span className="os-caption">{dayWords(v.publishedAt)}</span>}
              onOpen={() => onOpenVersion(v.id, v.label)}>
              <span className="os-mono">{fingerprint(v.sha256)}</span>
            </RecordRow>
          ))}
        </RecordList>
      ) : null}
    </DetailsSection>
  );
}

/** One version's facts: its label, when and by whom it was published, and its fingerprint's first eight characters. */
export function VersionFacts({ version }: { version: DocumentVersion }) {
  return (
    <Facts>
      <Fact label="Label" value={version.label} copyValue={version.label} />
      <Fact label="Published on" value={dayWords(version.publishedAt)} copyValue={version.publishedAt || undefined} />
      <Fact label="Published by" value={version.publishedByName} />
      <Fact label="Fingerprint" value={<span className="os-mono">{fingerprint(version.sha256)}</span>} copyValue={version.sha256 || undefined} />
    </Facts>
  );
}
