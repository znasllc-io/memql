import { useMemo } from "react";

import { useSessionIfPresent } from "../../chrome/access";
import { hasOrganizationDecisions, holds, holdsForOrganization } from "../../system/roles";

// THE PARTS OF DEPLOYABLES (epic memql#5289, task memql#5305; app access
// grants design, section 2's table and section 4 "A missing part").
//
// An app is `read app:<id>`; a PART of one is `execute app:<id>/<part>`, and
// Deployables is the first app to carry the vocabulary. Six parts, each the
// name of a thing a person does here, each grantable to a person or a group
// by name:
//
//   sources   add or edit a source, its credential, its auto-deploy switch
//   deploy    analyze, confirm, retry, cancel a run; publish a Library zip
//   publish   go live, pause, roll back
//   retire    deactivate an app, archive a source, delete a deployable, and
//             their plain inverses
//   domains   bind or remove a client's own domain
//   store     attach or change the Shopify store a storefront fronts
//   preview   publish a candidate version, exercise it against a development
//             store, open and end a preview
//
// SIX OF THEM ARE SEEDED ON OWNER AND DEVELOPER; `store` IS OWNER ALONE
// (memql#5541). It took over from the retired `app:stores` capabilities, and
// those were owner-only because v1:shopify:store is @rowAuthz(clusterOwner):
// a developer holding the part would be drawn the control and then served no
// rows, which is a refusal rendered as an empty panel (memql#5216).
//
// THE OS HIDES; THE ENGINE REFUSES. Every act on this app names the part it
// needs beside itself (`requires` on an ActSpec, on a HeadAction, on the
// controls a stop renders), and a control whose part the effective set does
// not hold is ABSENT -- never disabled, DESIGN.md rule 12. A call that
// reaches the engine anyway is refused with `capability_not_held`, whose copy
// `packages/refusals.ts` carries. The engine's part-to-construct table is
// pinned by TestTheDeployablesPartsAreDeclaredOnTheirConstructs; this file
// only names the parts, and `partResource` is the one spelling of the name.
//
// IT REPLACES `canWrite`, which was rank >= 200 and one answer for every act.
// A person granted `deploy` and not `publish` can push a build and cannot
// take a site live, which one boolean could not say. `preview` draws the same
// line one step earlier (epic memql#5531): a person may prepare a candidate
// version and exercise it end to end against a development store, and still
// not be able to put it in front of a shopper.

export const DEPLOYABLE_PARTS = ["sources", "deploy", "publish", "retire", "domains", "store", "preview"] as const;

export type DeployablePart = (typeof DEPLOYABLE_PARTS)[number];

/** Which parts the effective set holds, one answer per part. */
export type PartsHeld = Readonly<Record<DeployablePart, boolean>>;

/** The resource a part is spelled as in the seeds and on the grants. */
export function partResource(part: DeployablePart): string {
  return `app:deployables/${part}`;
}

/** No part held: what a reader, or a shell before its read landed, resolves to. */
export const NO_PARTS: PartsHeld = Object.freeze({
  sources: false,
  deploy: false,
  publish: false,
  retire: false,
  domains: false,
  store: false,
  preview: false,
});

/** Every part held: what the seeds give an owner. */
export const ALL_PARTS: PartsHeld = Object.freeze({
  sources: true,
  deploy: true,
  publish: true,
  retire: true,
  domains: true,
  store: true,
  preview: true,
});

/** `ALL_PARTS` minus the named ones, for a surface or a test that withholds some. */
export function partsWithout(...missing: DeployablePart[]): PartsHeld {
  const out = { ...ALL_PARTS } as Record<DeployablePart, boolean>;
  for (const part of missing) out[part] = false;
  return out;
}

/**
 * The parts the effective set holds right now.
 *
 * READS MODULE STATE, so a caller that memoises must name `accessEpoch` in
 * its deps; `useDeployableParts` does that for a component.
 */
export function heldParts(): PartsHeld {
  const out = {} as Record<DeployablePart, boolean>;
  for (const part of DEPLOYABLE_PARTS) out[part] = holds("execute", partResource(part));
  return out;
}

/**
 * The parts this session holds, recomputed when the effective set changes.
 *
 * `useSessionIfPresent` rather than `useSession`, so a page rendered in a
 * harness with no session around it still answers -- from the module state
 * the harness installed -- rather than throwing.
 */
export function useDeployableParts(): PartsHeld {
  const epoch = useSessionIfPresent()?.accessEpoch ?? 0;
  // `epoch` is the reactivity signal, not an input: the parts are read out of
  // band and this memo has to recompute when the set lands (memql#4857).
  return useMemo(() => heldParts(), [epoch]);
}

/** Resolve controls for the row or selected organization, never by unioning
 * permissions on unrelated clients. Supplied global parts remain the operator
 * and isolated-component contract when no organization decisions are present. */
export function partsForOrganization(accountId: string, fallback: PartsHeld, dataVerb: "create" | "update" = "update"): PartsHeld {
  if (!hasOrganizationDecisions()) return fallback;
  const allowed = holdsForOrganization(accountId, "read", "data") && holdsForOrganization(accountId, "read", "app:deployables") && holdsForOrganization(accountId, dataVerb, "data");
  return Object.fromEntries(DEPLOYABLE_PARTS.map(part => [part, allowed && holdsForOrganization(accountId, "execute", partResource(part))])) as Record<DeployablePart, boolean>;
}
