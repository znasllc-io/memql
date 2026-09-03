import { useMemo, useState } from "react";
import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import { FileArchive } from "lucide-react";

import { Button, Caption, LiveList, Notice, Row as ListRow, Subhead } from "../../../../kit";
import { useLiveView } from "../../../../live/liveView";
import { usePublish } from "../../actions";
import { PICKER_PAGE_SIZE, useZipArtifacts } from "../../actions/useZipArtifacts";
import { flatten, type SiteRow } from "../../rows";
import { isPublished } from "../rail";

// Deploy a Library zip to this hand-made deployable -- the Source stop's
// picker, folded in from the detail panel's publish picker (memql#4725).
//
// The picker is behind a disclosure and its feed is only retained while it is
// open (see `useZipArtifacts`): opening a page must not read somebody's whole
// Library on the chance that they might deploy. The Head's Redeploy on a
// hand-made deployable opens it, which is why `open` is the page's state
// rather than this component's.
//
// EVERY OUTCOME RENDERS HERE. The success summary and the refusal both land
// beside the button that produced them -- never a toast, because a refusal is
// usually the server's own reasoning and somebody who looked away has lost the
// only account of what happened. "Published" is the word the finished publish
// uses, because Deploy is the button that produced it.

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

export function ZipPicker({
  site,
  open,
  onOpenChange,
}: {
  site: SiteRow;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
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

  // A never-published deployable is DEPLOYED from a zip; one that has served
  // something is redeployed. Same act, and the word says which it is.
  const verb = isPublished(site) ? "Redeploy" : "Deploy";

  function choose(id: string) {
    setChosen(id);
    // A NEW CHOICE CLEARS THE LAST OUTCOME. Leaving a refusal on screen beside
    // a different bundle would attach the server's reason to the wrong file.
    reset();
  }

  if (!open) {
    return (
      <div className="os-form-row">
        <Button onClick={() => onOpenChange(true)}>{verb} from a zip</Button>
        <Caption>
          Deploys a zip you already own from the Library. The bytes are read by the cluster from its own storage --
          nothing is uploaded from here.
        </Caption>
      </div>
    );
  }

  return (
    <div className="os-deploy-publish">
      <Subhead>{verb} from a zip</Subhead>

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
          Showing the zips among your {PICKER_PAGE_SIZE} most recent Library entries. An older bundle can be deployed
          from the Library itself.
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
          sentence="That bundle was not deployed."
          next="Nothing changed -- this deployable is still serving what it was serving."
          detail={error}
        />
      )}

      <div className="os-form-row">
        <Button
          tone="primary"
          disabled={chosen === ""}
          busy={busy}
          busyLabel="Deploying"
          onClick={() => void publish(site.id, chosen)}
        >
          {chosen === "" ? "Pick a bundle" : `Deploy ${chosenTitle || chosen}`}
        </Button>
        <Button
          onClick={() => {
            onOpenChange(false);
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
 * every other tool in this deploy path prints -- and deliberately NOT the
 * kit's IEC `formatBytes`, whose "MiB" is the storage caps' voice, because the
 * publish summary this sentence sits in has always read "2.0 MB". Local to
 * this file: it has one caller.
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
