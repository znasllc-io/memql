import { useEffect, useRef, type ReactNode } from "react";

import { Button, Caption, Field, Select, Subhead } from "../../../../../kit";
import { toneFor } from "../../../packages/refusals";
import { ProblemNotice } from "../../../packages/ReportView";
import { shortRepo } from "../../../packages/rows";
import { ConnectGitHub } from "../../../sources/ConnectGitHub";
import { RepositoryPicker } from "../../../sources/RepositoryPicker";
import { returnPathFor } from "../../../sources/connectReturn";
import type { RepositoryRow } from "../../../sources/repositories";
import { credentialIsRevoked, githubGrantOf, type CredentialRow } from "../../../sources/rows";
import { useGithubConnect, useSourceRepositories } from "../../../sources/useGithubConnect";
import type { SourceProbeHandle } from "../../../sources/useProbes";
import { suggestName, type ComposeDraft } from "../../compose";
import { NameField } from "./fields";
import { TokenSourceForm } from "./TokenSourceForm";

// The repository answer, in its three readings (epic memql#4915, design
// sections A and C; the mount point Compose left for it in `Source.tsx`).
//
// ===========================================================================
// THE SAME QUESTION, ANSWERED BY WHAT THIS PERSON ALREADY HAS
// ===========================================================================
// "Where does this come from" has one answer and three ways of giving it,
// and which one a person sees is decided by what the cluster and they hold:
//
//   * a CONNECTION -- the picker is the answer. Choosing a repository fills
//     the URL, the credential and the branch list at once, and the token form
//     drops behind "Use a token instead".
//   * NO connection, and a cluster that has a GitHub App -- Connect above,
//     the same fold below it, closed.
//   * NO GitHub App on this cluster -- the token form IS the stop, with the
//     server's own sentence saying why. This is the reading every cluster
//     without an app gets, and it is what this surface did before Connect
//     existed.
//
// ===========================================================================
// NOTHING IS ASKED OF THE CLUSTER UNTIL SOMEBODY HOLDS A CONNECTION
// ===========================================================================
// `githubConnectBegin` MINTS A STATE ROW, so it is never called to find out
// whether this cluster has an app -- that is what makes the third reading a
// consequence of a click rather than of opening the stop. A person with no
// grant sees Connect and the fold, presses one of them, and learns from the
// answer.
//
// The picker's list, by contrast, is a READ, and it runs on its own the
// moment a connected person opens this stop: the measure of this surface is
// that they never notice it, and a list that made them press "Look again"
// before showing anything would be a surface announcing itself.
//
// ===========================================================================
// ONE REF FIELD AND ONE NAME FIELD, EVER
// ===========================================================================
// `TokenSourceForm` carries its own ref and name, so the picker's pair renders
// only while the fold is CLOSED. Two controls writing one draft field, side by
// side, is two answers to one question -- and in markup it is two inputs with
// the same accessible name, which is the version a screen reader gets.

/** The section a connect from this stop comes back to: the list, which is
 *  where New deployable is. */
const COMPOSE_SECTION = "deployables";

export function RepositorySource({
  draft,
  onDraft,
  credentials,
  probe,
  tokenFormOpen,
  onTokenFormOpenChange,
}: {
  draft: ComposeDraft;
  onDraft: (patch: Partial<ComposeDraft>) => void;
  credentials: readonly CredentialRow[];
  probe: SourceProbeHandle;
  /** The fold's state, held by the page so it survives a stop re-render. */
  tokenFormOpen: boolean;
  onTokenFormOpenChange: (open: boolean) => void;
}) {
  const connect = useGithubConnect();
  const repositories = useSourceRepositories();

  const grant = githubGrantOf(credentials);
  // A LAPSED GRANT IS NOT A CONNECTION. It cannot read a repository list, so
  // the picker would answer an empty invitation to somebody whose repair is
  // one click; the Connect control below says "Reconnect" for them instead.
  const connected = grant !== null && !credentialIsRevoked(grant);
  const grantId = connected ? grant.id : "";
  const returnPath = returnPathFor(COMPOSE_SECTION);

  // THE CLUSTER HAS NO GITHUB APP, and only a refused begin can say so.
  // The control is not rendered at all in this reading: a disabled Connect
  // would invite a person to fix something that is not theirs to fix, and
  // the copy for this code already tells them what to do instead.
  const noApp =
    connect.refusal !== null && connect.refusal.code === "github_app_not_configured" ? connect.refusal : null;

  // THE LIST, READ ONCE PER CONNECTION. Keyed on the grant's id in a ref
  // rather than in the dependency list, for the reason the Sources group
  // states about its install link: the hook's `read` is stable only while the
  // connection is, and depending on it alone would re-read every time the
  // socket redialled.
  const readFor = useRef("");
  const read = repositories.read;
  useEffect(() => {
    if (grantId === "" || grantId === readFor.current) return;
    readFor.current = grantId;
    void read("", 1);
  }, [grantId, read]);

  /**
   * Choosing a repository answers three fields at once.
   *
   * The REF is cleared rather than kept: a branch that existed in the last
   * repository is not evidence about this one, and a ref that does not exist
   * fails at fetch with a sentence about a branch nobody typed. The NAME is
   * only suggested where there is none, which is `ZipBranch`'s posture and
   * for its reason -- re-deriving one somebody edited would undo their typing.
   */
  function choose(repo: RepositoryRow) {
    onDraft({
      repoUrl: repo.url,
      repoRef: "",
      credentialId: grantId,
      name: draft.name || suggestName({ ...draft, choice: "repo", repoUrl: repo.url }, ""),
    });
    // ASK ABOUT THE ONE THAT WAS JUST CHOSEN. The probe is what answers the
    // branches this stop offers and the manifest the next stop previews, and
    // the old answer is about a different repository.
    probe.clear();
    void probe.probe(repo.url, grantId);
  }

  if (noApp !== null) {
    return (
      <>
        {/* THE TONE IS READ FROM THE CODE, never fixed to error: a cluster
            with no GitHub App is an operator's condition and this person's
            next step, and the fault colour would say they broke it
            (`toneFor`). */}
        <ProblemNotice problem={noApp} tone={toneFor(noApp.code)} />
        <TokenSourceForm draft={draft} onDraft={onDraft} credentials={credentials} probe={probe} />
      </>
    );
  }

  return (
    <>
      {connected ? (
        <>
          <RepositoryPicker
            page={repositories.page}
            readAt={repositories.readAt}
            busy={repositories.busy}
            refusal={repositories.refusal}
            installUrl={connect.installUrl}
            /* The chosen row is derived from the URL the draft holds rather
               than from a second field: `shortRepo` is the same reading the
               rail's own Source answer uses, so the mark and the note can
               never disagree. */
            chosen={draft.repoUrl === "" ? "" : shortRepo(draft.repoUrl)}
            idPrefix="os-compose-repo"
            onChoose={choose}
            onLookAgain={() => void repositories.read("", 1)}
            onReadMore={() => void repositories.read("", repositories.page.nextPage)}
          />
          {draft.repoUrl !== "" && !tokenFormOpen ? (
            <>
              <RefField draft={draft} onDraft={onDraft} branches={probe.reply?.branches ?? []} />
              <NameField draft={draft} onDraft={onDraft} label="Call it" placeholderFrom={suggestName(draft, "")} />
            </>
          ) : null}
        </>
      ) : (
        <ConnectGitHub
          label={grant === null ? "Connect GitHub" : "Reconnect GitHub"}
          caption={
            grant === null
              ? "Pick repositories from a list instead of pasting a URL and a token."
              : "This connection was disconnected. Reconnecting puts the picker back."
          }
          busy={connect.busy}
          refusal={connect.refusal}
          onConnect={() => void connect.connect(returnPath)}
        />
      )}

      <TokenFold open={tokenFormOpen} onOpenChange={onTokenFormOpenChange}>
        <TokenSourceForm draft={draft} onDraft={onDraft} credentials={credentials} probe={probe} />
      </TokenFold>
    </>
  );
}

/**
 * "Use a token instead": the disclosure, in `ZipPicker`'s shape.
 *
 * NOT "ADVANCED". A pasted URL and a personal token are a legitimate first
 * choice -- a host the app does not cover, an organization that will not
 * install one, or a preference -- and calling that advanced would be a
 * judgement about the person rather than a fact about the choice. It is
 * always present, never inside a menu, and closed by default because the
 * surface above it is the answer this cluster recommends.
 *
 * STATE-LIFTED for `ZipPicker`'s reason: the page owns it, so the fold
 * survives everything that re-renders this stop -- a probe answering, a
 * credential arriving on its feed -- rather than closing under somebody
 * mid-sentence.
 */
function TokenFold({
  open,
  onOpenChange,
  children,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  children: ReactNode;
}) {
  if (!open) {
    return (
      <div className="os-form-row">
        <Button tone="quiet" onClick={() => onOpenChange(true)}>
          Use a token instead
        </Button>
        <Caption>Paste a repository URL, and for a private one a token you hold.</Caption>
      </div>
    );
  }
  return (
    <div className="os-files-group">
      <Subhead>A URL and a token</Subhead>
      {children}
      <div className="os-form-row">
        {/* THE LABEL SAYS WHAT A CLICK DOES, which is the shell's rule for
            every control that toggles a thing (DESIGN.md rule 3's reasoning,
            applied to a disclosure). */}
        <Button tone="quiet" onClick={() => onOpenChange(false)}>
          Hide the token form
        </Button>
      </div>
    </div>
  );
}

/**
 * Which branch or tag, from the branches the probe answered.
 *
 * DEFAULT FIRST, AND THAT ORDER IS THE ENGINE'S (`probeBranches`): re-sorting
 * here would bury the one branch most people want under whatever sorts first.
 *
 * "Follow the default branch" IS A DIFFERENT ANSWER from picking the default
 * branch by name, which is why both are offered and neither is the other's
 * label: an empty ref is resolved at every fetch, so a repository that
 * renames its default keeps deploying, while a pinned name goes on meaning
 * that name. The draft's empty value has always meant the first of the two.
 *
 * WITH NO BRANCHES THERE IS NO PICKER. A probe that answered none -- no
 * grant, or a GitHub that would not list them -- leaves the ref to the form
 * behind the fold rather than offering an empty select, which is a control
 * that can only be wrong.
 */
function RefField({
  draft,
  onDraft,
  branches,
}: {
  draft: ComposeDraft;
  onDraft: (patch: Partial<ComposeDraft>) => void;
  branches: readonly string[];
}) {
  if (branches.length === 0) return null;
  return (
    <Field label="Branch or tag">
      <Select
        id="os-compose-repo-branch"
        label="Which branch or tag to deploy"
        value={draft.repoRef}
        onChange={(repoRef) => onDraft({ repoRef })}
      >
        <option value="">Follow the default branch</option>
        {branches.map((branch) => (
          <option key={branch} value={branch}>
            {branch}
          </option>
        ))}
      </Select>
    </Field>
  );
}
