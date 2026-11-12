import { Button, ContentSkeleton, Details, DetailsSection, Head, Notice } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { ReviewsPane } from "../reviews/ReviewsPane";
import { VersionFacts } from "./DocumentsTab";
import { documentKindWords } from "./model";
import { DocumentText } from "./PublishDocumentPage";
import { useWholesaleDocumentVersion, type WholesaleScope } from "./useWholesale";

// ONE EARLIER VERSION OF A STORE DOCUMENT, READ-ONLY: what an applicant may
// have accepted, as it was published. A version is never changed (the
// documents are append-only), so the page offers nothing to do.

export function DocumentVersionPage({ scope, versionId, listedName, trail, back }: {
  scope: WholesaleScope;
  versionId: string;
  /** The version's label, as the list named it: the title until it is read. */
  listedName: string;
  /** The trail down to the Documents tab: the version is added after it. */
  trail: readonly Breadcrumb[];
  back: { label: string; onSelect: () => void };
}) {
  const reading = useWholesaleDocumentVersion(scope, versionId);
  const version = reading.value;
  const name = version?.label || listedName || "Version";
  const head = <Head navigationKey={`wholesale-document:${versionId}`} title={name} meta={version ? documentKindWords(version.kind) : undefined}
    breadcrumbs={[...trail, { label: name }]} back={back} />;
  const context = { page: "Wholesale document version", versionId };

  if (version === null) {
    return (
      <ReviewsPane heading={head} bar={<ActionBar state="" tone={reading.state === "failed" ? "none" : "busy"} />} context={context}>
        {reading.state === "failed" ? (
          <Notice tone="error" sentence={reading.error}><Button onClick={reading.reread}>Try again</Button></Notice>
        ) : (
          <ContentSkeleton label="Reading the version" kind="detail" />
        )}
      </ReviewsPane>
    );
  }

  return (
    <ReviewsPane heading={head} bar={<ActionBar state="Earlier version" tone="none" />} context={context}>
      <Details>
        <DetailsSection title="Version">
          <VersionFacts version={version} />
        </DetailsSection>
        <DetailsSection title="Text">
          <DocumentText text={version.body} />
        </DetailsSection>
      </Details>
    </ReviewsPane>
  );
}
