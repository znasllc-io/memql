// One goal's world: seeded from the authorized reads, kept current by CDC.
//
// ===========================================================================
// EVERY EVENT IS RESOLVED THROUGH THE AUTHORIZED READ (design D6)
// ===========================================================================
// Whether an event arrives with a payload or as an id-only notification with
// `payload_omitted`, this hook re-reads the row through
// getRowByConceptAndId -- the same read useRowDetail performs, the same one
// the concept browser's live band uses since memql#4310. One code path, and
// the branch that would have trusted a payload does not exist to be forgotten.
//
// That costs a round trip per event, which is the right trade here for two
// reasons stated rather than assumed. A goal's event rate is low (a busy plan
// is low hundreds of rows over minutes, not a stream), and `plan` is heading
// for the `granted` tier (memql#4366), which is the id-only case -- so the
// expensive path is the one that will be taken for most events anyway. Making
// it the only path now means the tier landing changes nothing here.
//
// A REFUSED read drops the event silently. The caller was not entitled to the
// row; announcing "something you may not see changed" would leak the
// existence the gate withheld.
//
// ===========================================================================
// THE OWNERSHIP CHECK, AND WHY IT IS HERE RATHER THAN IN THE ENGINE
// ===========================================================================
// `v1:planner:plan` declares no row-authz tier today (memql#4366 records why
// the declaration did not ship with the subscription seam: the engine's own
// internal reads run with no AccessContext and an owned tier would refuse
// them). So `planById` answers for ANY plan id, including someone else's.
//
// Nexus refuses to draw a goal whose `requestedBy` is not the caller's own
// user id, and says so on the page. That is a CLIENT-SIDE filter and it is
// labelled as one wherever it appears -- it closes the deep-link hole this
// surface would otherwise open, it does not narrow the read underneath, and
// it is not a substitute for the declaration. The residual is recorded in
// docs/public/operate/auth/per-row-authz-audit.md, which is where the long
// tail is tracked, rather than in a comment nobody greps for.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { getRowByConceptAndId, type Event, type Row } from "@znasllc-io/memql-sdk-core/client";

import { bumpActivity } from "../../cluster/activity";
import { useCluster } from "../../cluster/ClusterProvider";
import { useMyAccess } from "../../cluster/useMyAccess";
import { eventRowId } from "../../concepts/liveBand";
import {
  AGENT_CONCEPT_ID,
  ARTIFACT_CONCEPT_ID,
  BUNDLE_CONCEPT_ID,
  CONSTRUCT_CONCEPT_ID,
  DEPENDENCY_EDGE_CONCEPT_ID,
  PLAN_CONCEPT_ID,
  TASK_CONCEPT_ID,
} from "../concepts";
import { EMPTY_WORLD, type GoalWorld } from "../scene/world";
import {
  EMPTY_FEED,
  belongsToPlan,
  dropRow,
  foldRow,
  setPlanner,
  worldFromFeed,
  type FeedSlot,
  type FeedState,
} from "./merge";

// The concept behind each slot, in one table. Both the seeding and the
// following read it, so "which concept is a task" is stated once.
const CONCEPT_FOR_SLOT: Record<FeedSlot, string> = {
  plan: PLAN_CONCEPT_ID,
  task: TASK_CONCEPT_ID,
  agent: AGENT_CONCEPT_ID,
  bundle: BUNDLE_CONCEPT_ID,
  construct: CONSTRUCT_CONCEPT_ID,
  edge: DEPENDENCY_EDGE_CONCEPT_ID,
  artifact: ARTIFACT_CONCEPT_ID,
};

const SLOTS: readonly FeedSlot[] = [
  "plan",
  "task",
  "agent",
  "bundle",
  "construct",
  "edge",
  "artifact",
];

export interface GoalWorldState {
  world: GoalWorld;
  loading: boolean;
  error: string;
  // Non-empty when the goal exists but is not the caller's. Distinct from
  // `missing` (no such goal) and from `error` (the read failed): a person who
  // followed someone else's link needs to be told which of the three
  // happened, and only one of them is worth retrying.
  refused: string;
  missing: boolean;
  // Non-empty when the CDC subscriptions could not be opened. Kept apart from
  // `error` for the reason useConceptRows states: a successful seed must not
  // erase a "this map is not live" notice, or the scene looks current moments
  // after going deaf.
  liveDegraded: string;
  reload: () => void;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// flatten turns the NESTED shape a row read returns (payload under `payload`)
// into the flat shape the rest of this feature reads, which is also the shape
// a full-payload CDC envelope already has. Without it every concept field on
// a re-read row is undefined and the map draws blanks -- a failure that looks
// like a rendering bug rather than a shape mismatch, which is exactly the
// trap useConceptRows documents at the same seam.
function flatten(row: Row): Row {
  const nested = row["payload"];
  const fields =
    nested && typeof nested === "object" && !Array.isArray(nested)
      ? (nested as Record<string, unknown>)
      : {};
  return { ...row, ...fields };
}

export function useGoalWorld(planId: string): GoalWorldState {
  const { query, subscriptions } = useCluster();
  const { access } = useMyAccess();
  const [feed, setFeed] = useState<FeedState>(EMPTY_FEED);
  // Starts true for the same reason useArtifacts' does: a read is effectively
  // in flight from mount, so "loading" is the honest initial state rather
  // than "confirmed empty", which is what a false would claim before anything
  // had been asked.
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [refused, setRefused] = useState("");
  const [missing, setMissing] = useState(false);
  const [liveDegraded, setLiveDegraded] = useState("");
  const [epoch, setEpoch] = useState(0);

  const myUserId = access?.userId ?? "";

  // The bundle id at EVENT time, not at subscribe time. A construct and a
  // dependency edge point at their bundle rather than at the plan, so
  // belongsToPlan cannot decide them until the bundle is known -- and the
  // bundle usually arrives after the subscription is already open. A ref
  // because the handler wants the latest committed value, not the one
  // captured when it subscribed (same reasoning as useConceptRows' paged-id
  // set).
  const bundleIdRef = useRef("");
  bundleIdRef.current = typeof feed.bundle?.["id"] === "string" ? (feed.bundle["id"] as string) : "";

  // ---- seed -------------------------------------------------------------
  useEffect(() => {
    if (planId === "") {
      setFeed(EMPTY_FEED);
      setLoading(false);
      setError("");
      setRefused("");
      setMissing(false);
      return;
    }
    if (query === null) {
      // Not connected yet -- NOT a definitive "no such goal". Leave `loading`
      // alone rather than asserting an answer before a read was attempted.
      return;
    }
    // The ownership check needs the caller's identity, and MyAccess resolves
    // on its own round trip. Seeding before it lands would either draw a goal
    // this hook has not yet decided the caller may see, or refuse one it may.
    // Waiting is the only honest option, and it is a few milliseconds.
    if (myUserId === "") return;

    let live = true;
    setLoading(true);
    setError("");
    setRefused("");
    setMissing(false);

    void (async () => {
      try {
        const planResult = await query.planById({ planId });
        if (!live) return;
        const plan = planResult.rows()[0] ?? null;
        if (plan === null) {
          setFeed(EMPTY_FEED);
          setMissing(true);
          setLoading(false);
          return;
        }
        const requestedBy = typeof plan["requestedBy"] === "string" ? plan["requestedBy"] : "";
        if (requestedBy !== myUserId) {
          setFeed(EMPTY_FEED);
          setRefused(
            "This goal belongs to someone else. Nexus draws your own goals; " +
              "the link names one that is not yours.",
          );
          setLoading(false);
          return;
        }

        // Published as soon as it is read, before the rest of the seed. The
        // goal's title, status and beacon are what the page is ABOUT, and
        // holding them behind six more reads means a skeleton where a header
        // could already be. It also puts the map in the state the feature is
        // named for: a goal on its own, with its world materializing into it.
        setFeed((current) => foldRow(current, "plan", plan));

        const ownerAgentId =
          typeof plan["ownerAgentId"] === "string" ? (plan["ownerAgentId"] as string) : "";

        const [tasks, agents, artifacts, bundleResult, planner] = await Promise.all([
          query.tasksForPlan({ planId }),
          query.agentsForPlan({ planId }),
          query.artifactsForPlan({ planId }),
          query.authoringBundleForPlan({ sourcePlanId: planId }),
          ownerAgentId === "" ? null : query.agentById({ agentId: ownerAgentId }),
        ]);
        if (!live) return;

        const bundle = bundleResult.rows()[0] ?? null;
        const bundleId = bundle === null ? "" : String(bundle["id"] ?? "");

        // The bundle's contents need the bundle's id, so they are a second
        // round trip rather than part of the fan-out above. A goal with no
        // bundle -- the common case, since authoring capture is off by
        // default -- skips it entirely.
        const [constructs, edges] =
          bundleId === ""
            ? [null, null]
            : await Promise.all([
                query.authoringConstructsForBundle({ bundleId }),
                query.dependencyEdgesForBundle({ bundleId }),
              ]);
        if (!live) return;

        // Built as ONE state from the seed rather than folded row by row: the
        // seed is a snapshot, and folding it would run the watermark rule
        // against an empty map seven hundred times to reach the same answer.
        // Follow events fold; the seed replaces.
        const seeded: FeedState = {
          plan,
          planner: planner === null ? null : (planner.rows()[0] ?? null),
          tasks: byId(tasks.rows()),
          agents: byId(agents.rows()),
          bundle,
          constructs: constructs === null ? new Map() : byId(constructs.rows()),
          edges: edges === null ? new Map() : byId(edges.rows()),
          artifacts: byId(artifacts.rows()),
        };
        // A seed that lands AFTER follow events have already arrived must not
        // roll those rows backwards. Re-folding the freshly-seeded rows over
        // whatever the follow put there is what applies the watermark rule to
        // exactly that race.
        //
        // `seeded` is a const, and that is load-bearing rather than style: a
        // state updater runs when React commits, not when setState is called,
        // so a variable reassigned on the next line would be read AFTER the
        // reassignment and the whole seed would land as an empty world. The
        // feed test that emits an event before the seed settles is what
        // catches it.
        setFeed((current) => (current === EMPTY_FEED ? seeded : reconcile(current, seeded)));
        setLoading(false);
      } catch (err: unknown) {
        if (!live) return;
        setError(describe(err));
        setLoading(false);
      }
    })();

    return () => {
      live = false;
    };
  }, [query, planId, myUserId, epoch]);

  // ---- follow -----------------------------------------------------------
  useEffect(() => {
    if (subscriptions === null || query === null || planId === "" || refused !== "") {
      setLiveDegraded("");
      return;
    }
    let live = true;
    const unsubscribes: Array<() => void> = [];

    for (const slot of SLOTS) {
      const conceptId = CONCEPT_FOR_SLOT[slot];
      try {
        unsubscribes.push(
          subscriptions.subscribeGraph(
            (event: Event) => {
              bumpActivity();
              if (!live) return;
              const rowId = eventRowId(event);
              if (rowId === "") return;
              // EVERY event, payload or not (design D6). The branch that
              // would have trusted the payload is deliberately absent.
              void getRowByConceptAndId(query, conceptId, rowId)
                .then((row) => {
                  if (!live) return;
                  if (row === null) {
                    // The read succeeded and found nothing: the row is gone.
                    setFeed((current) => dropRow(current, slot, rowId));
                    return;
                  }
                  const flat = flatten(row);
                  if (!belongsToPlan(slot, flat, planId, bundleIdRef.current)) return;
                  setFeed((current) => foldRow(current, slot, flat));
                })
                .catch(() => {
                  // Refused, or the connection dropped. Nothing honest to
                  // put on the map either way -- see this file's header.
                });
            },
            { concept: conceptId, actions: ["created", "updated", "deleted"] },
          ),
        );
      } catch (err) {
        // A failed subscribe does not break the map -- the seed still drew
        // it. It breaks the PROMISE that the map is live, which has to be
        // said out loud rather than inferred from a scene that never moves.
        setLiveDegraded(describe(err));
      }
    }

    return () => {
      live = false;
      for (const stop of unsubscribes) stop();
    };
  }, [subscriptions, query, planId, refused, epoch]);

  // ---- the planner, once the plan names one -----------------------------
  // Separate from the seed because `ownerAgentId` can be stamped on the plan
  // AFTER it is created (the planner is assigned when routing starts), and
  // that arrives as a plan update rather than as an agent event.
  const ownerAgentId =
    typeof feed.plan?.["ownerAgentId"] === "string" ? (feed.plan["ownerAgentId"] as string) : "";
  const plannerId = typeof feed.planner?.["id"] === "string" ? (feed.planner["id"] as string) : "";
  useEffect(() => {
    if (query === null || ownerAgentId === "" || ownerAgentId === plannerId) return;
    let live = true;
    void query
      .agentById({ agentId: ownerAgentId })
      .then((result) => {
        if (!live) return;
        setFeed((current) => setPlanner(current, result.rows()[0] ?? null));
      })
      .catch(() => {
        // The planner is one node of many. A refused or failed read leaves
        // the map without it rather than without the map.
      });
    return () => {
      live = false;
    };
  }, [query, ownerAgentId, plannerId]);

  const world = useMemo(
    () => (refused === "" && !missing ? worldFromFeed(feed) : EMPTY_WORLD),
    [feed, refused, missing],
  );

  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  return { world, loading, error, refused, missing, liveDegraded, reload };
}

function byId(rows: readonly Row[]): Map<string, Row> {
  const out = new Map<string, Row>();
  for (const row of rows) {
    const id = typeof row["id"] === "string" ? row["id"] : "";
    if (id !== "") out.set(id, row);
  }
  return out;
}

// reconcile folds a freshly-seeded snapshot over whatever the follow already
// put in place, so the seed-then-follow race resolves by watermark rather
// than by which promise settled last. Exported shape is deliberately not part
// of merge.ts: it is about the SEED, and merge.ts is about a single arrival.
function reconcile(current: FeedState, seeded: FeedState): FeedState {
  let next = current;
  if (seeded.plan !== null) next = foldRow(next, "plan", seeded.plan);
  if (seeded.bundle !== null) next = foldRow(next, "bundle", seeded.bundle);
  next = setPlanner(next, seeded.planner ?? next.planner);
  for (const row of seeded.tasks.values()) next = foldRow(next, "task", row);
  for (const row of seeded.agents.values()) next = foldRow(next, "agent", row);
  for (const row of seeded.constructs.values()) next = foldRow(next, "construct", row);
  for (const row of seeded.edges.values()) next = foldRow(next, "edge", row);
  for (const row of seeded.artifacts.values()) next = foldRow(next, "artifact", row);
  return next;
}
