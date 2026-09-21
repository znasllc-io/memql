import { GitBranch } from "lucide-react";
import { useEffect, useRef } from "react";

import { useSession } from "../../../../../chrome/access";
import { bare } from "../../../people";
import { Caption, EmptyState, Field, Notice, RefreshButton, Select } from "../../../../../kit";
import { toneFor } from "../../../packages/refusals";
import { ProblemNotice } from "../../../packages/ReportView";
import { shortRepo } from "../../../packages/rows";
import { RepositoryPicker } from "../../../sources/RepositoryPicker";
import { returnPathFor } from "../../../sources/connectReturn";
import type { RepositoryRow } from "../../../sources/repositories";
import { credentialIsRevoked, githubGrantOf, type CredentialFeedStatus, type CredentialRow } from "../../../sources/rows";
import { GithubAppOwnerField, OWN_ACCOUNT, SET_UP_SENTENCE, type GithubAppOwner } from "../../../sources/GithubAppSetup";
import type { GithubAppActions } from "../../../sources/useGithubApp";
import { useGithubConnect, useSourceRepositories, type GithubConnectActions } from "../../../sources/useGithubConnect";
import type { SourceProbeHandle } from "../../../sources/useProbes";
import { suggestName, type ComposeDraft } from "../../compose";
import { NameField } from "./fields";
import { TokenSourceForm } from "./TokenSourceForm";

// The repository answer, in its readings (epic memql#4915, design sections A
// and C; the mount point Compose left for it in `Source.tsx`).
//
// ===========================================================================
// THE SAME QUESTION, ANSWERED BY WHAT THIS PERSON ALREADY HAS
// ===========================================================================
// "Where does this come from" has one answer and several ways of giving it,
// and which one a person sees is decided by what the cluster and they hold:
//
//   * a CONNECTION -- the picker is the answer. Choosing a repository fills
//     the URL, the credential and the branch list at once.
//   * NO connection, and a cluster that has a GitHub App -- what connecting is
//     for, and Connect GitHub on the floor.
//   * NO GitHub App on this cluster -- said BEFORE anybody presses anything.
//     A cluster owner is asked the one question registering an app has, with
//     Set up GitHub on the floor; anybody else is told who can, and that a
//     token works meanwhile (`GithubNotSetUp`).
//   * no app, and NOBODY COULD SAY SO IN ADVANCE -- an engine that predates
//     the status call. Connect is offered, the cluster refuses it, and the
//     token form becomes the stop under the server's own sentence. This is
//     what every cluster without an app got before the status existed.
//
// ===========================================================================
// WHAT IS ASKED OF THE CLUSTER, AND WHEN
// ===========================================================================
// `githubConnectBegin` MINTS A STATE ROW, so it is never called to find out
// whether this cluster has an app. `githubAppStatus` is a READ and mints
// nothing, so the page asks it as the wizard opens (`useGithubApp`) -- which is
// what turned "press Connect and learn the cluster cannot" into a step that
// knows before it offers.
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

/** The section that resumes this repository step after OAuth. */
const COMPOSE_SECTION = "deployables";

interface RepositorySourceProps {
  draft: ComposeDraft;
  onDraft: (patch: Partial<ComposeDraft>) => void;
  credentials: readonly CredentialRow[];
  credentialFeed?: CredentialFeedStatus;
  probe: SourceProbeHandle;
  /**
   * Which of the two ways in is chosen -- held by the page, so the choice
   * survives everything that re-renders this step (a probe answering, a
   * credential arriving on its feed) rather than flipping back under somebody
   * mid-sentence. `true` is the token.
   */
  tokenFormOpen: boolean;
  onTokenFormOpenChange: (open: boolean) => void;
  /**
   * The connect, held by the PAGE. Connecting is this step's forward act, and a
   * wizard's forward act lives on its floor and nowhere else -- so the page
   * that draws the floor owns the call, and this step only says what it is for
   * and renders what came back.
   */
  connect: GithubConnectActions;
  /**
   * What this step needs before it can go on, said to the page that draws the
   * floor: nothing, a first connection, or a fresh one.
   *
   * THE STEP SAYS IT RATHER THAN THE PAGE WORKING IT OUT, because only the
   * step knows all of it. A grant that looks fine on its card can still have
   * been refused by GitHub (`reconnect_required`, `credential_revoked`), and
   * that answer arrives here, on the repository read. A page that judged by
   * the card alone would show a lapsed connection's sentence over a floor with
   * nothing to press.
   */
  onConnectionNeed?: (need: ConnectionNeed) => void;
  /**
   * The cluster's GitHub App, read by the PAGE when the wizard opened -- so
   * this step knows whether Connect can work BEFORE it offers it. Absent, or
   * with a null status, means "not known", which is read as "there is an app":
   * Connect is offered and a cluster with none refuses it in place, as it did
   * before the status could be asked for (`useGithubApp`).
   */
  app?: GithubAppActions;
  /** Whose GitHub account the app is registered under, held by the page
   *  because the floor's Set up GitHub needs it. */
  appOwner?: GithubAppOwner;
  onAppOwner?: (owner: GithubAppOwner) => void;
}

/**
 * What the step needs before it can go on.
 *
 *   connect / reconnect -- this person's GitHub connection.
 *   setup               -- the CLUSTER has no GitHub App, and this person may
 *                          register one. The floor's act, like the other two.
 *   unavailable         -- the cluster has none and this person may not. There
 *                          is NO act for it (rule 12: absent, never disabled);
 *                          the floor says why, and the token path is one choice
 *                          away.
 */
export type ConnectionNeed = "" | "connect" | "reconnect" | "setup" | "unavailable";

export function RepositorySource(props: RepositorySourceProps) {
  const { access } = useSession();
  // Owner oversight includes colleagues' cards; using a source is personal.
  const viewer = bare(access?.userId ?? "");
  const personal = props.credentials.filter(c => viewer !== "" && bare(c.ownerUserId) === viewer);
  const grant = githubGrantOf(personal);
  // A different viewer or grant gets fresh reads, errors and installation
  // help. An earlier request cannot land in the next person's picker.
  const feed = props.credentialFeed;
  const waiting = feed && (feed.state !== "live" || feed.error !== "");
  return <>
    {feed?.error ? <Notice tone="error" sentence="Your source connections could not be read." detail={feed.error} /> : null}
    {!grant && waiting ? <EmptyState icon={GitBranch}
      title={feed.state === "seeding" ? "Loading your source connections" : "Source connections are unavailable"}
      action={feed.state === "seeding" ? undefined : <RefreshButton label="Read source connections again" onClick={feed.retry} />}>
      {feed.state === "seeding" ? "Checking your saved GitHub connection." : "Reconnect to the cluster or read your connections again."}
    </EmptyState> : <PersonalRepositorySource {...props} credentials={personal}
      key={`${viewer}:${grant?.id ?? ""}:${grant?.status ?? ""}`} />}
  </>;
}

function PersonalRepositorySource({
  draft, onDraft, credentials, probe, tokenFormOpen, onTokenFormOpenChange, connect, onConnectionNeed,
  app, appOwner = OWN_ACCOUNT, onAppOwner,
}: RepositorySourceProps) {
  const install = useGithubConnect();
  const repositories = useSourceRepositories();

  const grant = githubGrantOf(credentials);
  // A LAPSED GRANT IS NOT A CONNECTION. It cannot read a repository list, so
  // the picker would answer an empty invitation to somebody whose repair is
  // one click; the Connect control below says "Reconnect" for them instead.
  const connected = grant !== null && !credentialIsRevoked(grant);
  const grantId = connected ? grant.id : "";
  const returnPath = returnPathFor(COMPOSE_SECTION);
  const reconnect = ["credential_not_found", "credential_revoked", "reconnect_required"].includes(repositories.refusal?.code ?? "");

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
    let current = true;
    void read(grantId, 1).then(answered => {
      if (current && answered) readFor.current = grantId;
    });
    return () => { current = false; };
  }, [grantId, read]);

  const learnedFor = useRef("");
  const learning = useRef(false);
  const learn = install.learn;
  useEffect(() => {
    if (!grantId || grantId === learnedFor.current || learning.current) return;
    learning.current = true;
    void learn(returnPath).then(answered => {
      learning.current = false;
      if (answered) learnedFor.current = grantId;
    });
  }, [grantId, learn, returnPath]);

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

  const method = tokenFormOpen ? "token" : "github";

  // THE CLUSTER HAS NO APP, AND THIS TIME IT IS KNOWN BEFORE ANYBODY PRESSES.
  // Only a status that ANSWERED "not configured" counts: null is "not known",
  // and not known keeps Connect on offer (`useGithubApp`).
  const appMissing = app?.status != null && !app.status.configured;
  const maySetup = appMissing && app?.status?.canSetup === true;

  // Said on every change and WITHDRAWN on the way out: a need that outlived
  // its step would leave "Connect GitHub" on the floor of the step after it.
  const need: ConnectionNeed =
    method !== "github" || noApp !== null
      ? ""
      : appMissing
        ? maySetup ? "setup" : "unavailable"
        : connected && !reconnect
          ? ""
          : grant === null ? "connect" : "reconnect";
  useEffect(() => {
    onConnectionNeed?.(need);
    return () => onConnectionNeed?.("");
  }, [need, onConnectionNeed]);

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
      {/* ONE QUESTION, ASKED AS ONE. This step used to open on a "Connect
          GitHub" button with a "Use a token instead" button beneath it --
          two loose controls wedged between two stacks of choice cards, the
          second of which unfolded a form with a "Hide the token form" button
          at its foot. They were always the two answers to a single question,
          so they are one choice now, in the shell's choice row: neither
          needs a card's sentence to be understood, and the line beneath says
          what the chosen one does. */}
      <Field label="How this cluster reaches it">
        <div className="os-choice-row" role="radiogroup" aria-label="How this cluster reaches it">
          <button type="button" role="radio" className="os-choice" aria-checked={method === "github"} onClick={() => onTokenFormOpenChange(false)}>GitHub</button>
          <button type="button" role="radio" className="os-choice" aria-checked={method === "token"} onClick={() => onTokenFormOpenChange(true)}>A token</button>
        </div>
      </Field>

      {method === "github" && appMissing ? (
        <GithubNotSetUp app={app!} maySetup={maySetup} owner={appOwner} onOwner={onAppOwner} />
      ) : method === "token" ? (
        <>
          {/* NOT "ADVANCED". A pasted URL and a personal token are a legitimate
              first choice -- a host the app does not cover, an organization
              that will not install one, or a preference -- and it stands
              beside GitHub as an equal rather than behind a disclosure. */}
          <Caption>Paste a repository URL, and for a private one a token you hold.</Caption>
          <TokenSourceForm draft={draft} onDraft={onDraft} credentials={credentials} probe={probe} />
        </>
      ) : connected && !reconnect ? (
        <>
          <RepositoryPicker
            page={repositories.page}
            readAt={repositories.readAt}
            busy={repositories.busy}
            refusal={repositories.refusal}
            installUrl={install.installUrl}
            /* The chosen row is derived from the URL the draft holds rather
               than from a second field: `shortRepo` is the same reading the
               rail's own Source answer uses, so the mark and the note can
               never disagree. */
            chosen={draft.repoUrl === "" ? "" : shortRepo(draft.repoUrl)}
            idPrefix="os-compose-repo"
            onChoose={choose}
            onLookAgain={() => void repositories.read(grantId, 1)}
            onReadMore={() => void repositories.read(grantId, repositories.page.nextPage)}
          />
          {draft.repoUrl !== "" ? (
            <>
              <RefField draft={draft} onDraft={onDraft} branches={probe.reply?.branches ?? []} />
              <NameField draft={draft} onDraft={onDraft} label="Call it" placeholderFrom={suggestName(draft, "")} />
            </>
          ) : null}
        </>
      ) : (
        <>
          {/* NOT CONNECTED: what connecting is FOR, and nothing to press here.
              The act is on the floor, where every step's forward act is. */}
          <Caption>
            {grant === null
              ? "Connect your GitHub account and pick from a list of your repositories. You come back to this step."
              : "Your GitHub connection has lapsed. Reconnect to pick from your repositories; you come back to this step."}
          </Caption>
          {/* IN PLACE, NEVER A TOAST, and in the tone the CODE asks for
              (`toneFor`): a refusal to connect is rendered where the person
              was when they asked. */}
          {(connect.refusal ?? repositories.refusal) ? <ProblemNotice problem={(connect.refusal ?? repositories.refusal)!} tone={toneFor((connect.refusal ?? repositories.refusal)!.code)} /> : null}
        </>
      )}
    </>
  );
}

/**
 * GitHub is chosen and the CLUSTER has no GitHub App.
 *
 * This used to be found out by pressing Connect: the cluster refused, and the
 * step swapped itself for a notice telling a cluster owner to "ask an operator"
 * -- the one person reading it who IS the operator. Now it is known when the
 * wizard opens, so the step says what is true and offers what can be done.
 *
 * FOR A CLUSTER OWNER it is one question -- whose GitHub account the app is
 * registered under -- and the act is the floor's, Set up GitHub, like every
 * forward act in this wizard. GitHub asks the real question on its own page
 * (it shows the app it is about to create); this only has to say what that
 * trip is for and that it ends back here.
 *
 * FOR ANYBODY ELSE there is nothing to press (rule 12: absent, never
 * disabled), so it says who can change it and what works meanwhile -- and the
 * token is one choice away, in the row above.
 */
function GithubNotSetUp({ app, maySetup, owner, onOwner }: {
  app: GithubAppActions;
  maySetup: boolean;
  owner: GithubAppOwner;
  onOwner?: (owner: GithubAppOwner) => void;
}) {
  if (!maySetup) {
    return (
      <Caption>
        This cluster is not linked to GitHub yet. A cluster owner sets that up once; until then, choose A token.
      </Caption>
    );
  }
  return (
    <>
      {/* "You come back to this step" is the wizard's own reassurance, the one
          the Connect reading gives: leaving for GitHub does not lose what was
          answered so far. */}
      <Caption>{SET_UP_SENTENCE} You come back to this step.</Caption>
      <GithubAppOwnerField owner={owner} onOwner={(next) => onOwner?.(next)} idPrefix="os-compose-github-app" />
      {/* IN PLACE, in the tone the CODE asks for -- where the person was when
          they asked. */}
      {app.refusal ? <ProblemNotice problem={app.refusal} tone={toneFor(app.refusal.code)} /> : null}
    </>
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
