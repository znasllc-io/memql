import { useMemo, useState } from "react";
import { ArrowUpCircle, Archive, GitBranch, Package as PackageIcon, Plus } from "lucide-react";

import type { LiveSnapshot } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, Chip, Head, LiveList, Notice, Row as ListRow, useLiveView } from "../../../kit";
import type { LiveView } from "../../../live/liveView";
import { NewPackage } from "./NewPackage";
import { PackageDetail } from "./PackageDetail";
import { packageFingerprint, shortVersion, sourceLabel, type PackageRow } from "./rows";

// The packages this cluster tracks, live.
//
// ===========================================================================
// THE UPDATE CUE IS THE POINT OF THIS LIST
// ===========================================================================
// A package's whole promise is that the repository stays the source of truth:
// somebody pushes, and the platform notices. That noticing is one broadcast
// row change (`updateAvailable`), and it lands here without a reload.
//
// It is rendered as a STANDING mark rather than as the arrival cue, and the
// two are different on purpose. The arrival cue is a ring that decays on the
// clock -- "something just happened". "There is a newer version than the one
// you are running" is a STATE, true until somebody deploys, so it gets a chip
// that stays. A cue alone would mean the news was only visible to whoever
// happened to be looking; a chip alone would mean it arrived silently. This
// list has both, and they say different things.

export function PackagesSection({
  source,
  snapshot,
  selectedPackageId,
  onSelect,
  viewerUserId,
  domain,
  canWrite,
  onReseed,
  onAsk,
}: {
  source: LiveView<PackageRow> | null;
  snapshot: LiveSnapshot<PackageRow>;
  selectedPackageId: string;
  onSelect: (packageId: string) => void;
  viewerUserId: string;
  domain: string;
  canWrite: boolean;
  onReseed: () => void;
  onAsk?: (tag: string) => void;
}) {
  const [showArchived, setShowArchived] = useState(false);
  const [adding, setAdding] = useState(false);

  const rows = source?.snapshot.rows ?? [];
  const open = rows.find((p) => p.id === selectedPackageId) ?? null;
  const archivedCount = useMemo(() => rows.filter((p) => p.status === "archived").length, [rows]);

  // THE FILTER IS THE VIEW KEY, deliberately. Revealing rows the browser
  // already had is not the cluster sending them, so a filter change must
  // re-BASELINE rather than announce every newly-visible row as an arrival --
  // which is exactly the contract clients/os/README.md states, and `viewKey`
  // is where a re-baseline is expressed.
  const shown = useLiveView<PackageRow, PackageRow>(
    source,
    showArchived ? "packages:archived" : "packages:active",
    (all) => all.filter((p) => (showArchived ? p.status === "archived" : p.status !== "archived")),
  );

  return (
    <div className="os-app-stack">
      <Head title="Packages">
        {canWrite ? (
          <Button tone="primary" onClick={() => setAdding((v) => !v)} ariaExpanded={adding}>
            <Plus size={13} aria-hidden /> Add a package
          </Button>
        ) : null}
      </Head>

      {snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return its packages."
          next="The engine decides which packages reach you: your own, or every one of them if you are a cluster owner."
        >
          <Button onClick={onReseed}>Try again</Button>
        </Notice>
      ) : null}

      {adding ? (
        <NewPackage
          onDone={(packageId) => {
            setAdding(false);
            if (packageId !== "") onSelect(packageId);
          }}
          onCancel={() => setAdding(false)}
        />
      ) : null}

      <LiveList<PackageRow>
        source={shown}
        rowId={(p) => p.id}
        fingerprint={packageFingerprint}
        label="Packages this cluster tracks"
        emptyText={
          showArchived
            ? "Nothing archived. Archived packages stay here, so they can always be found again."
            : "No packages yet. Add a repository or a zip, and this cluster will tell you what deploying it would do before it does anything."
        }
        renderRow={(p, tick) => (
          <ListRow
            icon={<PackageIcon size={13} aria-hidden />}
            name={p.name === "" ? "unnamed package" : p.name}
            current={p.status !== "archived"}
            dim={p.status === "archived"}
            onOpen={() => onSelect(p.id === selectedPackageId ? "" : p.id)}
            open={p.id === selectedPackageId}
            state={
              <>
                {p.updateAvailable ? (
                  <Chip tone="accent" title={`Newer upstream: ${p.latestKnownVersion}`}>
                    <ArrowUpCircle size={11} aria-hidden /> update
                  </Chip>
                ) : null}
                {p.status === "archived" ? (
                  <Chip tone="muted">
                    <Archive size={11} aria-hidden /> archived
                  </Chip>
                ) : null}
                {tick}
              </>
            }
          >
            <span className="os-pkg-source">
              {p.sourceKind === "repo" ? <GitBranch size={11} aria-hidden /> : null}
              {sourceLabel(p)}
            </span>
            <span className="os-pkg-version">
              {p.deployedVersion === "" ? "never deployed" : shortVersion(p.deployedVersion)}
            </span>
          </ListRow>
        )}
      />

      {archivedCount > 0 || showArchived ? (
        <div className="os-archive-toggle">
          <Button onClick={() => setShowArchived((v) => !v)} ariaExpanded={showArchived}>
            <Archive size={12} aria-hidden />{" "}
            {showArchived ? "Show active packages" : `Show archived (${archivedCount})`}
          </Button>
          <Caption>
            {showArchived
              ? "Archived packages are kept, not deleted. Restoring one puts it back on the active list."
              : "An archive is a place, not a void."}
          </Caption>
        </div>
      ) : null}

      {open ? (
        <PackageDetail
          pkg={open}
          viewerUserId={viewerUserId}
          domain={domain}
          canWrite={canWrite}
          onAsk={onAsk}
        />
      ) : null}
    </div>
  );
}
