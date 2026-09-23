import { useCallback, useEffect, useRef, useState } from "react";

import { useSession } from "../../../chrome/access";
import { bare } from "../people";
import { rememberConnectAttempt } from "./connectReturn";
import { useWrite, type WriteState } from "../packages/actions";
import { githubConnectBegin, readSourceRepositories, revokeSourceCredential } from "./calls";
import { EMPTY_PAGE, type RepositoryPage } from "./repositories";

// The GitHub Connect surfaces' write hooks (epic memql#4915).
//
// ONE HOOK PER SURFACE, exactly as `packages/actions.ts` states it, and for
// the same reason: a refusal belongs beside the control that produced it. The
// connected-account card and the pasted-credential list each call
// `useCredentialRevoke` for themselves, so "this cluster refused" never
// appears under a button somebody was not looking at.
//
// Everything here is built on `useWrite`, which is where `describe` turns a
// thrown engine error into a code and the server's own sentence. A second
// refusal-parsing path is the thing this import exists to prevent.

export interface GithubConnectActions extends WriteState {
  /** The app's installation page, once a call has answered one. Empty until
   *  then, and empty forever on a cluster with no GitHub App. */
  installUrl: string;
  /**
   * Begin a connect flow and NAVIGATE THE WHOLE PAGE to GitHub.
   *
   * `window.location.assign`, which is the AuthProvider's sign-in convention
   * and not an accident: a popup is a thing browsers block and a thing a
   * person can lose behind a window, and this flow ends by coming back to
   * this origin either way.
   *
   * A refused begin navigates NOWHERE and leaves the refusal on the hook, so
   * the surface renders it in place. An answered call with an empty URL is
   * treated the same way -- there is nothing to navigate to, and sending a
   * browser to "" would reload the OS and look like the button did nothing.
   */
  connect: (returnPath: string, credentialId?: string) => Promise<void>;
  /**
   * Begin WITHOUT navigating, to learn `installUrl`.
   *
   * The installation link has to be a real anchor with a real href -- it
   * opens GitHub in another tab -- so the URL must be in hand before anybody
   * clicks, and this call is the only thing that answers it. It is asked
   * once, by a surface that already knows the person is connected, which is
   * the only place the link is offered.
   *
   * ANSWERS WHETHER IT RAN. A caller that guards itself with "already asked"
   * has to be able to tell a call that was answered from one the cluster
   * never saw -- a browser that had not finished dialling when the surface
   * mounted would otherwise mark the question asked and never ask it, and
   * the link would be missing for the rest of the session.
   */
  learn: (returnPath: string) => Promise<boolean>;
}

/**
 * The sentence beneath the OS headline for a reason the engine ANSWERED
 * rather than threw. Written here because the engine's `connectResult`
 * carries the code alone -- the person-facing copy for each code lives in
 * `packages/refusals.ts`, and this is only the detail line under it.
 */
export function reasonSentence(reason: string): string {
  switch (reason) {
    case "github_app_not_configured":
      return "This cluster has no GitHub App configured.";
    case "credential_not_found":
      return "Connect your GitHub account to choose its repositories.";
    case "credential_revoked":
    case "reconnect_required":
      return "Reconnect your GitHub account to renew repository access.";
    case "rate_limited":
      return "GitHub temporarily limited repository requests. Try again shortly.";
    case "connect_state_invalid":
      return "The cluster could not store the connect state.";
    default:
      return `The cluster answered ${JSON.stringify(reason)}.`;
  }
}

export function useGithubConnect(): GithubConnectActions {
  const { access } = useSession();
  const viewer = bare(access?.userId ?? "");
  const { refusal, clear: clearWrite, run } = useWrite();
  const [installUrl, setInstallUrl] = useState("");
  const [busy, setBusy] = useState(false);
  const generation = useRef(0);
  useEffect(() => () => { generation.current++; }, []);
  const clearWriteRef = useRef(clearWrite);
  clearWriteRef.current = clearWrite;
  const clear = useCallback(() => {
    generation.current++;
    setBusy(false);
    setInstallUrl("");
    clearWriteRef.current();
  }, []);

  // A BEGIN THAT ANSWERS A REASON IS A REFUSAL. The engine never throws for
  // "this cluster has no GitHub App" or "the state row could not be written":
  // it answers the row with an empty `authorizeUrl` and the reason as a typed
  // code (integrations/identity/githubconnect.go, `connectResult`). `useWrite`
  // only records what is THROWN, so the reason is raised here, inside `run`,
  // in the `<code>: <sentence>` shape `describe` reads -- which is what puts
  // the same ProblemNotice under the button that a thrown refusal gets.
  // Before this, a not-configured cluster's
  // Connect button went busy for one round trip and then did nothing at all.
  const begin = useCallback(async (returnPath: string, credentialId?: string, flowId?: string) => {
    const request = ++generation.current;
    setBusy(true);
    const answer = await run(async query => {
      try {
        const begun = await githubConnectBegin(query, returnPath, credentialId, flowId);
        if (request !== generation.current) return null;
        if (begun.installUrl !== "") setInstallUrl(begun.installUrl);
        if (begun.reason !== "" && begun.reason !== "ok") {
          throw new Error(`${begun.reason}: ${reasonSentence(begun.reason)}`);
        }
        return begun;
      } catch (error) {
        if (request !== generation.current) return null;
        throw error;
      }
    });
    if (request !== generation.current) return null;
    setBusy(false);
    return answer;
  }, [run]);

  const connect = useCallback(
    async (returnPath: string, credentialId?: string) => {
      const flowId = crypto.randomUUID();
      const begun = await begin(returnPath, credentialId, flowId);
      if (begun === null) return;
      if (begun.authorizeUrl === "") return;
      rememberConnectAttempt(flowId, viewer, credentialId);
      window.location.assign(begun.authorizeUrl);
    },
    [begin, viewer],
  );

  const learn = useCallback(
    async (returnPath: string) => {
      const begun = await begin(returnPath);
      return begun !== null;
    },
    [begin],
  );

  return { busy, refusal, clear, installUrl, connect, learn };
}

export interface SourceRepositoriesActions extends WriteState {
  /** Last successful reading; readAt distinguishes unread from empty. */
  page: RepositoryPage;
  /** When the list was read, as an ISO instant. Empty = never read. */
  readAt: string;
  /**
   * Read a page.
   *
   * PAGE 1 REPLACES AND ANY OTHER APPENDS, which is what makes "Look again"
   * and "Read more" two different acts on one call: looking again is asking
   * the same question over, and reading more is continuing a walk. Appending
   * on a re-read would show every repository twice.
   */
  read: (credentialId: string, page: number, connectionId?: string) => Promise<boolean>;
}

export function useSourceRepositories(): SourceRepositoriesActions {
  const { refusal, clear: clearWrite, run } = useWrite();
  const [busy, setBusy] = useState(false);
  const [page, setPage] = useState<RepositoryPage>(EMPTY_PAGE);
  const [readAt, setReadAt] = useState("");
  const latestRead = useRef(0);
  useEffect(() => () => { latestRead.current++; }, []);
  const credential = useRef("");
  const clearWriteRef = useRef(clearWrite);
  clearWriteRef.current = clearWrite;
  const clear = useCallback(() => {
    latestRead.current++;
    credential.current = "";
    setBusy(false);
    setPage(EMPTY_PAGE);
    setReadAt("");
    clearWriteRef.current();
  }, []);

  const read = useCallback(
    async (credentialId: string, wanted: number, connectionId?: string) => {
      const boundary = JSON.stringify([credentialId, connectionId ?? ""]);
      const changedCredential = credential.current !== boundary;
      if (changedCredential) {
        clear();
        credential.current = boundary;
      }
      const request = ++latestRead.current;
      setBusy(true);
      const answered = await run(async (query) => {
        try {
          const result = await readSourceRepositories(query, credentialId, wanted, connectionId);
          if (request !== latestRead.current) return null;
          if (result.reason !== "" && result.reason !== "ok") {
            throw new Error(`${result.reason}: ${reasonSentence(result.reason)}`);
          }
          return result;
        } catch (error) {
          if (request !== latestRead.current) return null;
          throw error;
        }
      });
      // A REFUSED READ KEEPS THE LAST GOOD LIST. A refusal is not a zero
      // (clients/os/README.md): blanking the picker would say the grant
      // reaches nothing, which is a different and untrue answer.
      if (request !== latestRead.current) return false;
      setBusy(false);
      if (answered === null) return false;
      setPage((held) =>
        wanted > 1 && !changedCredential
          ? {
              ...answered,
              repositories: [...held.repositories, ...answered.repositories],
              // The walk's page 1 is the one that named the installations
              // and the pending organizations; a later page repeats them,
              // and taking the newer answer keeps one reading rather than
              // merging two.
            }
          : answered,
      );
      setReadAt(new Date().toISOString());
      return true;
    },
    [run, clear],
  );

  return { busy, refusal, clear, page, readAt, read };
}

export interface CredentialRevokeActions extends WriteState {
  /** True once local revocation succeeds, including an unconfirmed remote revoke. */
  revoke: (credentialId: string) => Promise<boolean>;
  revokedCredentialId: string;
  /**
   * Whether the last revoke also ended the authorization AT GITHUB, or null
   * while nothing has been revoked in this session.
   *
   * THREE STATES, NOT TWO. The engine flips the local row even when the
   * GitHub half failed, so a disconnect that answers `false` succeeded here
   * and left something standing there -- which is the one case a person has
   * more to do. `null` is what keeps that sentence off a card nobody has
   * pressed anything on.
   */
  remoteRevoked: boolean | null;
}

/**
 * Revoke a credential.
 *
 * CALLED ONCE PER SURFACE rather than shared: the connected-account card's
 * Disconnect and the pasted list's Revoke are two controls in one settings
 * group, and one busy flag across both would grey out a button nobody
 * pressed while showing its refusal underneath.
 *
 * Success is returned immediately so a surface can stop offering the credential
 * while its revoked row is still travelling over the live feed.
 */
export function useCredentialRevoke(): CredentialRevokeActions {
  const { busy, refusal, clear: clearWrite, run } = useWrite();
  const [remoteRevoked, setRemoteRevoked] = useState<boolean | null>(null);
  const [revokedCredentialId, setRevokedCredentialId] = useState("");
  const generation = useRef(0);
  useEffect(() => () => { generation.current++; }, []);
  const clearWriteRef = useRef(clearWrite);
  clearWriteRef.current = clearWrite;
  const clear = useCallback(() => {
    generation.current++;
    setRemoteRevoked(null);
    setRevokedCredentialId("");
    clearWriteRef.current();
  }, []);
  return {
    busy, refusal, clear, remoteRevoked, revokedCredentialId,
    revoke: async credentialId => {
      const request = ++generation.current;
      const answered = await run(async query => {
        try {
          return await revokeSourceCredential(query, credentialId);
        } catch (error) {
          if (request !== generation.current) return null;
          throw error;
        }
      });
      if (answered === null || request !== generation.current) return false;
      setRemoteRevoked(answered);
      setRevokedCredentialId(credentialId);
      return true;
    },
  };
}
