import { JoiningPolicyFields, useJoiningPolicy } from "../../auth/JoiningPolicy";
import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { Button, Caption, Field, Input, Notice, Panel, Select, Subhead } from "../../kit";
import { draftProblem, editFromDraft, useClusterPolicy } from "./clusterPolicy";
import { useSettingsWrites } from "./settingsWrites";

// The editable half of Settings -> Cluster (epic memql#4984): registration
// policy, how long a session lasts, and what the sign-in pages call this
// cluster.
//
// IT SITS BELOW THE FACTS RATHER THAN AMONG THEM. The panels above it answer
// "what is this cluster"; this one answers "what may it do". Interleaving an
// editable field into a `dl` of read-only facts is how somebody comes to
// believe a fact is a control -- DESIGN.md rule 6, actions are verbs and nouns
// are nouns -- so the form is its own Panel with its own Subhead and one Save.
//
// SAVE IS THE PANEL'S ONE PRIMARY, and Revert stands beside it only while
// there is something to revert. A disabled Revert on an untouched form is
// furniture; an absent one says the same thing more quietly.
//
// EVERY TTL IS BLANK-MEANS-DEFAULT. The row stores 0 for "fall back to the
// environment", and rendering that as a literal `0` in a field labelled
// "Session length" tells a person their sessions expire immediately. Blank
// with a `Cluster default` placeholder is the same value said truthfully; the
// conversion between the unit on screen and the seconds on the wire happens at
// exactly one seam, in clusterPolicy.ts.

export function PolicyPanel({ enabled }: { enabled: boolean }) {
  const policy = useClusterPolicy(enabled);
  const writes = useSettingsWrites();
  const joining = useJoiningPolicy(policy.draft, policy.set);
  const problem = joining.problem || draftProblem({ ...policy.draft, ...joining.effective });
  const clean = policy.clean && !joining.pending;
  const busy = writes.busyKey === "cluster-settings";

  if (!enabled) return null;

  return (
    <Panel label="Policy">
      <Subhead>Policy</Subhead>
      <p className="os-caption">
        Who may sign up, how long a session lasts, and what the sign-in pages
        call this cluster. A change applies to the next token minted or link
        issued -- it does not shorten a session somebody already has.
      </p>

      {policy.error ? (
        <Notice
          tone="warn"
          sentence="The cluster settings could not be read, so this form is showing defaults rather than what is stored."
          next="Saving now would write those defaults over whatever is there. Read again first."
          detail={policy.error}
        />
      ) : null}

      {writes.refusal ? (
        <Notice
          tone="error"
          sentence={
            writes.refusal.denied
              ? "The cluster refused that, and nothing was saved."
              : "That did not go through, and nothing was saved."
          }
          next={
            writes.refusal.auditEventId
              ? `Recorded as audit event ${writes.refusal.auditEventId}.`
              : undefined
          }
          detail={writes.refusal.detail}
        />
      ) : null}
      {writes.done ? <Notice tone="info" sentence={writes.done} /> : null}

      <JoiningPolicyFields policy={joining} />

      <Field label="Session length (minutes)">
        <Input
          id="policy-access-ttl"
          label="Session length in minutes"
          value={policy.draft.accessTokenMinutes}
          onChange={(next) => policy.set({ accessTokenMinutes: next })}
          placeholder="Cluster default"
        />
      </Field>
      <Field label="Stay signed in for (days)">
        <Input
          id="policy-refresh-ttl"
          label="Stay signed in for, in days"
          value={policy.draft.refreshTokenDays}
          onChange={(next) => policy.set({ refreshTokenDays: next })}
          placeholder="Cluster default"
        />
      </Field>
      <Field label="Sign-in link expires after (minutes)">
        <Input
          id="policy-magic-ttl"
          label="Sign-in link expires after, in minutes"
          value={policy.draft.magicLinkMinutes}
          onChange={(next) => policy.set({ magicLinkMinutes: next })}
          placeholder="Cluster default"
        />
      </Field>
      <Field label="Invitation expires after (days)">
        <Input
          id="policy-invite-ttl"
          label="Invitation expires after, in days"
          value={policy.draft.invitationDays}
          onChange={(next) => policy.set({ invitationDays: next })}
          placeholder="Cluster default"
        />
      </Field>
      <Caption>
        Blank means this cluster&apos;s own default, which is whatever its
        environment sets. Shortening how long people stay signed in does not
        end a session that is already running -- it refuses the next renewal.
      </Caption>

      <Field label="Sign-in cookie policy">
        <Select
          id="policy-samesite"
          label="Sign-in cookie policy"
          value={policy.draft.refreshCookieSameSite}
          onChange={(next) => policy.set({ refreshCookieSameSite: next })}
        >
          <option value="">Cluster default</option>
          <option value="lax">Same site (the usual setup)</option>
          <option value="none">Across sites (only when sign-in is on another domain)</option>
        </Select>
      </Field>

      <Field label="What the sign-in pages call this cluster">
        <Input
          id="policy-brand-name"
          label="What the sign-in pages call this cluster"
          value={policy.draft.brandName}
          onChange={(next) => policy.set({ brandName: next })}
          placeholder="MemQL"
        />
      </Field>
      <Field label="Accent colour">
        <Input
          id="policy-brand-colour"
          label="Accent colour"
          value={policy.draft.brandPrimaryColor}
          onChange={(next) => policy.set({ brandPrimaryColor: next })}
          placeholder="#2f6df6"
        />
      </Field>
      <Caption>
        These dress the identity service&apos;s own pages -- sign-in, enrolment
        and the mail it sends. They do not theme this desktop; Appearance does
        that, and a person&apos;s choice there is theirs rather than the
        cluster&apos;s.
      </Caption>

      {problem ? <Notice tone="warn" sentence={problem} /> : null}

      <div className="os-refresh-row">
        <Button
          tone="primary"
          busy={busy}
          busyLabel="Saving"
          disabled={clean || policy.loading || problem !== "" || policy.error !== ""}
          onClick={() => {
            void writes.saveClusterSettings(editFromDraft({ ...policy.draft, ...joining.effective })).then((ok) => {
              if (ok) { joining.reset(); policy.reload(); }
            });
          }}
        >
          Save
        </Button>
        {/* ABSENT while there is nothing to revert, rather than disabled. */}
        {clean ? null : (
          <Button
            onClick={() => {
              writes.clear();
              policy.revert();
              joining.reset();
            }}
          >
            Discard changes
          </Button>
        )}
        {policy.loading ? <RecordListSkeleton label="Reading what is stored" /> : <Caption>{clean
              ? "Nothing to save."
              : "Not saved yet."}</Caption>}
      </div>
    </Panel>
  );
}
