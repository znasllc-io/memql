import { useCallback, useState } from "react";

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
  connect: (returnPath: string) => Promise<void>;
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

export function useGithubConnect(): GithubConnectActions {
  const { busy, refusal, clear, run } = useWrite();
  const [installUrl, setInstallUrl] = useState("");

  const connect = useCallback(
    async (returnPath: string) => {
      const begun = await run((query) => githubConnectBegin(query, returnPath));
      if (begun === null) return;
      if (begun.installUrl !== "") setInstallUrl(begun.installUrl);
      if (begun.authorizeUrl === "") return;
      window.location.assign(begun.authorizeUrl);
    },
    [run],
  );

  const learn = useCallback(
    async (returnPath: string) => {
      const begun = await run((query) => githubConnectBegin(query, returnPath));
      if (begun === null) return false;
      setInstallUrl(begun.installUrl);
      return true;
    },
    [run],
  );

  return { busy, refusal, clear, installUrl, connect, learn };
}

export interface SourceRepositoriesActions extends WriteState {
  /** What the last read answered. Never null: an unread picker and one that
   *  answered nothing both render the same empty invitation. */
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
  read: (credentialId: string, page: number) => Promise<void>;
}

export function useSourceRepositories(): SourceRepositoriesActions {
  const { busy, refusal, clear, run } = useWrite();
  const [page, setPage] = useState<RepositoryPage>(EMPTY_PAGE);
  const [readAt, setReadAt] = useState("");

  const read = useCallback(
    async (credentialId: string, wanted: number) => {
      const answered = await run((query) => readSourceRepositories(query, credentialId, wanted));
      // A REFUSED READ KEEPS THE LAST GOOD LIST. A refusal is not a zero
      // (clients/os/README.md): blanking the picker would say the grant
      // reaches nothing, which is a different and untrue answer.
      if (answered === null) return;
      setPage((held) =>
        wanted > 1
          ? {
              ...answered,
              repositories: [...held.repositories, ...answered.repositories],
              // The walk's page 1 is the one that named the installations
              // and the pending organisations; a later page repeats them,
              // and taking the newer answer keeps one reading rather than
              // merging two.
            }
          : answered,
      );
      setReadAt(new Date().toISOString());
    },
    [run],
  );

  return { busy, refusal, clear, page, readAt, read };
}

export interface CredentialRevokeActions extends WriteState {
  revoke: (credentialId: string) => Promise<void>;
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
 * Nothing is removed locally on success. The row flips to `revoked` and
 * arrives on the credential feed's own broadcast, which is what makes the
 * card and the list agree with every other browser looking at it.
 */
export function useCredentialRevoke(): CredentialRevokeActions {
  const { busy, refusal, clear, run } = useWrite();
  const [remoteRevoked, setRemoteRevoked] = useState<boolean | null>(null);
  return {
    busy,
    refusal,
    clear,
    remoteRevoked,
    revoke: async (credentialId) => {
      const answered = await run((query) => revokeSourceCredential(query, credentialId));
      // A REFUSED REVOKE ANSWERS NOTHING ABOUT GITHUB. `run` returns null for
      // one, and recording `false` there would say the authorization is still
      // standing when nothing was attempted -- beside a refusal that already
      // says what happened.
      if (answered !== null) setRemoteRevoked(answered);
    },
  };
}
