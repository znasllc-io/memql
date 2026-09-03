import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import { FileArchive } from "lucide-react";

import {
  Caption,
  ChoiceStack,
  LiveList,
  Notice,
  Row as ListRow,
  formatBytes,
  useLiveView,
} from "../../../../../kit";
import { flatten } from "../../../../../kit/rows";
import { zipUnusableNote, type ZipVerdict } from "../../../sources/probe";
import type { ArtifactProbeHandle, SourceProbeHandle } from "../../../sources/useProbes";
import { PICKER_PAGE_SIZE, useZipArtifacts } from "../../../sources/useZipArtifacts";
import type { CredentialRow } from "../../../sources/rows";
import { suggestName, type ComposeDraft } from "../../compose";
import { CiHandoff } from "./CiHandoff";
import { KindField, NameField } from "./fields";
import { RepositorySource } from "./RepositorySource";

// The compose Source stop: where this deployable comes from, asked once
// (epic memql#4885, design section C).
//
// ===========================================================================
// THREE ANSWERS, AND THE THIRD IS NOT A KIND OF THE OTHER TWO
// ===========================================================================
// A repository, a zip already in Files, or bytes your CI pushes. They are not
// three flavours of one field: a repository is tracked and re-fetched, a zip
// is a snapshot with nothing upstream, and a CI push is a door this cluster
// opens and then waits at. So they are the shell's own choice control, chosen
// once, and each carries what it alone needs.
//
// ===========================================================================
// THE PROBE IS A COURTESY. IT ANSWERS, IT DOES NOT DECIDE
// ===========================================================================
// On blur the repository branch asks `sourceProbe` whether this cluster can
// read the tree, and renders its typed reason. What that reason is WORTH is
// `sources/probe.ts`'s rule and not this file's: a definite answer about the
// repository parks the flow, and an answer about the probe itself -- rate
// limiting, or a probe that threw -- says so, leaves the field editable and
// leaves Analyze reachable. A public repository is never blocked by a probe
// that could not run (design H).
//
// ===========================================================================
// THE REPOSITORY BRANCH IS THREE READINGS, AND IT OWNS THEM
// ===========================================================================
// GitHub Connect (memql#4915) landed in the slot this file left for it: with
// a grant the branch is a picker over the repositories that grant can see,
// without one it offers Connect, and on a cluster with no GitHub App it is
// the URL-plus-token form it has always been. `RepositorySource` decides
// which, because that decision is about a person's credentials rather than
// about which of the three SOURCES they chose -- which is all this file is
// for.

export function ComposeSourceStop({
  draft,
  onDraft,
  credentials,
  isClusterOwner,
  probe,
  zipProbe,
  zip,
  siteId,
  clusterDomain,
  locked,
  tokenFormOpen,
  onTokenFormOpenChange,
}: {
  draft: ComposeDraft;
  onDraft: (patch: Partial<ComposeDraft>) => void;
  credentials: readonly CredentialRow[];
  /** A CI-pushed source is a cluster owner's act (design section C). */
  isClusterOwner: boolean;
  probe: SourceProbeHandle;
  zipProbe: ArtifactProbeHandle;
  /** The zip's verdict once it has been probed; null before that. */
  zip: ZipVerdict | null;
  /** The draft site, once Analyze has created one on the hand-made path. */
  siteId: string;
  clusterDomain: string;
  /** Chosen once: after Analyze the stop is facts, not fields. */
  locked: boolean;
  /** "Use a token instead", held by the page for `ZipPicker`'s reason. */
  tokenFormOpen: boolean;
  onTokenFormOpenChange: (open: boolean) => void;
}) {
  if (locked) return <ChosenSource draft={draft} zip={zip} siteId={siteId} clusterDomain={clusterDomain} />;

  return (
    <div className="os-stop-body">
      <ChoiceStack
        name="os-compose-source"
        label="Where it comes from"
        /* PROSE, not the data voice: "A repository" is a sentence about a
           choice rather than a value anybody types anywhere, and the code
           face would say it was one. */
        voice="prose"
        value={draft.choice}
        onChange={(choice) =>
          onDraft({ choice: choice as ComposeDraft["choice"], name: "", kind: "", artifactId: "", repoUrl: "" })
        }
        options={[
          {
            value: "repo",
            label: "A repository",
            description:
              "The repository stays the source of truth, and this cluster notices when something newer lands there. github.com today.",
          },
          {
            value: "zip",
            label: "A zip in Files",
            description: "A snapshot you already own, with nothing upstream. It deploys in exactly the same way.",
          },
          ...(isClusterOwner
            ? [
                {
                  value: "ci",
                  label: "Pushed by your CI",
                  description:
                    "Nothing is fetched. This cluster opens a door, hands you the route and the token command, and waits for the first push.",
                },
              ]
            : []),
        ]}
      />

      {draft.choice === "repo" ? (
        <RepositorySource
          draft={draft}
          onDraft={onDraft}
          credentials={credentials}
          probe={probe}
          tokenFormOpen={tokenFormOpen}
          onTokenFormOpenChange={onTokenFormOpenChange}
        />
      ) : null}
      {draft.choice === "zip" ? (
        <ZipBranch draft={draft} onDraft={onDraft} zipProbe={zipProbe} zip={zip} />
      ) : null}
      {draft.choice === "ci" ? <CiBranch draft={draft} onDraft={onDraft} /> : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// A zip in Files
// ---------------------------------------------------------------------------

interface ZipRow {
  id: string;
  title: string;
  mimeType: string;
}

function zipFromRow(raw: Row): ZipRow {
  const row = flatten(raw);
  return { id: rowString(row, "id"), title: rowString(row, "title"), mimeType: rowString(row, "mimeType") };
}

/**
 * The zip picker, and what the cluster says the zip IS.
 *
 * The feed is retained only while this branch is showing (`useZipArtifacts`),
 * because reading somebody's whole Library on the chance that they might pick
 * a zip is a read nobody asked for. Choosing one opens it through the same
 * fetch a deploy uses -- so a zip the deploy would refuse is refused here, by
 * the same code, before anything is created.
 */
function ZipBranch({
  draft,
  onDraft,
  zipProbe,
  zip,
}: {
  draft: ComposeDraft;
  onDraft: (patch: Partial<ComposeDraft>) => void;
  zipProbe: ArtifactProbeHandle;
  zip: ZipVerdict | null;
}) {
  const {
    feed: { source: collection },
    pageWasFull,
  } = useZipArtifacts(true);
  const zips = useLiveView<Row, ZipRow>(collection, "compose-zips", (rows) =>
    rows.map(zipFromRow).filter((z) => z.id !== ""),
  );

  function choose(row: ZipRow) {
    onDraft({ artifactId: row.id, name: draft.name || suggestName({ ...draft, choice: "zip" }, row.title) });
    void zipProbe.probe(row.id);
  }

  return (
    <>
      <LiveList<ZipRow>
        source={zips}
        rowId={(z) => z.id}
        fingerprint={(z) => `${z.title}|${z.mimeType}`}
        label="Your Library zips"
        emptyText="No zips in your Library yet. Upload one in Files and it will appear here."
        renderRow={(row) => (
          <ListRow
            icon={<FileArchive size={16} aria-hidden />}
            name={row.title || row.id}
            current={draft.artifactId === row.id}
            open={draft.artifactId === row.id}
            onOpen={() => choose(row)}
            state={draft.artifactId === row.id ? <span className="os-livelist-tick">chosen</span> : null}
          >
            <span className="os-caption os-mono">{row.mimeType}</span>
          </ListRow>
        )}
      />
      {pageWasFull ? (
        <Caption>
          Showing the zips among your {PICKER_PAGE_SIZE} most recent Library entries. An older one can be deployed from
          a deployable that already exists.
        </Caption>
      ) : null}

      {zipProbe.error === "" ? null : (
        <Notice
          tone="error"
          sentence="That zip could not be opened."
          next="Nothing was created. Pick another, or fix the archive and upload it again."
          detail={zipProbe.error}
        />
      )}

      {zipProbe.reply === null || zip === null ? null : zip === "package" ? (
        <p className="os-stop-verdict" data-tone="ok" role="status">
          a package -- {zipProbe.reply.fileCount} files, {formatBytes(zipProbe.reply.totalBytes)}. Analyze reads its
          manifest and says what deploying it would do.
        </p>
      ) : zip === "built_site" ? (
        <p className="os-stop-verdict" data-tone="ok" role="status">
          a built site -- index.html at the root, {zipProbe.reply.fileCount} files,{" "}
          {formatBytes(zipProbe.reply.totalBytes)}.
        </p>
      ) : (
        /* NEITHER IS NOT A REFUSAL, so it is not a Notice: the zip is a
           perfectly good file and this cluster cannot deploy it. It says
           what it counted and stops there. */
        <p className="os-stop-verdict" data-tone="warn" role="status">
          {zipUnusableNote(zipProbe.reply)}
        </p>
      )}

      {zip === "built_site" ? <KindField draft={draft} onDraft={onDraft} /> : null}
      {zip === null ? null : <NameField draft={draft} onDraft={onDraft} label="Call it" placeholderFrom="" />}
    </>
  );
}

// ---------------------------------------------------------------------------
// Pushed by your CI
// ---------------------------------------------------------------------------

function CiBranch({ draft, onDraft }: { draft: ComposeDraft; onDraft: (patch: Partial<ComposeDraft>) => void }) {
  return (
    <>
      <Caption>
        Nothing is fetched and nothing is built here. Analyze creates the deployable as a draft with a placeholder
        bundle; your CI publishes into it, and the address starts serving the first time it does.
      </Caption>
      <NameField draft={draft} onDraft={onDraft} label="Call it" placeholderFrom="" />
      <KindField draft={draft} onDraft={onDraft} />
    </>
  );
}

// ---------------------------------------------------------------------------
// Once it is chosen
// ---------------------------------------------------------------------------

/**
 * After Analyze the source is FACTS, and the rail's note already carries the
 * one-line answer -- so this body adds only what the note cannot: for a
 * CI-pushed deployable, the route and the command that mints a token for it.
 */
function ChosenSource({
  draft,
  zip,
  siteId,
  clusterDomain,
}: {
  draft: ComposeDraft;
  zip: ZipVerdict | null;
  siteId: string;
  clusterDomain: string;
}) {
  if (draft.choice === "ci" && siteId !== "") {
    return (
      <div className="os-stop-body">
        <CiHandoff siteId={siteId} name={draft.name} clusterDomain={clusterDomain} />
      </div>
    );
  }
  if (draft.choice === "zip" && zip === "built_site") {
    return (
      <div className="os-stop-body">
        <Caption>
          A built site is its own output, so nothing here is built. Deploy publishes this zip's files under a new
          version and points the address at them.
        </Caption>
      </div>
    );
  }
  return null;
}
