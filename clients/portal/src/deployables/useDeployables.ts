import { useCallback, useEffect, useMemo, useState } from "react";
import {
  newShortId,
  rowNumber,
  rowString,
  type Role,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { useAuth } from "../auth/AuthProvider";
import { omitBlank } from "../cluster/args";
import { useCluster } from "../cluster/ClusterProvider";
import { clusterDomainFor } from "../cluster/editorLink";
import { useMyAccess } from "../cluster/useMyAccess";
import { fetchSiteVersionHistory, type SiteVersion } from "./calls";
import { STOREFRONT_KIND, isZipArtifact } from "./concepts";
import { hostnameFor } from "./hostname";
import { describePublishFailure } from "./publishRefusal";

// The deployables surface's hooks: the list + create, the deploy picker's
// artifact set, one deployable's detail and its five actions, and the version
// walk behind rollback. One module, the same choice artifacts/useArtifacts.ts
// makes (contrast the sites/ tree this replaces, which split three hooks
// across three files for state that is read together on two screens).
//
// ===========================================================================
// AUTHORIZATION IS NOT A SCREEN ANY MORE, AND THAT IS THE CHANGE (memql#4344)
// ===========================================================================
//
// v1:platform:site used to be @rowAuthz(clusterOwner): a non-owner's read came
// back empty, so the page rendered an explanation instead of a table and
// sites/SitesRefused.tsx existed to say so. The concept now declares the
// COMPOSITE tier, @rowAuthz(owner="ownerUserId", clusterOwner) -- the owner, OR
// a cluster owner -- so an ordinary caller has deployables of their own and
// there is nothing to refuse. The refusal component is deleted rather than
// ported, because the sentence it renders is now false.
//
// What the role still decides is one COLUMN. sitesAll's filter is
// `ownerUserId==actor.userId || actor.isClusterOwner==true`, so a cluster
// owner's list is every deployable in the cluster and an ordinary caller's is
// their own -- same call, same page, different population. Naming the owner is
// informative only in the first case, which is why isClusterOwner is read here
// at all.
//
// ===========================================================================
// WHY THE NAMED QUERY AND NOT useConceptRows
// ===========================================================================
//
// The generic concept browse would now be correctly scoped (row admission
// applies to it too), and it would still be the wrong read: sitesAll carries
// `isNotDeleted`, and deleteSite is a SOFT delete. A generic browse cannot
// express that conjunct, so a deployable somebody deleted would sit in the list
// looking live -- and clicking it would open a detail page whose own read
// (siteById, which carries the same conjunct) answers "no deployable has that
// id". It also carries the hostname sort and the siteFull projection.
//
// The cost is the live CDC band the Sites list had. Writes bump an epoch and
// re-run the read instead, the shape useArtifacts.ts and useCampaigns.ts use --
// so a deployable created here still appears without a manual refresh, it just
// arrives on the write's own settle rather than on a subscription.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// -------------------- the LIST screen --------------------

export interface CreateDeployableInput {
  // The hostname LABEL, not the hostname. The domain is the cluster's and is
  // never typed -- see hostname.ts.
  slug: string;
  kind: string;
  title: string;
  // Storefront only, both required when kind is shopify_storefront.
  storeDomain: string;
  storefrontTokenRef: string;
}

export interface DeployablesState {
  rows: Row[];
  loading: boolean;
  error: string;
  reload: () => void;
  role: Role;
  isClusterOwner: boolean;
  // The domain this cluster serves, for composing and previewing a hostname.
  // "" when the serving node is old enough not to publish it AND the identity
  // URL is not the derivable shape -- the create form says so rather than
  // guessing.
  domain: string;
  createBusy: boolean;
  createError: string;
  // The id of the deployable the last successful create made, so the page can
  // navigate to it. Deploying is the next thing anyone does.
  createdId: string;
  createDeployable: (input: CreateDeployableInput) => void;
}

export function useDeployables(): DeployablesState {
  const { query } = useCluster();
  const { config } = useAuth();
  const { access, loading: accessLoading } = useMyAccess();
  const [rows, setRows] = useState<Row[]>([]);
  // Starts true: a read is effectively in flight from mount, so "loading" is
  // the honest initial state -- not "confirmed empty", which is what false
  // would claim before any read has been attempted. See the effect below for
  // why a null query must not push it back to false.
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);
  const [createBusy, setCreateBusy] = useState(false);
  const [createError, setCreateError] = useState("");
  const [createdId, setCreatedId] = useState("");

  useEffect(() => {
    if (query === null) {
      // Not yet connected -- NOT a definitive "no deployables" answer. Leave
      // `loading` as it is rather than asserting emptiness before a read has
      // even been attempted; `query` is genuinely null for at least the first
      // render of every mount, so forcing false here would paint the empty
      // state on every single visit.
      return;
    }
    let live = true;
    setLoading(true);
    setError("");

    void query
      .sitesAll({})
      .then((result) => {
        if (live) setRows(result.rows());
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, epoch]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);
  const domain = useMemo(() => clusterDomainFor(config), [config]);

  const createDeployable = useCallback(
    (input: CreateDeployableInput) => {
      if (query === null) return;
      const siteId = newShortId();
      const hostname = hostnameFor(input.slug, domain);
      if (hostname === "") {
        setCreateError(
          "This cluster did not tell the console which domain it serves, so a hostname cannot be " +
            "composed here. An operator can create the deployable directly.",
        );
        return;
      }
      setCreateBusy(true);
      setCreateError("");
      setCreatedId("");
      void query
        .createSite({
          siteId,
          hostname,
          kind: omitBlank(input.kind),
          // createSite requires a bundleRef -- there is no "nothing published
          // yet" state in the schema -- so a brand-new deployable takes the
          // placeholder prefix docs/public/operate/site-hosting.md already
          // documents for exactly this case, and stays in `draft`. A draft
          // answers 404 BEFORE any file lookup (component/edge/handler.go), so
          // the placeholder is never opened; deploying from the Library
          // replaces it with a real content-addressed version.
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
        })
        .then(() => {
          setCreatedId(siteId);
          setEpoch((n) => n + 1);
        })
        // The server's message is kept VERBATIM here rather than translated.
        // The two rules a browser cannot mirror -- cluster-wide uniqueness and
        // the cluster-owner exemption -- are refused server-side, and their
        // messages name the colliding site and the rule; a friendlier
        // paraphrase would drop the one fact the person needs.
        .catch((err: unknown) => setCreateError(describe(err)))
        .finally(() => setCreateBusy(false));
    },
    [query, domain],
  );

  const role: Role = access?.clusterRole ?? "";
  return {
    rows,
    // Access resolving is part of loading here rather than a separate gate:
    // nothing on this page is WITHHELD from a role, so the only thing waiting
    // buys is not flashing an owner column that is about to disappear.
    loading: loading || accessLoading,
    error,
    reload,
    role,
    isClusterOwner: role === "owner",
    domain,
    createBusy,
    createError,
    createdId,
    createDeployable,
  };
}

// -------------------- the deploy picker's artifacts --------------------

export interface ZipArtifactsState {
  rows: Row[];
  loading: boolean;
  error: string;
}

// The caller's deployable bundles: their Library, narrowed to file artifacts
// with a zip MIME type.
//
// FILTERED CLIENT-SIDE, and that is a fact about the query surface rather than
// a shortcut. libraryArtifacts already excludes archived rows and is already
// owner-scoped; there is no facet query for "kind=file AND a zip mimeType"
// (libraryArtifactsByKind takes a kind and nothing else), and a page-sized read
// re-narrowed by kind alone would still hand back PDFs. The list a person picks
// from is small either way.
export function useZipArtifacts(): ZipArtifactsState {
  const { query } = useCluster();
  const [rows, setRows] = useState<Row[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  useEffect(() => {
    if (query === null) return;
    let live = true;
    setLoading(true);
    setError("");

    void query
      .libraryArtifacts({})
      .then((result) => {
        if (live) setRows(result.rows().filter(isZipArtifact));
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query]);

  return { rows, loading, error };
}

// -------------------- the DETAIL screen --------------------

// What a successful deploy produced. Rendered rather than logged: "deployed"
// with no version is indistinguishable from "nothing happened", and the version
// is the handle rollback uses.
export interface PublishOutcome {
  version: string;
  bundleRef: string;
  fileCount: number;
  totalBytes: number;
}

export interface DeployableDetailState {
  site: Row | null;
  loading: boolean;
  error: string;
  actionMessage: string;
  actionError: string;
  busy: boolean;
  // Set while sitePublishFromArtifact is in flight. Distinct from `busy`
  // because the deploy band says something specific about what is happening --
  // the cluster is reading and expanding a zip, which is a wait with no
  // browser-side progress to report.
  publishBusy: boolean;
  publishOutcome: PublishOutcome | null;
  // True once deleteSite has succeeded. The page navigates away on this --
  // there is nothing left at this address to show.
  deleted: boolean;
  // Bumped by every settled action. The history walk keys off it, so a deploy
  // puts its own version into the rollback list without a reload.
  epoch: number;
  reload: () => void;
  publishFromArtifact: (artifactId: string) => void;
  publishBundleRef: (bundleRef: string) => void;
  setStatus: (status: string) => void;
  remove: () => void;
}

export function useDeployableDetail(siteId: string): DeployableDetailState {
  const { query } = useCluster();
  const [site, setSite] = useState<Row | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [actionMessage, setActionMessage] = useState("");
  const [actionError, setActionError] = useState("");
  const [busy, setBusy] = useState(false);
  const [publishBusy, setPublishBusy] = useState(false);
  const [publishOutcome, setPublishOutcome] = useState<PublishOutcome | null>(null);
  const [deleted, setDeleted] = useState(false);
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (siteId === "") {
      // Structurally nothing to fetch, ever, given the current route -- a
      // genuine terminal state, unlike the connection race below.
      setSite(null);
      setLoading(false);
      setError("");
      return;
    }
    if (query === null) return;
    let live = true;
    setLoading(true);
    setError("");

    // Read through the NAMED siteById query rather than the generic
    // getRowByConceptAndId: siteById is the seam the rollback walk wraps in
    // asOf(), so reading the CURRENT row through the same query keeps "the
    // current state" and "version one of the history" from being two code
    // paths that can disagree.
    void query
      .siteById({ siteId })
      .then((result) => {
        if (live) setSite(result.rows()[0] ?? null);
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, siteId, epoch]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  // run funnels the two non-terminal row writes through one place so busy /
  // message handling and the follow-up re-read cannot be forgotten on the
  // second one. remove() is deliberately NOT run through this -- a successful
  // delete has no row left to reload.
  const run = useCallback((what: Promise<unknown> | null, done: string) => {
    if (what === null) return;
    setBusy(true);
    setActionMessage("");
    setActionError("");
    void what
      .then(() => setActionMessage(done))
      .catch((err: unknown) => setActionError(describe(err)))
      .finally(() => {
        setBusy(false);
        setEpoch((n) => n + 1);
      });
  }, []);

  // Deploy from the Library. The bytes never touch this browser: the cluster
  // reads them from its own object storage, validates the zip and hands it to
  // the same edge.Publisher a CI publish goes through. That is why there is no
  // progress fraction here and an upload has one -- there is nothing local to
  // measure.
  const publishFromArtifact = useCallback(
    (artifactId: string) => {
      if (query === null || siteId === "" || artifactId === "") return;
      setPublishBusy(true);
      setActionMessage("");
      setActionError("");
      setPublishOutcome(null);
      void query
        .sitePublishFromArtifact({ siteId, artifactId })
        .then((result) => {
          const row = result.rows()[0] ?? null;
          setPublishOutcome({
            version: rowString(row, "version"),
            bundleRef: rowString(row, "bundleRef"),
            fileCount: rowNumber(row, "fileCount"),
            totalBytes: rowNumber(row, "totalBytes"),
          });
          setEpoch((n) => n + 1);
        })
        // The one call site that TRANSLATES its error, because this one alone
        // carries a stable machine-readable reason. See publishRefusal.ts.
        .catch((err: unknown) => setActionError(describePublishFailure(err)))
        .finally(() => setPublishBusy(false));
    },
    [query, siteId],
  );

  const publishBundleRef = useCallback(
    (bundleRef: string) =>
      run(
        query ? query.updateSiteBundle({ siteId, bundleRef }) : null,
        "Published. The edge resolves the new bundle on its next cache miss for this hostname.",
      ),
    [query, run, siteId],
  );

  const setStatus = useCallback(
    (status: string) =>
      run(query ? query.updateSiteStatus({ siteId, status }) : null, `Status set to ${status}.`),
    [query, run, siteId],
  );

  const remove = useCallback(() => {
    if (query === null) return;
    setBusy(true);
    setActionMessage("");
    setActionError("");
    void query
      .deleteSite({ siteId })
      .then(() => {
        setActionMessage("Deleted.");
        setDeleted(true);
      })
      .catch((err: unknown) => setActionError(describe(err)))
      .finally(() => setBusy(false));
  }, [query, siteId]);

  return {
    site,
    loading,
    error,
    actionMessage,
    actionError,
    busy,
    publishBusy,
    publishOutcome,
    deleted,
    epoch,
    reload,
    publishFromArtifact,
    publishBundleRef,
    setStatus,
    remove,
  };
}

// -------------------- the rollback picker's versions --------------------

export interface DeployableHistoryState {
  versions: SiteVersion[];
  loading: boolean;
  error: string;
}

// The rollback picker's data: calls.ts's asOf walk, wired to the connection and
// re-run whenever `epoch` changes.
//
// The epoch is a PARAMETER rather than an internal reload() the page has to
// remember to call. That is the bug this shape closes: the sites version worked
// the other way, exposed reload(), and no caller ever invoked it -- so a
// version somebody had just published was absent from its own rollback list
// until the page was reloaded by hand.
export function useDeployableHistory(siteId: string, epoch: number): DeployableHistoryState {
  const { query } = useCluster();
  const [versions, setVersions] = useState<SiteVersion[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  useEffect(() => {
    if (siteId === "") {
      setVersions([]);
      setLoading(false);
      setError("");
      return;
    }
    if (query === null) return;
    let live = true;
    setLoading(true);
    setError("");

    void fetchSiteVersionHistory(query, siteId)
      .then((next) => {
        if (live) setVersions(next);
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, siteId, epoch]);

  return { versions, loading, error };
}
