import { useCallback, useState } from "react";
import { newShortId, rowNumber, rowString } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { STOREFRONT_KIND } from "./concepts";
import { hostnameFor } from "./hostname";
import { describePublishFailure } from "./publishRefusal";

// The two writes this app makes, each with its own busy/error pair.
//
// ===========================================================================
// TWO HOOKS, NOT ONE, BECAUSE THE ERRORS RENDER IN TWO PLACES
// ===========================================================================
// A refusal belongs beside the control that produced it -- never a toast --
// and the create form and the publish picker are on two different sections. A
// shared busy/error pair would put a hostname refusal under a publish button
// somebody was looking at, which is worse than no message: it names a failure
// they did not cause.
//
// ===========================================================================
// NOTHING HERE CHECKS A ROLE, AND NOTHING HERE IS THE AUTHORIZATION
// ===========================================================================
// `createSite` stamps `ownerUserId` from the verified actor and the Go hostname
// policy decides every claim; `sitePublishFromArtifact` re-resolves both rows
// under the caller's own actor before it reads a byte. The manifest's admin
// gate on the Actions section is presentation (spec section E) -- editing a
// boolean in a browser changes none of it.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** Blank strings are OMITTED rather than sent: an explicit "" would write one. */
function omitBlank(value: string): string | undefined {
  const trimmed = value.trim();
  return trimmed === "" ? undefined : trimmed;
}

// ---------------------------------------------------------------------------
// Create a deployable
// ---------------------------------------------------------------------------

export interface CreateSiteInput {
  /** The hostname LABEL, not the hostname -- the domain is the cluster's. */
  slug: string;
  kind: string;
  title: string;
  /** Storefront only. */
  storeDomain: string;
  storefrontTokenRef: string;
}

export interface CreateSiteState {
  busy: boolean;
  /** The server's own sentence, verbatim. "" when the last attempt succeeded. */
  error: string;
  /** The id of the last site created here, so the surface can say which. */
  createdId: string;
  create: (input: CreateSiteInput, domain: string) => Promise<string>;
  reset: () => void;
}

export function useCreateSite(): CreateSiteState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [createdId, setCreatedId] = useState("");

  const create = useCallback(
    async (input: CreateSiteInput, domain: string): Promise<string> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was written.");
        return "";
      }
      const hostname = hostnameFor(input.slug, domain);
      if (hostname === "") {
        // A composed hostname needs both halves, and the domain half is the
        // cluster's answer rather than anything typed here. Saying so beats
        // sending a hostname we know is wrong and rendering the refusal.
        setError(
          domain.trim() === ""
            ? "This cluster did not tell the shell which domain it serves, so a hostname cannot be composed here. An operator can create the deployable directly."
            : "Give the deployable a name first.",
        );
        return "";
      }

      const siteId = newShortId();
      setBusy(true);
      setError("");
      setCreatedId("");
      try {
        await query.createSite({
          siteId,
          hostname,
          kind: omitBlank(input.kind),
          // `bundleRef` is required -- the schema has no "nothing published
          // yet" state -- so a brand-new deployable takes the placeholder
          // prefix and stays in `draft`. A draft answers 404 BEFORE any file
          // lookup (component/edge/handler.go), so the placeholder is never
          // opened; publishing from the Library replaces it with a real
          // content-addressed version.
          bundleRef: `blob://sites/${siteId}/pending/`,
          status: "draft",
          title: omitBlank(input.title),
          ...(input.kind === STOREFRONT_KIND
            ? {
                binding: {
                  storeDomain: input.storeDomain.trim(),
                  storefrontTokenRef: input.storefrontTokenRef.trim(),
                },
              }
            : {}),
        });
        setCreatedId(siteId);
        // NOTHING IS INSERTED LOCALLY. `graph.node.created.v1:platform:site`
        // is broadcast to browser subscribers, so the row arrives on the same
        // feed the list and the map already draw -- with the arrival cue,
        // exactly like a site somebody else created. A local insert would put
        // a row on screen that the cluster had not confirmed, and the two
        // would then differ in whatever the optimistic copy guessed wrong.
        return siteId;
      } catch (err: unknown) {
        // VERBATIM. The two rules a browser cannot mirror -- cluster-wide
        // uniqueness and the cluster-owner exemption -- are refused
        // server-side, and their messages name the colliding site and the
        // rule. A friendlier paraphrase would drop the one fact that helps.
        setError(describe(err));
        return "";
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return {
    busy,
    error,
    createdId,
    create,
    reset: () => {
      setError("");
      setCreatedId("");
    },
  };
}

// ---------------------------------------------------------------------------
// Publish a Library artifact to a deployable
// ---------------------------------------------------------------------------

export interface PublishOutcome {
  version: string;
  bundleRef: string;
  fileCount: number;
  totalBytes: number;
}

export interface PublishState {
  busy: boolean;
  /** A refusal, already turned into a sentence somebody can act on. */
  error: string;
  outcome: PublishOutcome | null;
  publish: (siteId: string, artifactId: string) => Promise<boolean>;
  reset: () => void;
}

export function usePublish(): PublishState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [outcome, setOutcome] = useState<PublishOutcome | null>(null);

  const publish = useCallback(
    async (siteId: string, artifactId: string): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was published.");
        return false;
      }
      if (siteId === "" || artifactId === "") {
        setError("Pick a bundle first.");
        return false;
      }
      setBusy(true);
      setError("");
      setOutcome(null);
      try {
        // The bytes never touch this browser: the cluster reads them from its
        // own object storage, validates the zip and hands it to the same
        // edge.Publisher a CI publish goes through. That is why there is no
        // progress fraction here -- there is nothing local to measure.
        const result = await query.sitePublishFromArtifact({ siteId, artifactId });
        const row = result.rows()[0] ?? null;
        setOutcome({
          version: rowString(row, "version"),
          bundleRef: rowString(row, "bundleRef"),
          fileCount: rowNumber(row, "fileCount"),
          totalBytes: rowNumber(row, "totalBytes"),
        });
        return true;
      } catch (err: unknown) {
        setError(describePublishFailure(err));
        return false;
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return {
    busy,
    error,
    outcome,
    publish,
    reset: () => {
      setError("");
      setOutcome(null);
    },
  };
}

// ---------------------------------------------------------------------------
// The client tie (epic memql#4800, D5)
// ---------------------------------------------------------------------------

export interface SiteAccountState {
  busy: boolean;
  /** The server's own sentence, verbatim. "" when the last attempt worked. */
  error: string;
  /** Pass "" to clear the tie. */
  setAccount: (siteId: string, accountId: string) => Promise<boolean>;
}

/**
 * Point a deployable at a client, or at nobody.
 *
 * ITS OWN BUSY/ERROR PAIR, for the reason the two hooks above have theirs: a
 * refusal belongs beside the control that produced it, and the picker is on
 * the site DETAIL while create and publish are elsewhere. A shared pair would
 * put a tie refusal under a publish button.
 *
 * `updateSiteAccount` is a single-purpose write that stamps the field, so ""
 * clears the tie rather than inheriting the stored value -- see the mutation
 * for why that is stamped rather than accepted.
 */
export function useSiteAccount(): SiteAccountState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const setAccount = useCallback(
    async (siteId: string, accountId: string): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was written.");
        return false;
      }
      setBusy(true);
      setError("");
      try {
        await query.updateSiteAccount({ siteId, accountId });
        // NOTHING IS UPDATED LOCALLY. v1:platform:site broadcasts `updated`,
        // so the row arrives on the feed the list, the map and this panel all
        // read -- which is also what makes the tie appear on somebody else's
        // screen without a reload.
        return true;
      } catch (err: unknown) {
        setError(describe(err));
        return false;
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return { busy, error, setAccount };
}
