import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import { FileArchive } from "lucide-react";

import {
  Caption,
  ChoiceStack,
  Field,
  LiveList,
  Notice,
  RecordRow as ListRow,
  formatBytes,
  useLiveView,
} from "../../../../../kit";
import { flatten } from "../../../../../kit/rows";
import { zipUnusableNote, type ZipVerdict } from "../../../sources/probe";
import type { ArtifactProbeHandle, SourceProbeHandle } from "../../../sources/useProbes";
import { PICKER_PAGE_SIZE, useZipArtifacts } from "../../../sources/useZipArtifacts";
import type { CredentialFeedStatus, CredentialRow } from "../../../sources/rows";
import type { GithubAppOwner } from "../../../sources/GithubAppSetup";
import type { GithubAppActions } from "../../../sources/useGithubApp";
import type { CredentialRevokeActions, GithubConnectActions } from "../../../sources/useGithubConnect";
import type { PackageRow } from "../../../packages/rows";
import { sourceLabel } from "../../../packages/rows";
import { suggestName, type ComposeDraft } from "../../compose";
import { CiHandoff } from "./CiHandoff";
import { KindField, NameField } from "./fields";
import { RepositorySource, type ConnectionNeed } from "./RepositorySource";

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
// Choosing a repository probes it under the caller's GitHub grant. A probe
// that could not run retains a retry and its server explanation. A definite
// authorization refusal clears the selection and offers reconnection. New
// repository creation has no pasted-token route; existing source details
// retain their stored-credential controls.

/** What each way in is called, as a person chose it. */
export const SOURCE_KIND_LABEL: Readonly<Record<string, string>> = {
  repo: "A repository",
  zip: "A zip in Files",
  ci: "Pushed by your CI",
};

/** What the step that follows the choice is called. */
export const SOURCE_DETAIL_NAME: Readonly<Record<string, string>> = {
  repo: "Repository",
  zip: "Zip",
  ci: "Your CI",
};

/**
 * WHERE IT COMES FROM -- one question, and the whole of its step.
 *
 * This used to be the top of a single Source step that then kept growing
 * beneath it: three cards, then two loose buttons (connect, or a token), then
 * a second stack of cards about updates, with a picker and three fields
 * arriving in between. Three questions on one page, none of them finished
 * before the next began -- the owner's word was "overcrowded".
 *
 * A wizard asks one thing at a time, so this step is the choice and nothing
 * else. CHOOSING ANSWERS IT: there is no Continue to press after the only
 * thing on the page has been answered, so the wizard moves on to the step the
 * answer names -- and this one folds to a line that can be opened again to
 * choose differently.
 */
export function ComposeSourceKindStep({
  draft,
  onChoose,
  isClusterOwner,
}: {
  draft: ComposeDraft;
  onChoose: (choice: ComposeDraft["choice"]) => void;
  /** A CI-pushed source is a cluster owner's act (design section C). */
  isClusterOwner: boolean;
}) {
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
        onChange={(choice) => onChoose(choice as ComposeDraft["choice"])}
        options={[
          {
            value: "repo",
            label: SOURCE_KIND_LABEL.repo!,
            description:
              "The repository stays the source of truth, and this cluster notices when something newer lands there. github.com today.",
          },
          {
            value: "zip",
            label: SOURCE_KIND_LABEL.zip!,
            description: "A snapshot you already own, with nothing upstream. It deploys in exactly the same way.",
          },
          ...(isClusterOwner
            ? [
                {
                  value: "ci",
                  label: SOURCE_KIND_LABEL.ci!,
                  description:
                    "Nothing is fetched. This cluster opens a door, hands you the route and the token command, and waits for the first push.",
                },
              ]
            : []),
        ]}
      />
    </div>
  );
}

/**
 * THE STEP THE CHOICE NAMES: Repository, Zip, or Your CI.
 *
 * Everything the chosen way in needs, and only that -- reached once the choice
 * is made, so none of it competes with the choice itself.
 */
export function ComposeSourceDetailStep({
  draft,
  onDraft,
  credentials,
  credentialFeed,
  probe,
  zipProbe,
  zip,
  siteId,
  clusterDomain,
  locked,
  connect,
  disconnect,
  invalidCredentialId,
  onConnectionInvalid,
  onConnectionNeed,
  app,
  appOwner,
  onAppOwner,
  duplicateOf = null,
}: {
  draft: ComposeDraft;
  onDraft: (patch: Partial<ComposeDraft>) => void;
  credentials: readonly CredentialRow[];
  credentialFeed?: CredentialFeedStatus;
  probe: SourceProbeHandle;
  zipProbe: ArtifactProbeHandle;
  /** The zip's verdict once it has been probed; null before that. */
  zip: ZipVerdict | null;
  /** The draft site, once Analyze has created one on the hand-made path. */
  siteId: string;
  clusterDomain: string;
  /** Chosen once: after Analyze the step is facts, not fields. */
  locked: boolean;
  /** The GitHub connect, held by the page because it is the floor's act. */
  connect: GithubConnectActions;
  disconnect?: CredentialRevokeActions;
  invalidCredentialId?: string;
  onConnectionInvalid?: (credentialId: string) => void;
  /** What the repository step needs before it can go on; see RepositorySource. */
  onConnectionNeed?: (need: ConnectionNeed) => void;
  /** The cluster's GitHub App and where an owner would register one -- held by
   *  the page for the floor's reason, and only passed through here. */
  app?: GithubAppActions;
  appOwner?: GithubAppOwner;
  onAppOwner?: (owner: GithubAppOwner) => void;
  /**
   * The ACTIVE source that already tracks this repository at this ref
   * (2026-09-05 design, D8), when there is one. The engine refuses the second
   * registration; this says so before Analyze, and names the source.
   */
  duplicateOf?: PackageRow | null;
}) {
  if (locked) return <ChosenSource draft={draft} zip={zip} siteId={siteId} clusterDomain={clusterDomain} />;

  return (
    <div className="os-stop-body">
      {draft.choice === "repo" ? (
        <>
          <RepositorySource
            draft={draft}
            onDraft={onDraft}
            credentials={credentials}
            credentialFeed={credentialFeed}
            probe={probe}
            connect={connect}
            disconnect={disconnect}
            invalidCredentialId={invalidCredentialId}
            onConnectionInvalid={onConnectionInvalid}
            onConnectionNeed={onConnectionNeed}
            app={app}
            appOwner={appOwner}
            onAppOwner={onAppOwner}
          />
          {/* ASKED ONCE THERE IS A REPOSITORY TO ASK IT ABOUT. It used to stand
              under an empty picker as two more full-width cards. It is a
              choice row now -- the two answers are short, and the line beneath
              says what the chosen one does. */}
          {draft.repoUrl === "" ? null : (
            <>
              <Field label="When something newer lands">
                <div className="os-choice-row" role="radiogroup" aria-label="Deployment mode">
                  <button type="button" role="radio" className="os-choice" aria-checked={!draft.autoDeploy} onClick={() => onDraft({ autoDeploy: false })}>Manual</button>
                  <button type="button" role="radio" className="os-choice" aria-checked={draft.autoDeploy === true} onClick={() => onDraft({ autoDeploy: true })}>Automatic</button>
                </div>
              </Field>
              <Caption>
                {draft.autoDeploy
                  ? "New versions are checked for and deployed automatically. A changed build plan still waits for your review."
                  : "New versions are checked for automatically. The current one keeps serving until you deploy."}
              </Caption>
            </>
          )}
          {/* ONE SOURCE, ONCE (2026-09-05, D8). The engine refuses a second
              registration of a repository at a ref; this says so here, while
              the URL is still being chosen, and names the source that has it
              -- the same posture the probe's "private, or not there" takes,
              parking the stop rather than letting Analyze fail one round trip
              later. */}
          {duplicateOf === null ? null : (
            <p className="os-stop-verdict" data-tone="warn" role="status">
              This repository at this ref is already tracked by <strong>{duplicateOf.name || sourceLabel(duplicateOf)}</strong>.
              A source is added once -- open that one instead, or archive it first to start over.
            </p>
          )}
        </>
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
            stateExtra={draft.artifactId === row.id ? <span className="os-livelist-tick">chosen</span> : null}
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
