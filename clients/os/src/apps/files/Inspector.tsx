import { useCallback, useMemo, useState } from "react";

import { useAuthSource } from "../../auth/context";
import { useOs } from "../../chrome/state";
import { canOpen } from "../../system/registry";
import { Details, DetailsSection, Fact, Facts, Notice, Head, formatMoment } from "../../kit";
import { LabelEditor } from "./LabelEditor";
import { AccountPicker } from "../accounts/AccountPicker";
import { useAccountOptions } from "../accounts/tie";
import { useArtifactAccounts } from "./actions/accounts";
import { useFileVersions } from "./actions/versions";
import { VersionHistory } from "./VersionHistory";
import { fileHeadFromRow, type VersionEntry } from "./versions";
import { useLiveCollection } from "../../live/useLiveCollection";
import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import type { Breadcrumb } from "../../kit/Breadcrumbs";
import { MATERIALIZER_APP_ID, MATERIALIZER_MADE_SECTION } from "../../items/materialize";
import {
  OVER_LIMIT_SENTENCE,
  planDownload,
  runBufferedDownload,
} from "./actions/download";
import { downloadWorkerRegistration, runWorkerDownload } from "./actions/downloadWorker";
import { artifactName, fileStory, type ArtifactRow, type CompositionRow } from "./rows";

// File details occupy the page, laid out like Fleet's machine Details tab
// (José, Nov 19: "this is how details should show up"; Jesús picked draft A,
// Nov 20): each section's name in its own left column, then a label column
// and a value column, a thin line between sections, and the copyable row under
// the pointer lit with its copy button -- the kit's Details, DetailsSection and
// Fact.copyValue, which own that grid for Fleet and Cluster too (DESIGN.md,
// "Details and settings content"). Same sections and words as before; Labels
// and Client are one short row each. No action bar (Nov 20 call, below).

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function Inspector({
  row,
  files,
  composition,
  folderNameOf,
  presence,
  onClose,
  backLabel = "Files",
  breadcrumbs,
}: {
  row: ArtifactRow;
  files: readonly Row[];
  /**
   * The composition that produced this file, if one did (epic memql#4981,
   * #4983). ONE SENTENCE AND ONE ACT is the whole of what Files says about
   * it: the record -- the sources, the template, the models that contributed,
   * the provenance -- belongs to the Materializer, and restating any of it
   * here would be a second reading free to disagree with the app whose
   * subject it is.
   */
  composition: CompositionRow | null;
  folderNameOf: (folderId: string) => string;
  presence: (workerId: string) => { name?: string; online: boolean } | null;
  onClose: () => void;
  backLabel?: string;
  breadcrumbs?: readonly Breadcrumb[];
}) {
  const { actions, registry } = useOs();
  const authSource = useAuthSource();

  const name = artifactName(row);
  const machine = row.producedByWorkerId ? presence(row.producedByWorkerId) : null;
  const story = fileStory(row, machine, composition);

  // NO ACTION BAR (José, Nov 20 2026 call: "we shouldn't have this footer
  // there ... this footer should only show up in the wizards ... it's
  // supposed to just show the details, not show actions. The actions are
  // back there"). Open, Download, Share, Send to desktop and Delete live on
  // the file's own icons and right-click menu in the place it was opened
  // from. The one act left here is downloading an older version from the
  // Versions list, and its refusal reports beside that list.
  const [downloadError, setDownloadError] = useState("");
  const accounts = useAccountOptions();
  const accountTie = useArtifactAccounts();

  // --- versions (epic memql#4806) ---
  //
  // The backing file id, taken from the artifact's own source ref. Blank for
  // every non-file kind, which reads nothing and shows no panel: a note has
  // no upload versions, and offering an empty history for one would answer a
  // question nobody asked.
  const fileId = useMemo(
    () => (row.kind === "file" ? (row.sourceConceptRef.split(":").pop() ?? "") : ""),
    [row.kind, row.sourceConceptRef],
  );
  const listedHead = files.find(file => rowString(file, "id") === fileId);
  const head = useLiveCollection<Row>(fileId && !listedHead ? `files:head:${fileId}` : null, connection => ({
    concept: "v1:library:file", paged: false,
    eventInScope: id => id === fileId,
    inScope: row => rowString(row, "id") === fileId,
    seed: async (_cursor, signal) => ({ rows: (await connection.query.libraryFileById({ fileId }, { signal })).rows(), nextCursor: "" }),
  }));
  const liveHead = listedHead ?? head.snapshot.rows[0];
  const uploadUnavailable = !!liveHead && rowString(liveHead, "status") === "failed" && !rowString(liveHead, "blobUrl").includes("://");
  const liveVersion = liveHead ? fileHeadFromRow(liveHead) : null;
  const headRevision = liveVersion ? `${liveVersion.versionNumber}:${liveVersion.sha256}:${liveVersion.versionUploadedAt}` : "";
  const versions = useFileVersions(fileId, headRevision);
  const [downloadingVersion, setDownloadingVersion] = useState(0);

  const downloadVersion = useCallback(
    async (entry: VersionEntry) => {
      setDownloadingVersion(entry.versionNumber);
      setDownloadError("");
      try {
        const registration = await downloadWorkerRegistration();
        const plan = planDownload({ workerAvailable: registration !== null, sizeBytes: entry.size });
        if (plan.path === "refused") {
          setDownloadError(OVER_LIMIT_SENTENCE);
          return;
        }
        // The version rides the SAME two runners the current version does --
        // worker when one exists, buffered otherwise -- so the over-limit
        // sentence, the refusal wording and the save behaviour cannot fork
        // between "this file" and "an older copy of this file".
        const version = entry.current ? undefined : entry.versionNumber;
        if (plan.path === "worker" && registration !== null) {
          await runWorkerDownload({
            artifactId: row.id,
            fileName: entry.name || name,
            sizeBytes: entry.size,
            bearer: () => authSource.bearer(),
            registration,
            ...(version === undefined ? {} : { version }),
          });
        } else {
          await runBufferedDownload({
            artifactId: row.id,
            fileName: entry.name || name,
            bearer: () => authSource.bearer(),
            ...(version === undefined ? {} : { version }),
          });
        }
      } catch (err: unknown) {
        setDownloadError(describe(err));
      } finally {
        setDownloadingVersion(0);
      }
    },
    [row.id, name, authSource],
  );

  const filedIn = folderNameOf(row.folderId);

  return (
    <section className="os-action-pane os-file-detail" aria-label="File details" data-os-page-context={JSON.stringify({ page: "File details", fileId: row.id, name, kind: row.kind })}>
      <div className="os-action-body os-file-detail-body">
      <Head navigationKey={`artifact:${row.id}`} title={name} breadcrumbs={breadcrumbs} back={{label: backLabel, onSelect: onClose}} />

      {uploadUnavailable ? <Notice tone="error" sentence="This upload did not finish." next="Upload the original file again from Files." /> : null}

      {/* A summary describes the contents; origin is a fact below. */}
      {row.summary.trim() !== "" ? <p className="os-files-summary">{row.summary}</p> : null}

      <Details>
        <DetailsSection title="Details">
          {/* Copy where it helps (DESIGN.md "Quiet copying"): names, formats,
              exact times and identifiers; the origin sentence and the kind
              are read, not copied. The two opaque values, Run and Id, come
              last, out of the way of the facts somebody actually reads. */}
          <Facts>
            <Fact label="Origin" value={story.sentence} />
            <Fact label="Kind" value={row.kind.replaceAll("_", " ")} />
            <Fact label="Filed in" value={filedIn} copyValue={filedIn} />
            <Fact label="Format" value={row.format || row.mimeType} copyValue={row.format || row.mimeType} mono />
            {row.kind === "document" ? (
              <Fact label="Validation" value={row.validationStatus} />
            ) : null}
            <Fact label="Created" value={formatMoment(row.createdAt)} copyValue={row.createdAt} />
            {row.producedByRunId !== "" ? (
              <Fact label="Run" value={row.producedByRunId} copyValue={row.producedByRunId} mono />
            ) : null}
            <Fact label="Id" value={row.id} copyValue={row.id} mono />
            {/* MADE IN THE MATERIALIZER. A fact and an act, and deliberately
                not a panel: what this file was made FROM is the record's
                question, and the record is one click away in the app that
                owns it. The act is ABSENT rather than disabled when that app
                is not open-able (DESIGN.md rule 12) -- `openApp` no-ops on an
                app the registry does not hold. */}
            {composition !== null ? (
              <Fact
                label="Made in"
                value={
                  canOpen(registry, MATERIALIZER_APP_ID) ? (
                    <button
                      type="button"
                      className="os-link"
                      onClick={() =>
                        actions.openApp(MATERIALIZER_APP_ID, MATERIALIZER_MADE_SECTION, {
                          compositionId: composition.id,
                        })
                      }
                    >
                      Open in Materializer
                    </button>
                  ) : (
                    "the Materializer"
                  )
                }
              />
            ) : null}
          </Facts>
        </DetailsSection>

        {/* LABELS (epic memql#5009): editable, so a section of its own rather
            than a fact row. One short row: the field and Add, then the chips.
            The browse's label FACET asks a different question of the same
            field and lives in the search panel's kind menu (DESIGN.md rule 2). */}
        <DetailsSection title="Labels">
          <LabelEditor artifactId={row.id} labels={row.labels} />
        </DetailsSection>

        {/* WHO THIS IS FOR (epic memql#4800, D5). ONE ACCOUNT PER ROW: a row
            belongs to one account, so this is the single picker, one short
            row. The index still stores the list `accountIds` until the export
            piece retires it, so the write sends a one-element list, or an
            empty one for none; the engine derives `accountId` from it. A file
            labelled with several accounts before this change shows its first,
            and choosing here replaces the list with that one choice.

            The write is ordinary and ungated. An account is a record with no
            read effect, so labelling a file changes who it is ABOUT and
            nothing about who may read it. */}
        <DetailsSection title="Client">
          <div className="os-file-detail-short">
            <AccountPicker
              id={`files-inspector-account-${row.id}`}
              label="The client this file is about"
              value={row.accountIds[0] ?? ""}
              accounts={accounts}
              disabled={accountTie.busy}
              onChange={(next) => void accountTie.setAccounts(row.id, next === "" ? [] : [next])}
            />
          </div>
          {accountTie.error === "" ? null : (
            <Notice
              tone="error"
              sentence="The client was not changed."
              next="This file still has the client it had."
              detail={accountTie.error}
            />
          )}
        </DetailsSection>

        {row.kind === "file" ? (
          <VersionHistory
            history={versions.history}
            headName={versions.head?.name ?? name}
            loading={versions.loading}
            error={versions.error}
            readAt={versions.readAt}
            presence={presence}
            onRefresh={versions.refresh}
            onDownload={(entry) => downloadVersion(entry)}
            downloadingVersion={downloadingVersion}
          />
        ) : null}
      </Details>
      {downloadError ? <Notice tone="error" sentence="The download did not land." detail={downloadError} /> : null}
      </div>
    </section>
  );
}
