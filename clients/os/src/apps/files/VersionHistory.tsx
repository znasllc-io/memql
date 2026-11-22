import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { Download } from "lucide-react";

import { AutoRefresh, Button, Chip, DetailsSection, Notice, formatBytes, formatFreshness, formatMoment, useNow } from "../../kit";
import type { MachinePresence } from "../../items/provenance";
import { shortDigest, versionStory, type VersionEntry, type VersionHistory as History } from "./versions";

// A compact version list. Each entry describes its own recorded origin;
// missing upload attribution is not evidence of a browser upload.
//
// It is one section of the file's Details page, laid out like Fleet's (item
// 11, Nov 20): "Versions" and its count in the section column, then one row
// per version on the page's label and value columns -- the version and its
// chip, then its story, size and digest -- with when and the download at the
// row's end.

function entryTitle(entry: VersionEntry): string {
  const uploaded = `Saved ${formatMoment(entry.uploadedAt)}`;
  if (entry.supersededAt === "") return uploaded;
  return `${uploaded}. Superseded ${formatMoment(entry.supersededAt)}.`;
}

export function VersionHistory({
  history,
  headName,
  loading,
  error,
  readAt,
  presence,
  onRefresh,
  onDownload,
  downloadingVersion,
}: {
  history: History;
  /** The current version's name, so an entry only names itself when it differs. */
  headName: string;
  loading: boolean;
  error: string;
  readAt: Date | null;
  presence: (workerId: string) => MachinePresence | null;
  onRefresh: () => void;
  /** Returns the download's promise, so a version's Download takes no second press until it settles. */
  onDownload: (entry: VersionEntry) => unknown;
  /** The version currently downloading, or 0. */
  downloadingVersion: number;
}) {
  const now = useNow();

  return (
    <DetailsSection title="Versions" label="Version history" meta={!loading && !error && readAt !== null ? history.total : undefined}>
      <AutoRefresh onRefresh={onRefresh} busy={loading} />

      {error !== "" ? (
        <Notice
          tone="error"
          sentence="The version history could not be read."
          next={history.entries.length > 0 ? "What is below is what was read last time." : undefined}
          detail={error}
        />
      ) : null}

      {history.entries.length === 0 && loading && !error ? <RecordListSkeleton label="Loading file versions" /> : null}
      {history.entries.length === 0 && !loading && error === "" ? (
        <p className="os-caption">No versions to show for this file.</p>
      ) : null}

      {history.entries.length > 0 ? (
        <ol className="os-file-version-list">
          {history.entries.map((entry) => {
            const machine =
              entry.uploadedFromWorkerId === "" ? null : presence(entry.uploadedFromWorkerId);
            const story = versionStory(entry, machine);
            return (
              <li key={entry.key} className="os-files-version" data-current={entry.current || undefined}>
                <p className="os-files-version-line">
                  <span className="os-files-version-n">v{entry.versionNumber}</span>
                  {/* The chip distinguishes the live version from the ones
                      under it, so on a file with only one version there is
                      nothing for it to distinguish and it is noise. */}
                  {entry.current && history.entries.length > 1 ? (
                    <Chip tone="accent">current</Chip>
                  ) : null}
                </p>
                <div className="os-files-version-body">
                  <p className="os-files-version-story">
                    <span>{story.sentence}</span>
                    <span className="os-files-version-bytes">{formatBytes(entry.size)}</span>
                    <span className="os-files-version-hash" title={entry.sha256 || "not measured yet"}>
                      {shortDigest(entry.sha256)}
                    </span>
                  </p>
                  {/* A version that arrived under a different name is news;
                      repeating the same name on every row is not. */}
                  {entry.name !== "" && entry.name !== headName ? (
                    <p className="os-files-version-name" title={entry.name}>
                      as {entry.name}
                    </p>
                  ) : null}
                </div>
                <p className="os-files-version-meta">
                  <span className="os-files-version-when" title={entryTitle(entry)}>
                    {formatFreshness(entry.uploadedAt, now)}
                  </span>
                  <Button
                    onClick={() => onDownload(entry)}
                    busy={downloadingVersion === entry.versionNumber}
                    busyLabel="..."
                    ariaLabel={`Download version ${entry.versionNumber}`}
                  >
                    <Download size={12} aria-hidden />
                  </Button>
                </p>
              </li>
            );
          })}
        </ol>
      ) : null}

      {history.truncated ? (
        // The head knows which version it is, so a short read is a fact this
        // panel can state rather than a prefix it shows as if it were all.
        <p className="os-caption">
          Showing the {history.shown} most recent of {history.total} versions.
        </p>
      ) : null}
    </DetailsSection>
  );
}
