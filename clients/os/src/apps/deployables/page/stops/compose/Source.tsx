import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import { FileArchive } from "lucide-react";

import {
  Caption,
  ChoiceStack,
  Field,
  Input,
  LiveList,
  Notice,
  Row as ListRow,
  Select,
  formatBytes,
  useLiveView,
} from "../../../../../kit";
import { flatten } from "../../../../../kit/rows";
import { CredentialField } from "../../../sources/CredentialField";
import {
  SOURCE_HOST,
  probeNote,
  probeParks,
  probeWantsCredential,
  zipUnusableNote,
  type ZipVerdict,
} from "../../../sources/probe";
import type { ArtifactProbeHandle, SourceProbeHandle } from "../../../sources/useProbes";
import { PICKER_PAGE_SIZE, useZipArtifacts } from "../../../sources/useZipArtifacts";
import type { CredentialRow } from "../../../sources/rows";
import { DEPLOYABLE_KINDS, NOT_OFFERED_SENTENCE } from "../../../targets";
import { suggestName, type ComposeDraft } from "../../compose";
import { CiHandoff } from "./CiHandoff";

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
// THE REPOSITORY PICKER IS NOT HERE, AND THAT IS ON PURPOSE
// ===========================================================================
// GitHub Connect (memql#4912) is a later epic: with a grant, this stop
// becomes a picker over the repositories that grant can see. Until then the
// pasted URL and a personal token are the whole of it, and building half a
// picker now would be building the surface that epic replaces.

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
}) {
  if (locked) return <ChosenSource draft={draft} zip={zip} siteId={siteId} clusterDomain={clusterDomain} />;

  return (
    <div className="os-stop-body">
      <ChoiceStack
        name="os-compose-source"
        label="Where it comes from"
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
        <RepositoryBranch draft={draft} onDraft={onDraft} credentials={credentials} probe={probe} />
      ) : null}
      {draft.choice === "zip" ? (
        <ZipBranch draft={draft} onDraft={onDraft} zipProbe={zipProbe} zip={zip} />
      ) : null}
      {draft.choice === "ci" ? <CiBranch draft={draft} onDraft={onDraft} /> : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// A repository
// ---------------------------------------------------------------------------

function RepositoryBranch({
  draft,
  onDraft,
  credentials,
  probe,
}: {
  draft: ComposeDraft;
  onDraft: (patch: Partial<ComposeDraft>) => void;
  credentials: readonly CredentialRow[];
  probe: SourceProbeHandle;
}) {
  const reply = probe.reply;
  const reason = reply?.reason ?? "";
  const note = reply === null ? "" : probeNote(reply);
  // The credential field appears when GitHub's answer is one a credential
  // could change, and stays once one is chosen: switching between two of
  // your own is exactly what somebody does when the first cannot see it.
  const wantsCredential = probeWantsCredential(reason) || draft.credentialId !== "";

  return (
    <>
      {/* THE PROBE FIRES ON BLUR, and the handler sits on a WRAPPER rather
          than on the input: React's onBlur is `focusout`, which bubbles, and
          the kit's Input carries a visually-hidden label plus no blur prop of
          its own. Adding one to the kit for a single caller would be
          promoting a control on its FIRST use, which the kit's own header
          asks surfaces not to do. */}
      <div onBlur={() => void probe.probe(draft.repoUrl, draft.credentialId)}>
        <Field label="Repository URL">
          <Input
            id="os-compose-repo-url"
            label="The repository this deployable is built from"
            value={draft.repoUrl}
            onChange={(repoUrl) => {
              // A NEW URL MAKES THE OLD ANSWER WRONG, not stale: "private, or
              // not there" beside a URL it was never about is worse than no
              // answer at all.
              probe.clear();
              onDraft({ repoUrl });
            }}
            placeholder={`https://${SOURCE_HOST}/acme/storefront`}
            onEnter={() => void probe.probe(draft.repoUrl, draft.credentialId)}
          />
        </Field>
      </div>

      {probe.busy ? <Caption>Asking whether this cluster can read it...</Caption> : null}
      {/* SAID ONCE (DESIGN.md rule 7). A reason that PARKS the flow is the
          rail's note -- a stopped stop names its reason there -- so the body
          would be repeating the sentence directly beneath it. What the body
          adds is the answer the rail has no room for: the branch a public
          repository will follow, or that a token is working. */}
      {note === "" || probeParks(reason) ? null : (
        <p className="os-stop-verdict" data-tone="ok" role="status">
          {note}
        </p>
      )}
      {/* A PROBE THAT COULD NOT RUN IS NOT AN ANSWER ABOUT THE REPOSITORY.
          The server's sentence renders here, the field stays editable, and
          Analyze stays reachable -- the fetch is the authority. */}
      {probe.error === "" ? null : (
        <Notice
          tone="warn"
          sentence="This cluster could not check the repository just now."
          next="Nothing is wrong with what you typed. Deploying still works: the fetch asks again, and it is the one that decides."
          detail={probe.error}
        />
      )}

      <Field label="Branch or tag">
        <Input
          id="os-compose-repo-ref"
          label="Which branch or tag to deploy"
          value={draft.repoRef}
          onChange={(repoRef) => onDraft({ repoRef })}
          placeholder="the default branch"
        />
      </Field>

      {wantsCredential ? (
        <>
          <CredentialField
            id="os-compose-credential"
            credentials={credentials}
            value={draft.credentialId}
            onChange={(credentialId) => {
              onDraft({ credentialId });
              // THE POINT OF CHOOSING ONE IS TO ASK AGAIN UNDER IT.
              void probe.probe(draft.repoUrl, credentialId);
            }}
          />
          <Caption>
            A private repository is fetched under one of your own credentials. It is read at fetch time, on this
            cluster, and never leaves it.
          </Caption>
        </>
      ) : null}

      <NameField draft={draft} onDraft={onDraft} label="Call it" placeholderFrom={suggestName(draft, "")} />
    </>
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
// The two fields the hand-made paths share
// ---------------------------------------------------------------------------

function NameField({
  draft,
  onDraft,
  label,
  placeholderFrom,
}: {
  draft: ComposeDraft;
  onDraft: (patch: Partial<ComposeDraft>) => void;
  label: string;
  placeholderFrom: string;
}) {
  return (
    <Field label={label}>
      <Input
        id="os-compose-name"
        label="What this deployable is called"
        value={draft.name}
        onChange={(name) => onDraft({ name })}
        placeholder={placeholderFrom || "storefront"}
      />
    </Field>
  );
}

/**
 * The kind, for a deployable this cluster is not analyzing.
 *
 * A package declares each app's kind in its manifest and the report reads it
 * back; a built-site zip and a CI push declare nothing, so the choice is the
 * person's. The one sentence about the three kinds that are NOT offered sits
 * beneath it, said once, in place of three disabled controls.
 */
function KindField({ draft, onDraft }: { draft: ComposeDraft; onDraft: (patch: Partial<ComposeDraft>) => void }) {
  const chosen = DEPLOYABLE_KINDS.find((k) => k.value === draft.kind);
  return (
    <>
      <Field label="What kind">
        <Select
          id="os-compose-kind"
          label="What kind of deployable this is"
          value={draft.kind}
          onChange={(kind) => onDraft({ kind })}
        >
          <option value="">Choose a kind</option>
          {DEPLOYABLE_KINDS.map((kind) => (
            <option key={kind.value} value={kind.value}>
              {kind.label}
            </option>
          ))}
        </Select>
      </Field>
      {chosen ? <Caption>{chosen.blurb}</Caption> : null}
      <Caption>{NOT_OFFERED_SENTENCE}</Caption>
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
