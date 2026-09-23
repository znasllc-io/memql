import { GitBranch } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { useSession } from "../../../../../chrome/access";
import { bare } from "../../../people";
import { Caption, EmptyState, Field, Notice, RefreshButton, Select, Subhead } from "../../../../../kit";
import { toneFor } from "../../../packages/refusals";
import { ProblemNotice } from "../../../packages/ReportView";
import { shortRepo } from "../../../packages/rows";
import { DisconnectGitHub } from "../../../sources/ConnectedAccountCard";
import { RepositoryPicker } from "../../../sources/RepositoryPicker";
import { returnPathFor } from "../../../sources/connectReturn";
import type { RepositoryRow } from "../../../sources/repositories";
import { credentialIsRevoked, githubGrantOf, isGithubAppGrant, type CredentialFeedStatus, type CredentialRow } from "../../../sources/rows";
import { GithubAppOwnerField, OWN_ACCOUNT, SET_UP_SENTENCE, type GithubAppOwner } from "../../../sources/GithubAppSetup";
import type { GithubAppActions } from "../../../sources/useGithubApp";
import { useCredentialRevoke, useGithubConnect, useSourceRepositories, type GithubConnectActions, type CredentialRevokeActions } from "../../../sources/useGithubConnect";
import { probeNote, probeParks } from "../../../sources/probe";
import type { SourceProbeHandle } from "../../../sources/useProbes";
import { suggestName, type ComposeDraft } from "../../compose";
import { NameField } from "./fields";

// Repository creation uses the caller's GitHub App grant. Stored token
// credentials remain available to existing sources and Settings; this flow
// has no token fallback. The page owns connection actions and the selected
// source so they survive responsive step remounts.

/** The section that resumes this repository step after OAuth. */
const COMPOSE_SECTION = "deployables";

interface RepositorySourceProps {
  /** Connection status can stand apart from adding a repository source. */
  showRepositories?: boolean;
  draft: ComposeDraft;
  onDraft: (patch: Partial<ComposeDraft>) => void;
  credentials: readonly CredentialRow[];
  credentialFeed?: CredentialFeedStatus;
  probe: SourceProbeHandle;
  /**
   * The connect, held by the PAGE. Connecting is this step's forward act, and a
   * wizard's forward act lives on its floor and nowhere else -- so the page
   * that draws the floor owns the call, and this step only says what it is for
   * and renders what came back.
   */
  connect: GithubConnectActions;
  disconnect?: CredentialRevokeActions;
  invalidCredentialId?: string;
  onConnectionInvalid?: (credentialId: string) => void;
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
 *                          the floor says who can set it up.
 */
export type ConnectionNeed = "" | "connect" | "reconnect" | "setup" | "unavailable";

export function RepositorySource(props: RepositorySourceProps) {
  const { access } = useSession();
  const localDisconnect = useCredentialRevoke();
  const disconnect = props.disconnect ?? localDisconnect;
  // Owner oversight includes colleagues' cards; using a source is personal.
  const viewer = bare(access?.userId ?? "");
  const personal = props.credentials.filter(c => viewer !== "" && bare(c.ownerUserId) === viewer);
  const grant = githubGrantOf(personal);
  // A different viewer or grant gets fresh reads, errors and installation
  // help. An earlier request cannot land in the next person's picker.
  const activeGrantId = grant && !credentialIsRevoked(grant) && disconnect.revokedCredentialId !== grant.id ? grant.id : "";
  useEffect(() => { if (!props.disconnect) localDisconnect.clear(); }, [viewer, grant?.id, props.disconnect, localDisconnect.clear]);
  const previousGrantId = useRef(activeGrantId);
  // A source selected under another account or a revoked grant cannot carry
  // its URL, branch or asynchronous probe into the next connection.
  useEffect(() => {
    const selected = props.draft.credentialId;
    const selectedWithGithub = selected !== "" && (previousGrantId.current === selected ||
      props.credentials.some(c => c.id === selected && isGithubAppGrant(c)));
    previousGrantId.current = activeGrantId;
    if (selectedWithGithub && selected !== activeGrantId) {
      props.probe.clear();
      props.onDraft({ repoUrl: "", repoRef: "", credentialId: "" });
    }
  }, [activeGrantId, props.credentials, props.draft.credentialId, props.probe.clear, props.onDraft]);
  const feed = props.credentialFeed;
  const waiting = feed && (feed.state !== "live" || feed.error !== "");
  return <>
    {feed?.error ? <Notice tone="error" sentence="Your source connections could not be read." detail={feed.error} /> : null}
    {!grant && waiting ? <EmptyState icon={GitBranch}
      title={feed.state === "seeding" ? "Loading your source connections" : "Source connections are unavailable"}
      action={feed.state === "seeding" ? undefined : <RefreshButton label="Read source connections again" onClick={feed.retry} />}>
      {feed.state === "seeding" ? "Checking your saved GitHub connection." : "Reconnect to the cluster or read your connections again."}
    </EmptyState> : <PersonalRepositorySource {...props} credentials={personal} disconnect={disconnect}
      key={`${viewer}:${grant?.id ?? ""}:${grant?.status ?? ""}`} />}
  </>;
}

function PersonalRepositorySource({
  draft, onDraft, credentials, probe, connect, onConnectionNeed,
  app, appOwner = OWN_ACCOUNT, onAppOwner, disconnect, invalidCredentialId = "", onConnectionInvalid, showRepositories = true,
}: RepositorySourceProps & { disconnect: CredentialRevokeActions }) {
  const install = useGithubConnect();
  const repositories = useSourceRepositories();
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);

  const grant = githubGrantOf(credentials);
  const disconnected = grant !== null && disconnect.revokedCredentialId === grant.id;
  // A LAPSED GRANT IS NOT A CONNECTION. It cannot read a repository list, so
  // the picker would answer an empty invitation to somebody whose repair is
  // one click; the Connect control below says "Reconnect" for them instead.
  const connected = grant !== null && !credentialIsRevoked(grant) && !disconnected;
  const grantId = connected ? grant.id : "";
  const returnPath = returnPathFor(COMPOSE_SECTION);
  const authRefusals = ["credential_not_found", "credential_revoked", "reconnect_required"];
  const [refusedGrant, setRefusedGrant] = useState("");
  const probeRefused = draft.credentialId === grant?.id && authRefusals.includes(probe.reply?.reason ?? "");
  useEffect(() => {
    if (probeRefused && grant) {
      setRefusedGrant(grant.id);
      onConnectionInvalid?.(grant.id);
    }
  }, [probeRefused, grant?.id, onConnectionInvalid]);
  const reconnect = authRefusals.includes(repositories.refusal?.code ?? "") || probeRefused ||
    (grant !== null && (refusedGrant === grant.id || invalidCredentialId === grant.id));

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
    if (!showRepositories || grantId === "" || grantId === readFor.current) return;
    let current = true;
    void read(grantId, 1).then(answered => {
      if (current && answered) readFor.current = grantId;
    });
    return () => { current = false; };
  }, [grantId, read, showRepositories]);

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

  async function disconnectAccount() {
    if (!grant || !await disconnect.revoke(grant.id) || !mounted.current) return;
    repositories.clear();
    install.clear();
    connect.clear();
    probe.clear();
    onDraft({ repoUrl: "", repoRef: "", credentialId: "" });
  }

  useEffect(() => {
    if (reconnect && draft.credentialId === grant?.id) {
      probe.clear();
      onDraft({ repoUrl: "", repoRef: "", credentialId: "" });
    }
  }, [reconnect, draft.credentialId, grant?.id, probe.clear, onDraft]);

  // THE CLUSTER HAS NO APP, AND THIS TIME IT IS KNOWN BEFORE ANYBODY PRESSES.
  // Only a status that ANSWERED "not configured" counts: null is "not known",
  // and not known keeps Connect on offer (`useGithubApp`).
  const appMissing = noApp !== null || (app?.status != null && !app.status.configured);
  const maySetup = appMissing && app?.status?.canSetup === true;

  // Said on every change and WITHDRAWN on the way out: a need that outlived
  // its step would leave "Connect GitHub" on the floor of the step after it.
  const need: ConnectionNeed =
    appMissing
        ? maySetup ? "setup" : "unavailable"
        : connected && !reconnect
          ? ""
          : grant === null ? "connect" : "reconnect";
  useEffect(() => {
    onConnectionNeed?.(need);
    return () => onConnectionNeed?.("");
  }, [need, onConnectionNeed]);

  return (
    <>
      <Subhead>GitHub</Subhead>
      {noApp ? <ProblemNotice problem={noApp} tone={toneFor(noApp.code)} /> : null}
      {appMissing ? (
        <GithubNotSetUp app={app} maySetup={maySetup} owner={appOwner} onOwner={onAppOwner} />
      ) : connected && !reconnect ? (
        <>
          <DisconnectGitHub compact summary={<Caption>Connected to GitHub as @{grant.login || "unknown"}.</Caption>}
            busy={disconnect.busy} refusal={disconnect.refusal} onDisconnect={() => void disconnectAccount()} />
          {showRepositories ? <RepositoryPicker
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
          /> : null}
          {showRepositories && draft.repoUrl !== "" ? (
            <>
              {probe.busy ? <Caption>Checking the repository…</Caption> : null}
              {probe.reply && !probeParks(probe.reply.reason) && probeNote(probe.reply) ?
                <p className="os-stop-verdict" data-tone={probe.reply.reason === "ok" ? "ok" : "warn"} role="status">{probeNote(probe.reply)}</p> : null}
              {probe.error ? <Notice tone="warn" sentence="This cluster could not check the repository just now."
                detail={probe.error} next="Retry the check or continue to analysis.">
                <RefreshButton label="Check repository again" busy={probe.busy} onClick={() => void probe.probe(draft.repoUrl, draft.credentialId)} />
              </Notice> : null}
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
            {disconnected
              ? "Your GitHub connection is disconnected. Reconnect to choose an account; you come back to this step."
              : grant === null
              ? "Connect your GitHub account and pick from a list of your repositories. You come back to this step."
              : "Your GitHub connection has lapsed. Reconnect to pick from your repositories; you come back to this step."}
          </Caption>
          <Caption>Reconnecting opens GitHub’s account chooser. Choose another account there, or add an account if it is not listed. Your GitHub App installations stay in place.</Caption>
          {disconnect.remoteRevoked === false ? <Notice tone="warn" sentence="Disconnected from MemQL." detail="GitHub did not confirm that your personal authorization ended. You can remove it under Applications in your GitHub settings; this does not require uninstalling the app." /> : null}
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
 * disabled), so it says who can set it up.
 */
function GithubNotSetUp({ app, maySetup, owner, onOwner }: {
  app?: GithubAppActions;
  maySetup: boolean;
  owner: GithubAppOwner;
  onOwner?: (owner: GithubAppOwner) => void;
}) {
  if (!maySetup) {
    return (
      <Caption>
        This cluster is not linked to GitHub yet. Ask a cluster owner to set it up before choosing a repository.
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
      {app?.refusal ? <ProblemNotice problem={app.refusal} tone={toneFor(app.refusal.code)} /> : null}
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
