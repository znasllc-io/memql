import { useState } from "react";

import { Button, Caption, Field, Input, Subhead } from "../../../kit";
import { toneFor } from "../packages/refusals";
import { ProblemNotice } from "../packages/ReportView";
import type { GithubAppActions } from "./useGithubApp";

// The cluster's GitHub App, on the two surfaces that meet it (engine design
// record 2026-09-20-github-app-setup): the add wizard's repository step and
// Settings > Sources.
//
// ===========================================================================
// ONE QUESTION, ASKED THE SAME WAY IN BOTH PLACES
// ===========================================================================
// Registering the app has exactly one thing to decide here -- whose GitHub
// account it is registered under -- because GitHub asks everything else on its
// own page, where it shows the app it is about to create. So the question is
// one component (`GithubAppOwnerField`) and the two surfaces differ only in
// where the ACT sits: on the wizard's floor, where every forward act of that
// wizard is, and beside the question in Settings, which has no floor.
//
// ===========================================================================
// WHAT IS SAID TO SOMEBODY WHO CANNOT DO IT
// ===========================================================================
// Nothing is offered to them (rule 12: absent, never disabled). They are told
// who can, and what works meanwhile -- which is the other way in that the
// surface they are on already has.

/** Where a cluster owner registers the cluster's GitHub App. */
export interface GithubAppOwner {
  kind: "account" | "organization";
  /** The organization's GitHub login, when that is the kind. */
  organization: string;
}

export const OWN_ACCOUNT: GithubAppOwner = { kind: "account", organization: "" };

/** Whether there is enough to register the app under. An organization is named
 *  by its login, and until one is typed there is nothing to ask GitHub for. */
export function ownerIsNamed(owner: GithubAppOwner): boolean {
  return owner.kind === "account" || owner.organization.trim() !== "";
}

/** The `organization` argument for an owner: "" is the person's own account. */
export function organizationOf(owner: GithubAppOwner): string {
  return owner.kind === "organization" ? owner.organization.trim() : "";
}

/** What an owner is told the trip to GitHub is for: what gets made, the whole
 *  of what it may do, and that it is done once for everybody. */
export const SET_UP_SENTENCE =
  "This cluster is not linked to GitHub yet. Setting it up creates a GitHub App that can only read the repositories people choose. It is done once; everyone here then connects their own account.";

export function GithubAppOwnerField({
  owner,
  onOwner,
  idPrefix,
}: {
  owner: GithubAppOwner;
  onOwner: (owner: GithubAppOwner) => void;
  /** Two surfaces can be open at once, and an id is per document. */
  idPrefix: string;
}) {
  return (
    <>
      <Field label="Register the app under">
        <div className="os-choice-row" role="radiogroup" aria-label="Register the app under">
          <button type="button" role="radio" className="os-choice" aria-checked={owner.kind === "account"}
            onClick={() => onOwner({ kind: "account", organization: "" })}>Your account</button>
          {/* THE LOGIN SURVIVES A CHANGE OF MIND: choosing the account and then
              the organization again finds what was typed. */}
          <button type="button" role="radio" className="os-choice" aria-checked={owner.kind === "organization"}
            onClick={() => onOwner({ kind: "organization", organization: owner.organization })}>An organization</button>
        </div>
      </Field>
      {owner.kind === "organization" ? (
        <Field label="Organization">
          <Input
            id={`${idPrefix}-organization`}
            label="The organization's GitHub login"
            value={owner.organization}
            onChange={(organization) => onOwner({ kind: "organization", organization })}
            placeholder="acme"
          />
        </Field>
      ) : null}
    </>
  );
}

/**
 * Settings > Sources, on a cluster with no GitHub App: in place of Connect,
 * which cannot work.
 */
export function GithubAppMissing({ app, returnPath }: { app: GithubAppActions; returnPath: string }) {
  const [owner, setOwner] = useState<GithubAppOwner>(OWN_ACCOUNT);
  const maySetup = app.status?.canSetup === true;
  return (
    /* A PART OF THE GROUP NAMED `GitHub`, which is what the connected card is:
       the same heading in the same place, for the thing that card will become.
       Rendered loose it ran straight on from the group's own sentence, two
       captions with nothing between them saying they were about different
       things. */
    <section className="os-field-group" aria-label="GitHub">
      <Subhead>GitHub</Subhead>
      {maySetup ? (
        /* THE ADD-CREDENTIAL FORM'S GRID, because this is the same thing in the
           same group: a few fields and their act, spaced as one form. */
        <div className="os-stop-form">
          <Caption>{SET_UP_SENTENCE}</Caption>
          <GithubAppOwnerField owner={owner} onOwner={setOwner} idPrefix="os-sources-github-app" />
          {/* ABSENT until there is somebody to register it under, like the
              wizard's own act -- and BUSY for the whole call, because beginning
              a setup mints a state row and the page is about to navigate away. */}
          {ownerIsNamed(owner) ? (
            <div className="os-form-row">
              <Button busy={app.busy} busyLabel="Opening GitHub" onClick={() => void app.setup(returnPath, organizationOf(owner))}>
                Set up GitHub
              </Button>
            </div>
          ) : null}
          {app.refusal ? <ProblemNotice problem={app.refusal} tone={toneFor(app.refusal.code)} /> : null}
        </div>
      ) : (
        <Caption>
          This cluster is not linked to GitHub yet. Ask a cluster owner to set it up before choosing a repository.
        </Caption>
      )}
    </section>
  );
}

/**
 * The app this cluster has, for a cluster owner: which one, whose it is to
 * change, and -- for one registered from here -- taking it away.
 *
 * AN ARMED TWO-STEP WITH NO TYPED NAME, which is Disconnect's posture and for
 * its reason: what the confirmation contributes is saying what stops working,
 * and the act is undone by setting GitHub up again.
 *
 * WHAT REMOVING DOES NOT DO is said in the armed sentence, because it is the
 * one thing an owner cannot see from here: the app still exists at GitHub, and
 * an owner removing it because its credentials may have leaked has to delete it
 * THERE for that to mean anything.
 */
export function GithubAppBlock({ app }: { app: GithubAppActions }) {
  const [armed, setArmed] = useState(false);
  const status = app.status;
  if (status === null || !status.configured) return null;
  const fromHere = status.source === "cluster";
  return (
    <section className="os-field-group" aria-label="GitHub App">
      <Subhead>GitHub App</Subhead>
      <Caption>
        Everyone on this cluster connects GitHub through{" "}
        {status.slug === "" ? "one app" : (
          <a className="os-link" href={`https://github.com/apps/${encodeURIComponent(status.slug)}`} target="_blank" rel="noreferrer">
            {status.slug}
          </a>
        )}
        {fromHere
          ? ", registered from here."
          : ", set by the deployment's environment. It is changed where the cluster is deployed."}
      </Caption>
      {fromHere ? (
        <section className="os-settings-danger">
          {armed ? (
            <>
              <Caption>
                Every GitHub connection on this cluster stops fetching, and each person reconnects once GitHub is set
                up again. The app itself stays at GitHub until it is deleted there.
              </Caption>
              <div className="os-confirm-row">
                <Button tone="quiet" onClick={() => setArmed(false)}>Cancel</Button>
                <Button tone="danger" busy={app.busy} onClick={() => void app.remove().then((done) => { if (done) setArmed(false); })}>
                  Remove
                </Button>
              </div>
            </>
          ) : (
            <>
              {/* WHAT IT DOES, before it is armed: a bare "Remove" under a
                  heading that says "GitHub App" does not say of what, or what
                  follows. Disconnect's posture, one block up. */}
              <Caption>Unlinks this cluster from GitHub. Connections made through the app stop fetching until it is set up again.</Caption>
              <Button onClick={() => setArmed(true)}>Remove</Button>
            </>
          )}
          {app.refusal ? <ProblemNotice problem={app.refusal} tone={toneFor(app.refusal.code)} /> : null}
        </section>
      ) : null}
    </section>
  );
}
