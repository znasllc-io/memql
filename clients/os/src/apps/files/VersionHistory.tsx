import { Download, RotateCw } from "lucide-react";

import { Button, Chip, Notice, ProvenanceDot, Subhead, formatBytes, formatFreshness, formatMoment, useNow } from "../../kit";
import type { MachinePresence } from "../../items/provenance";
import { shortDigest, versionStory, type VersionEntry, type VersionHistory as History } from "./versions";

// The version history panel (epic memql#4806, #4808).
//
// ===========================================================================
// A HISTORY IS A TIMELINE, NOT A TABLE
// ===========================================================================
// Version numbers are a real sequence -- v3 came after v2 because somebody
// uploaded it -- so the stack is drawn as a spine with a node per version:
// filled at the one the file holds now, hollow below it. That is the one
// visual device this panel adds, and it earns its place by carrying
// information the rows would otherwise only imply.
//
// EVERY VERSION REPEATS THE HEADER'S STORY LANGUAGE. The sentence and the
// presence dot at the top of the inspector are the thing this platform can say
// that a folder of bytes cannot; a history where each entry says where THOSE
// bytes came from is the same claim over time. A version pushed from a laptop
// says so, next to a version dropped from a browser that says "Uploaded here",
// because that is what happened.
//
// NOTHING RENDERS BLANK. An unmeasured hash is a dash, not an error and not an
// empty cell -- absent means "not measured yet", which the concept says in as
// many words.

function entryTitle(entry: VersionEntry): string {
  const uploaded = `Uploaded ${formatMoment(entry.uploadedAt)}`;
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
  onDownload: (entry: VersionEntry) => void;
  /** The version currently downloading, or 0. */
  downloadingVersion: number;
}) {
  const now = useNow();

  return (
    <section className="os-files-versions" aria-label="Version history">
      <div className="os-files-versions-head">
        <Subhead>Versions</Subhead>
        <span className="os-caption">
          {/* WHEN THIS WAS READ, said plainly. These rows carry no broadcast
              routing rule, so this panel is a read rather than a feed -- and a
              surface that looked live while sitting still would be the lie
              worth avoiding here. */}
          {loading ? "Reading" : readAt === null ? "Not read yet" : `Read ${formatFreshness(readAt.toISOString(), now)}`}
        </span>
        <Button onClick={onRefresh} ariaLabel="Read the version history again">
          <RotateCw size={12} aria-hidden />
        </Button>
      </div>

      {error !== "" ? (
        <Notice
          tone="error"
          sentence="The version history could not be read."
          next={history.entries.length > 0 ? "What is below is what was read last time." : undefined}
          detail={error}
        />
      ) : null}

      {history.entries.length === 0 && !loading && error === "" ? (
        <p className="os-caption">No versions to show for this file.</p>
      ) : null}

      {history.total === 1 && history.entries.length === 1 ? (
        // The invitation. A file with one version is the normal case, and the
        // panel's job there is to teach what the action does rather than to
        // render a list of one.
        <p className="os-files-versions-only">
          One version so far. Upload a new one and this file keeps its place: same row, same
          folder, same labels, with this version kept and downloadable.
        </p>
      ) : null}

      {history.entries.length > 0 ? (
        <ol className="os-files-spine">
          {history.entries.map((entry) => {
            const machine =
              entry.uploadedFromWorkerId === "" ? null : presence(entry.uploadedFromWorkerId);
            const story = versionStory(entry, machine);
            return (
              <li key={entry.key} className="os-files-version" data-current={entry.current || undefined}>
                <span className="os-files-version-node" aria-hidden />
                <div className="os-files-version-body">
                  <p className="os-files-version-line">
                    <span className="os-files-version-n">v{entry.versionNumber}</span>
                    {/* The chip distinguishes the live version from the ones
                        under it, so on a file with only one version there is
                        nothing for it to distinguish and it is noise. */}
                    {entry.current && history.entries.length > 1 ? (
                      <Chip tone="accent">current</Chip>
                    ) : null}
                    <span className="os-files-version-when" title={entryTitle(entry)}>
                      {formatFreshness(entry.uploadedAt, now)}
                    </span>
                  </p>
                  <p className="os-files-version-story">
                    <ProvenanceDot tone={story.tone} />
                    <span>{story.sentence}</span>
                  </p>
                  {/* A version that arrived under a different name is news;
                      repeating the same name on every row is not. */}
                  {entry.name !== "" && entry.name !== headName ? (
                    <p className="os-files-version-name" title={entry.name}>
                      as {entry.name}
                    </p>
                  ) : null}
                  <p className="os-files-version-meta">
                    <span className="os-files-version-bytes">{formatBytes(entry.size)}</span>
                    <span className="os-files-version-hash" title={entry.sha256 || "not measured yet"}>
                      {shortDigest(entry.sha256)}
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
                </div>
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
    </section>
  );
}
