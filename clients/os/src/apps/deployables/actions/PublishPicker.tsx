import { useMemo, useState } from "react";
import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import { FileArchive } from "lucide-react";

import { Button, Caption, LiveList, Notice, Row as ListRow, Subhead } from "../../../kit";
import { useLiveView } from "../../../live/liveView";
import { flatten, siteName, type SiteRow } from "../rows";
import { usePublish } from "../actions";
import { PICKER_PAGE_SIZE, useZipArtifacts } from "./useZipArtifacts";

// Publish a Library zip to this deployable.
//
// The picker is behind a disclosure and its feed is only retained while it is
// open (see `useZipArtifacts`): opening a detail panel must not read somebody's
// whole Library on the chance that they might publish.
//
// EVERY OUTCOME RENDERS HERE. The success summary and the refusal both land
// beside the button that produced them -- never a toast, because a refusal is
// usually the server's own reasoning and somebody who looked away has lost the
// only account of what happened.

interface ZipRow {
  id: string;
  title: string;
  mimeType: string;
  createdAt: string;
}

function zipFromRow(raw: Row): ZipRow {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    title: rowString(row, "title"),
    mimeType: rowString(row, "mimeType"),
    createdAt: rowString(row, "createdAt"),
  };
}

export function PublishPicker({ site }: { site: SiteRow }) {
  const [open, setOpen] = useState(false);
  const [chosen, setChosen] = useState("");
  const { busy, error, outcome, publish, reset } = usePublish();
  const {
    feed: { source: collection },
    pageWasFull,
  } = useZipArtifacts(open);

  const zips = useLiveView<Row, ZipRow>(collection, "zips", (rows) =>
    rows.map(zipFromRow).filter((z) => z.id !== ""),
  );

  const chosenTitle = useMemo(
    () => zips?.snapshot.rows.find((z) => z.id === chosen)?.title ?? "",
    [zips, zips?.snapshot, chosen],
  );

  function choose(id: string) {
    setChosen(id);
    // A NEW CHOICE CLEARS THE LAST OUTCOME. Leaving a refusal on screen beside
    // a different bundle would attach the server's reason to the wrong file.
    reset();
  }

  if (!open) {
    return (
      <div className="os-form-row">
        <Button tone="primary" onClick={() => setOpen(true)}>
          Publish from the Library
        </Button>
        <Caption>
          Deploys a zip you already own. The bytes are read by the cluster from its own storage --
          nothing is uploaded from here.
        </Caption>
      </div>
    );
  }

  return (
    <div className="os-deploy-publish">
      <Subhead>Publish to {siteName(site)}</Subhead>

      <LiveList<ZipRow>
        source={zips}
        rowId={(z) => z.id}
        // A Library row's title and type are what a person would call a
        // change. `createdAt` never moves and nothing here churns, so there is
        // no heartbeat to keep out of this fingerprint -- but it is written as
        // the fields rather than as the whole row for the same reason:
        // whatever gets added to `ZipRow` later should have to be considered.
        fingerprint={(z) => `${z.title}|${z.mimeType}`}
        label="Your Library bundles"
        emptyText="No zips in your Library yet. Upload a zipped build -- index.html at the top level -- and it will appear here."
        renderRow={(zip) => (
          <ListRow
            icon={<FileArchive size={16} aria-hidden />}
            name={zip.title || zip.id}
            current={chosen === zip.id}
            open={chosen === zip.id}
            onOpen={() => choose(zip.id)}
            state={chosen === zip.id ? <span className="os-livelist-tick">chosen</span> : null}
          >
            <span className="os-caption os-mono">{zip.mimeType}</span>
          </ListRow>
        )}
      />

      {pageWasFull ? (
        <Caption>
          Showing the zips among your {PICKER_PAGE_SIZE} most recent Library entries. An older
          bundle can be published from the Library itself.
        </Caption>
      ) : null}

      {outcome ? (
        <Notice
          tone="info"
          sentence={`Published version ${outcome.version || "(unnamed)"} -- ${outcome.fileCount} files, ${formatBytes(outcome.totalBytes)}.`}
          next="The deployable is serving it now. The bundle reference above updates from the cluster's own event."
          detail={outcome.bundleRef}
        />
      ) : null}

      {error === "" ? null : (
        <Notice
          tone="error"
          sentence="That bundle was not published."
          next="Nothing changed -- this deployable is still serving what it was serving."
          detail={error}
        />
      )}

      <div className="os-form-row">
        <Button
          tone="primary"
          disabled={chosen === ""}
          busy={busy}
          busyLabel="Publishing"
          onClick={() => void publish(site.id, chosen)}
        >
          {chosen === "" ? "Pick a bundle" : `Publish ${chosenTitle || chosen}`}
        </Button>
        <Button
          onClick={() => {
            setOpen(false);
            setChosen("");
            reset();
          }}
        >
          Done
        </Button>
      </div>
    </div>
  );
}

/**
 * Bytes, in the unit a person would use.
 *
 * Powers of 1024 with the SI-ish names everybody actually reads, which is what
 * every other tool in this deploy path prints. Local to this file: it has one
 * caller, and promoting on the first use invents an abstraction from one
 * example.
 */
function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 bytes";
  const units = ["bytes", "KB", "MB", "GB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${unit === 0 ? Math.round(value) : value.toFixed(1)} ${units[unit]}`;
}
