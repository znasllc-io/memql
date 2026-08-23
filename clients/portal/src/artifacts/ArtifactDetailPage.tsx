import type { ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { rowString } from "@znasllc-io/memql-sdk-core/client";

import { Empty, ErrorMessage } from "../components/StatusMessage";
import { Band, Breadcrumbs, Container, DataText, LabelChips, PageHeader, Skeleton } from "../ui";
import { artifactsPath } from "./urls";
import { useArtifactDetail } from "./useArtifacts";

// One artifact's detail + label editor (task 4 of the artifacts-labels
// epic): the Library index row's own fields, and LabelChips wired to the
// two builtins that add/remove one label at a time.
//
// LABELS ARE THE ONE PIECE OF NEW UI HERE. Everything else about the row
// (title, kind, source, validation) is read straight off the index row --
// there is no per-field renderer to keep in step with the concept's schema,
// because the concept only has 17-odd fields and this page reads the ones
// an operator actually orients on, the same editorial choice
// SiteDetailPage.tsx makes for v1:platform:site rather than dumping every
// field through a generic key/value walk.
export function ArtifactDetailPage(): ReactNode {
  const { artifactId = "" } = useParams<{ artifactId: string }>();
  const detail = useArtifactDetail(artifactId);

  if (detail.error) {
    return <ErrorMessage>Could not read this artifact: {detail.error}</ErrorMessage>;
  }
  if (detail.loading && detail.artifact === null) {
    return <Skeleton variant="kv" rows={6} />;
  }
  if (detail.artifact === null) {
    return (
      <Empty>
        No artifact has that id. It may have been deleted, or the link may name an artifact from
        another cluster.{" "}
        <Link to={artifactsPath()} className="text-accent underline">
          Back to artifacts
        </Link>
      </Empty>
    );
  }

  const artifact = detail.artifact;
  const title = rowString(artifact, "title");
  const summary = rowString(artifact, "summary");
  const kind = rowString(artifact, "kind");
  const source = rowString(artifact, "source");
  const lens = rowString(artifact, "lens");
  const format = rowString(artifact, "format");
  const validationStatus = rowString(artifact, "validationStatus");
  const updatedAt = rowString(artifact, "updatedAt");
  const sourceConceptRef = rowString(artifact, "sourceConceptRef");
  const producedByWorkerName = rowString(artifact, "producedByWorkerName");

  return (
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          eyebrow={
            <Breadcrumbs items={[{ label: "Artifacts", to: artifactsPath() }, { label: title || artifactId }]} />
          }
          title={title || artifactId}
          blurb={
            <>
              <DataText kind="id">{sourceConceptRef}</DataText> · {kind || "unknown kind"} ·{" "}
              {lens || "unknown lens"} · source {source || "unknown"}
            </>
          }
        />

        {summary ? <p className="max-w-2xl text-sm text-muted">{summary}</p> : null}

        <Band title="Labels" meta="free-text -- yours, or added by an agent you talk to">
          {/* The live-region announcement lives HERE, at the page level, not
              inside LabelChips itself -- role="status" is a page-level
              convention in this codebase (WriteOutcome.tsx,
              SidebarProfile.tsx), and LabelChips is a shared primitive with
              no page context of its own. Visually hidden: the chip that
              changed is already visible feedback for a sighted user: this is
              only for the screen-reader announcement a silent chip update
              would otherwise skip. */}
          <span role="status" className="sr-only">
            {detail.announcement}
          </span>
          <LabelChips
            labels={detail.labels}
            onAdd={detail.addLabel}
            onRemove={detail.removeLabel}
            busy={detail.labelBusy}
          />
          {detail.labelError ? (
            <p className="mt-2">
              <ErrorMessage>{detail.labelError}</ErrorMessage>
            </p>
          ) : null}
        </Band>

        <Band title="Details">
          <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1.5 text-sm">
            <dt className="text-muted">Format</dt>
            <dd className="text-fg">{format || "—"}</dd>
            <dt className="text-muted">Validation</dt>
            <dd className="text-fg">{validationStatus || "none"}</dd>
            {producedByWorkerName ? (
              <>
                <dt className="text-muted">Produced by</dt>
                <dd className="text-fg">{producedByWorkerName}</dd>
              </>
            ) : null}
            <dt className="text-muted">Updated</dt>
            <dd>
              <DataText kind="time">{updatedAt || "—"}</DataText>
            </dd>
          </dl>
        </Band>
      </section>
    </Container>
  );
}
