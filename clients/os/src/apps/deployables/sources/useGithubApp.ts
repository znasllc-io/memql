import { useCallback, useEffect, useRef, useState } from "react";

import { useOsConnection } from "../../../live/connection";
import { useWrite, type WriteState } from "../packages/actions";
import { githubAppRemove, githubAppSetupBegin, readGithubAppStatus, type GithubAppStatus } from "./calls";

// The cluster's GitHub App, as a surface needs it (engine design record
// 2026-09-20-github-app-setup).
//
// ===========================================================================
// KNOWING BEFORE OFFERING
// ===========================================================================
// Connect GitHub needs the cluster to have a GitHub App, and the only way a
// surface could learn that it did not was to press Connect and be refused --
// so the wizard offered a button and then took it back, with a sentence telling
// a CLUSTER OWNER to "ask an operator". This reads the answer when the surface
// opens, so what is offered is what can be done: Connect when there is an app,
// Set up GitHub when there is none and this person may give it one, and the
// token path -- with the reason -- when there is none and they may not.
//
// ===========================================================================
// NULL MEANS "NOT KNOWN", AND NOT KNOWN MEANS THE OLD BEHAVIOUR
// ===========================================================================
// `status` is null until the cluster answers, and STAYS null when it does not
// -- a browser still dialling, an engine older than the capability. A caller
// treats null as "assume there is an app": Connect is offered, and a cluster
// that turns out to have none refuses it in place, exactly as before this hook
// existed. The alternative -- reading "did not answer" as "no app" -- would
// take Connect away from every cluster whose status read merely failed.

export interface GithubAppActions extends WriteState {
  /** What the cluster said, or null while it has not. */
  status: GithubAppStatus | null;
  /**
   * Begin registering the app and NAVIGATE THE WHOLE PAGE to the cluster's own
   * page that hands the manifest to GitHub. `window.location.assign`, for
   * `useGithubConnect`'s reason: this flow ends by coming back to this origin.
   * A refused begin navigates nowhere and leaves the refusal on the hook.
   */
  setup: (returnPath: string, organization: string) => Promise<void>;
  /** Remove the app registered from this product, then read the status again. */
  remove: () => Promise<boolean>;
  /** Ask again -- after a return from GitHub, say. */
  refresh: () => void;
}

/** The sentence beneath the headline for a reason the engine ANSWERED. The
 *  headline and the next step for each code live in `packages/refusals.ts`. */
function reasonSentence(reason: string): string {
  switch (reason) {
    case "github_app_managed_by_environment":
      return "The deployment's environment sets this cluster's GitHub App.";
    case "github_app_setup_forbidden":
      return "Only a cluster owner can register or remove the cluster's GitHub App.";
    case "github_app_setup_invalid":
      return "That is not a GitHub organization's login.";
    case "github_app_not_configured":
      return "This cluster has no GitHub App registered from here.";
    case "connect_state_invalid":
      return "The cluster could not store the setup state.";
    default:
      return `The cluster answered ${JSON.stringify(reason)}.`;
  }
}

export function useGithubApp(): GithubAppActions {
  const connection = useOsConnection();
  const { busy, refusal, clear, run } = useWrite();
  const [status, setStatus] = useState<GithubAppStatus | null>(null);
  const [asked, setAsked] = useState(0);
  const query = connection?.query ?? null;

  // READ ONCE PER CONNECTION, and again when asked. A failed read leaves the
  // status null on purpose (see the header): it is an enhancement over a
  // surface that already tells the truth when pressed.
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  useEffect(() => {
    if (query === null) return;
    let current = true;
    void readGithubAppStatus(query)
      .then((answer) => {
        if (current && alive.current) setStatus(answer);
      })
      .catch(() => {
        if (current && alive.current) setStatus(null);
      });
    return () => {
      current = false;
    };
  }, [query, asked]);

  const refresh = useCallback(() => setAsked((n) => n + 1), []);

  const setup = useCallback(
    async (returnPath: string, organization: string) => {
      const begun = await run(async (q) => {
        const answer = await githubAppSetupBegin(q, returnPath, organization.trim());
        if (answer.reason !== "" && answer.reason !== "ok") {
          // A begin that ANSWERS a reason is a refusal, raised here in the
          // `<code>: <sentence>` shape `describe` reads -- `useGithubConnect`'s
          // rule, so the same notice lands beside the same kind of control.
          throw new Error(`${answer.reason}: ${reasonSentence(answer.reason)}`);
        }
        return answer;
      });
      if (begun === null || begun.startUrl === "") return;
      window.location.assign(begun.startUrl);
    },
    [run],
  );

  const remove = useCallback(async () => {
    const done = await run(async (q) => {
      const answer = await githubAppRemove(q);
      if (!answer.removed) {
        throw new Error(`${answer.reason}: ${reasonSentence(answer.reason)}`);
      }
      return true;
    });
    if (done) refresh();
    return done === true;
  }, [run, refresh]);

  return { busy, refusal, clear, status, setup, remove, refresh };
}
