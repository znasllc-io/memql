import { Caption, Field, Input, Notice } from "../../../../../kit";
import { CredentialField } from "../../../sources/CredentialField";
import { SOURCE_HOST, probeNote, probeParks, probeWantsCredential } from "../../../sources/probe";
import type { SourceProbeHandle } from "../../../sources/useProbes";
import type { CredentialRow } from "../../../sources/rows";
import { suggestName, type ComposeDraft } from "../../compose";
import { NameField } from "./fields";

// A repository named by URL, with a personal token when it is private
// (epic memql#4885, design section C).
//
// ===========================================================================
// A COMPONENT OF ITS OWN, AND WHAT THAT IS FOR
// ===========================================================================
// This is the whole of the repository answer on the Source stop today: the
// URL, the ref, the probe's verdict, the credential when GitHub's answer is
// one a credential could change, and the name. It is separable rather than
// inlined so that GitHub Connect (memql#4912) can mount it in one line under
// the picker it adds -- that epic reshapes what sits ABOVE this form, not
// this form.
//
// Nothing here anticipates that epic. On this branch there is no grant and no
// GitHub App, the pasted URL and a personal token ARE the answer, and every
// sentence below is written in the present tense about what exists.
//
// ===========================================================================
// THE PROBE ANSWERS, IT DOES NOT DECIDE
// ===========================================================================
// What a probe reason is WORTH belongs to `sources/probe.ts` and not to this
// file: a definite answer about the repository parks the flow, and an answer
// about the probe itself says so and leaves Analyze reachable. Design H: the
// fetch is the authority and the probe is a courtesy.

export function TokenSourceForm({
  draft,
  onDraft,
  credentials,
  probe,
}: {
  draft: ComposeDraft;
  onDraft: (patch: Partial<ComposeDraft>) => void;
  credentials: readonly CredentialRow[];
  probe: SourceProbeHandle;
}) {
  const reply = probe.reply;
  const reason = reply?.reason ?? "";
  const note = reply === null ? "" : probeNote(reply);
  // The credential field appears when GitHub's answer is one a credential
  // could change, and stays once one is chosen: switching between two of
  // your own is exactly what somebody does when the first cannot see it.
  const wantsCredential = probeWantsCredential(reason) || draft.credentialId !== "";

  return (
    <>
      {/* THE PROBE FIRES ON BLUR, and the handler sits on a WRAPPER rather
          than on the input: React's onBlur is `focusout`, which bubbles, and
          the kit's Input carries a visually-hidden label plus no blur prop of
          its own. Adding one to the kit for a single caller would be
          promoting a control on its FIRST use, which the kit's own header
          asks surfaces not to do. */}
      <div onBlur={() => void probe.probe(draft.repoUrl, draft.credentialId)}>
        <Field label="Repository URL">
          <Input
            id="os-compose-repo-url"
            label="The repository this deployable is built from"
            value={draft.repoUrl}
            onChange={(repoUrl) => {
              // A NEW URL MAKES THE OLD ANSWER WRONG, not stale: "private, or
              // not there" beside a URL it was never about is worse than no
              // answer at all.
              probe.clear();
              onDraft({ repoUrl });
            }}
            placeholder={`https://${SOURCE_HOST}/acme/storefront`}
            onEnter={() => void probe.probe(draft.repoUrl, draft.credentialId)}
          />
        </Field>
      </div>

      {probe.busy ? <Caption>Asking whether this cluster can read it...</Caption> : null}
      {/* SAID ONCE (DESIGN.md rule 7). A reason that PARKS the flow is the
          rail's note -- a stopped stop names its reason there -- so the body
          would be repeating the sentence directly beneath it. What the body
          adds is the answer the rail has no room for: the branch a public
          repository will follow, or that a token is working. */}
      {note === "" || probeParks(reason) ? null : (
        <p className="os-stop-verdict" data-tone="ok" role="status">
          {note}
        </p>
      )}
      {/* A PROBE THAT COULD NOT RUN IS NOT AN ANSWER ABOUT THE REPOSITORY.
          The server's sentence renders here, the field stays editable, and
          Analyze stays reachable -- the fetch is the authority. */}
      {probe.error === "" ? null : (
        <Notice
          tone="warn"
          sentence="This cluster could not check the repository just now."
          next="Nothing is wrong with what you typed. Deploying still works: the fetch asks again, and it is the one that decides."
          detail={probe.error}
        />
      )}

      <Field label="Branch or tag">
        <Input
          id="os-compose-repo-ref"
          label="Which branch or tag to deploy"
          value={draft.repoRef}
          onChange={(repoRef) => onDraft({ repoRef })}
          placeholder="the default branch"
        />
      </Field>

      {wantsCredential ? (
        <>
          <CredentialField
            id="os-compose-credential"
            credentials={credentials}
            value={draft.credentialId}
            onChange={(credentialId) => {
              onDraft({ credentialId });
              // THE POINT OF CHOOSING ONE IS TO ASK AGAIN UNDER IT.
              void probe.probe(draft.repoUrl, credentialId);
            }}
          />
          <Caption>
            A private repository is fetched under one of your own credentials. It is read at fetch time, on this
            cluster, and never leaves it.
          </Caption>
        </>
      ) : null}

      <NameField draft={draft} onDraft={onDraft} label="Call it" placeholderFrom={suggestName(draft, "")} />
    </>
  );
}
