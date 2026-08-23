import { useState, type FormEvent, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { rowNumber, rowString } from "@znasllc-io/memql-sdk-core/client";

import { Empty, ErrorMessage } from "../components/StatusMessage";
import {
  Band,
  Breadcrumbs,
  Button,
  ButtonLink,
  Callout,
  ConfirmDialog,
  Container,
  DataText,
  Field,
  LabelChips,
  PageHeader,
  Skeleton,
} from "../ui";
import { Download, GraduationCap } from "../ui/icons";
import { artifactContentUrl } from "./transport";
import { artifactsPath } from "./urls";
import { isFileArtifact, useArtifactDetail } from "./useArtifacts";

// One artifact's detail: the Library index row's own fields, the label
// editor, the export link, the backing file when there is one (with the
// training control), and archive behind a confirmation.
//
// LABELS AND TRAINING ARE THE TWO PIECES OF NEW UI. Everything else about the
// row (title, kind, source, validation) is read straight off the index row --
// there is no per-field renderer to keep in step with the concept's schema,
// because this page reads the fields an operator actually orients on, the
// same editorial choice SiteDetailPage.tsx makes for v1:platform:site rather
// than dumping every field through a generic key/value walk.
//
// EXPORT IS OFFERED FOR EVERY ARTIFACT, not only for files. GET
// /artifacts/{id}/content streams a file's bytes AND renders a note, a
// generated output or a memory as a download with a filename derived from its
// title (design D9: one route exports the whole Library). A page that showed
// the link only for kind=file would be hiding a working route.
export function ArtifactDetailPage(): ReactNode {
  const { artifactId = "" } = useParams<{ artifactId: string }>();
  const detail = useArtifactDetail(artifactId);
  const [confirmArchive, setConfirmArchive] = useState(false);

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
          actions={
            <div className="flex items-center gap-2">
              <ButtonLink size="xs" href={artifactContentUrl(artifactId)}>
                <Download size={14} aria-hidden="true" />
                Export
              </ButtonLink>
              <Button size="xs" onClick={() => setConfirmArchive(true)} disabled={detail.archived}>
                Archive
              </Button>
            </div>
          }
        />

        {detail.archived ? (
          <Callout tone="warn" title="Archived">
            This artifact is out of the default Library list. Nothing was destroyed -- the row, its
            labels and its provenance are intact, and the Show archived toggle on the list brings it
            back into view.
          </Callout>
        ) : null}

        {detail.archiveError ? (
          <ErrorMessage>Could not archive: {detail.archiveError}</ErrorMessage>
        ) : null}

        {summary ? <p className="max-w-2xl text-sm text-muted">{summary}</p> : null}

        <Band title="Labels" meta="free-text -- yours, or added by an agent you talk to">
          {/* The live-region announcement lives HERE, at the page level, not
              inside LabelChips itself -- role="status" is a page-level
              convention in this codebase (WriteOutcome.tsx,
              RailStatus.tsx), and LabelChips is a shared primitive with
              no page context of its own. Visually hidden: the chip that
              changed is already visible feedback for a sighted user: this is
              only for the screen-reader announcement a silent chip update
              would otherwise skip. It also carries the training and archive
              outcomes, for the same reason. */}
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

        {isFileArtifact(artifact) ? (
          <FileBand detail={detail} />
        ) : null}

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

      <ConfirmDialog
        open={confirmArchive}
        title="Archive this artifact?"
        confirmLabel="Archive"
        tone="danger"
        busy={detail.archiveBusy}
        onCancel={() => setConfirmArchive(false)}
        onConfirm={() => {
          setConfirmArchive(false);
          detail.archive();
        }}
      >
        <p>{`"${title || artifactId}" drops out of the default Library list.`}</p>
        <p className="mt-2">
          Nothing is destroyed: the row, its labels and its provenance survive, and the Show
          archived toggle brings it back into view. A file artifact archives its stored file with
          it.
        </p>
      </ConfirmDialog>
    </Container>
  );
}

// The backing file, and the second act (design D7): a file in the Library is
// yours the moment it lands, and training it into a knowledge domain is a
// separate decision recorded on the file.
function FileBand({ detail }: { detail: ReturnType<typeof useArtifactDetail> }): ReactNode {
  const file = detail.file;
  if (file === null) {
    return (
      <Band title="File">
        {detail.loading ? (
          <Skeleton variant="kv" rows={3} />
        ) : (
          <p className="text-sm text-muted">
            This artifact says it is a file, but its backing row did not come back. That is what a
            file archived out from under its index row looks like.
          </p>
        )}
      </Band>
    );
  }

  const status = rowString(file, "status");
  const failureReason = rowString(file, "failureReason");
  const embeddingStatus = rowString(file, "embeddingStatus");

  return (
    <Band title="File" meta="the bytes behind this artifact, and what the Library made of them">
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1.5 text-sm">
        <dt className="text-muted">Name</dt>
        <dd className="text-fg">{rowString(file, "name") || "—"}</dd>
        <dt className="text-muted">Type</dt>
        <dd className="text-fg">{rowString(file, "mimeType") || "unknown"}</dd>
        <dt className="text-muted">Size</dt>
        <dd>
          <DataText kind="number">{rowNumber(file, "size").toLocaleString()}</DataText>
          <span className="text-muted"> bytes</span>
        </dd>
        <dt className="text-muted">Analysis</dt>
        <dd className="text-fg">
          {status || "unknown"}
          {failureReason ? <span className="text-danger"> — {failureReason}</span> : null}
        </dd>
        <dt className="text-muted">Searchable</dt>
        <dd className="text-fg">
          {/* `none` is not a failure: a type the Library cannot read is stored
              opaquely by design (3.4), and saying "none" rather than hiding
              the row is what tells a person why it will not turn up in a
              meaning search. */}
          {embeddingStatus || "none"}
        </dd>
      </dl>

      <div className="mt-4">
        <TrainControl detail={detail} />
      </div>
    </Band>
  );
}

// THE DOMAIN PICKER, AND WHY IT IS A COMBO RATHER THAN A CLOSED LIST.
//
// "The domains the caller may write to" is a question this engine cannot
// answer. v1:knowledge:knowledgeDomain is declared in no .memql file in the
// engine tree -- it is product-owned (design D2) -- so there is no query to
// list domains and no per-row tier to lean on. libraryTrainFile delegates the
// decision to a DomainWriteAuthorizer the product wires, and REFUSES when
// none is (integrations/library/train.go), which makes the server the only
// authority on the answer.
//
// So the suggestions are the one list the engine CAN produce: the domains
// this caller has already trained a file into, read off their own
// v1:library:file rows. A datalist rather than a <select>, because that list
// is a memory aid and not the set of legal values -- a first training into a
// new domain must be possible, and a closed control would make the common
// first use of the feature impossible. A refusal comes back from the server
// and renders inline, which is the honest place for it.
function TrainControl({ detail }: { detail: ReturnType<typeof useArtifactDetail> }): ReactNode {
  const [domain, setDomain] = useState("");
  const listId = "artifact-train-domains";

  function submit(event: FormEvent): void {
    event.preventDefault();
    const target = domain.trim();
    if (target === "") return;
    detail.train(target);
    setDomain("");
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-2">
      <p className="text-xs text-muted">
        Trained into:{" "}
        {detail.trainedDomains.length === 0 ? (
          <span className="text-subtle">nothing yet — uploading does not train.</span>
        ) : (
          detail.trainedDomains.map((one) => (
            <span key={one} className="mr-1.5">
              <DataText kind="id">{one}</DataText>
            </span>
          ))
        )}
      </p>
      {detail.trainError ? <ErrorMessage>{detail.trainError}</ErrorMessage> : null}
      <div className="flex flex-wrap items-end gap-2">
        <Field
          label="Train into a knowledge domain"
          grow
          hint="The cluster decides whether you may write to it; a domain you have used before is suggested."
        >
          {/* A plain <input list=...> rather than ui/TextInput: the list
              attribute is what makes it a combo box, and TextInput takes no
              arbitrary props by design. The class string is TextInput's own
              inset recipe, kept identical on purpose. */}
          <input
            type="text"
            list={listId}
            value={domain}
            disabled={detail.trainBusy}
            onChange={(event) => setDomain(event.target.value)}
            placeholder="finance-policies"
            aria-label="Knowledge domain"
            className="w-full rounded border border-line bg-surface px-3 py-2 text-sm text-fg placeholder:text-subtle disabled:cursor-not-allowed disabled:opacity-40"
          />
          <datalist id={listId}>
            {detail.knownDomains.map((one) => (
              <option key={one} value={one} />
            ))}
          </datalist>
        </Field>
        <Button
          type="submit"
          size="xs"
          busy={detail.trainBusy}
          busyLabel="Training…"
          disabled={domain.trim() === ""}
        >
          <GraduationCap size={14} aria-hidden="true" />
          Train
        </Button>
      </div>
    </form>
  );
}
