import { useState } from "react";

import {
  Button,
  Caption,
  ChoiceStack,
  Field,
  FormRow,
  Input,
  Notice,
  Panel,
  Subhead,
} from "../../../kit";
import { DEPLOYABLE_KINDS, STOREFRONT_KIND } from "../concepts";
import { hostnameFor, validateSlug } from "../hostname";
import { useCreateSite } from "../actions";

// Create a deployable.
//
// The form takes a NAME and a KIND. It does not take a hostname: the domain is
// the one this cluster serves, composed here and previewed so nobody has to
// guess what they are claiming, and every other hostname shape -- a custom
// apex, a second domain -- is cluster-owner-only and hand-certified
// (memql#4224), so offering the field would offer a claim this window cannot
// complete.
//
// It does not take an OWNER either: `createSite` stamps `ownerUserId` from the
// verified actor, so the form has nothing to say about it and sending a field
// would only be a way to be wrong.

export function CreateSite({ domain }: { domain: string }) {
  const [slug, setSlug] = useState("");
  const [kind, setKind] = useState("spa");
  const [title, setTitle] = useState("");
  const [storeDomain, setStoreDomain] = useState("");
  const [tokenRef, setTokenRef] = useState("");
  const { busy, error, createdId, create, reset } = useCreateSite();

  const hostname = hostnameFor(slug, domain);
  // The BROWSER's half of the policy, at keystroke rate. The server decides;
  // this only means somebody learns that "api" is reserved before they submit
  // rather than after.
  const slugProblem = validateSlug(slug, domain);
  const storefront = kind === STOREFRONT_KIND;
  const storefrontIncomplete =
    storefront && (storeDomain.trim() === "" || tokenRef.trim() === "");

  async function submit() {
    const id = await create({ slug, kind, title, storeDomain, storefrontTokenRef: tokenRef }, domain);
    if (id === "") return;
    setSlug("");
    setTitle("");
    setStoreDomain("");
    setTokenRef("");
  }

  return (
    <Panel label="Create a deployable">
      <Subhead>New deployable</Subhead>

      <Field label="Name">
        <Input
          id="os-deploy-slug"
          label="Name"
          value={slug}
          placeholder="shop"
          disabled={busy}
          onChange={(next) => {
            setSlug(next);
            reset();
          }}
          onEnter={() => void submit()}
        />
        <span className="os-caption os-mono">
          {hostname === "" ? `<name>.${domain || "<domain unknown>"}` : hostname}
        </span>
      </Field>

      {slugProblem === "" ? null : <Caption>{slugProblem}</Caption>}

      <ChoiceStack
        name="os-deploy-kind"
        label="Kind"
        value={kind}
        onChange={setKind}
        options={DEPLOYABLE_KINDS.map((k) => ({
          value: k.value,
          label: k.label,
          description: k.blurb,
        }))}
      />
      <Caption>
        Android, iOS and macOS builds are not deployables. They are distributed through stores and
        signed downloads rather than answered at a hostname, so this cluster's edge has nothing to
        resolve for them.
      </Caption>

      {storefront ? (
        <>
          <Field label="Shopify store domain">
            <Input
              id="os-deploy-store"
              label="Shopify store domain"
              value={storeDomain}
              placeholder="example.myshopify.com"
              disabled={busy}
              onChange={setStoreDomain}
            />
          </Field>
          <Field label="Storefront token secret name">
            <Input
              id="os-deploy-token-ref"
              label="Storefront token secret name"
              value={tokenRef}
              placeholder="shopify-storefront-token"
              disabled={busy}
              onChange={setTokenRef}
            />
          </Field>
          {/* THE NAME OF A SECRET, NOT A SECRET. The row stores a reference to
              a v1:platform:globalSecret; the token itself never reaches this
              form, this row, or this browser. The edge resolves it at serve
              time, which is the only place it is dereferenced. */}
          <Caption>
            The token itself is never typed here. Name the globalSecret that holds it -- the edge
            resolves it at serve time into the storefront's runtime config.
          </Caption>
        </>
      ) : null}

      <Field label="Label (optional)">
        <Input
          id="os-deploy-title"
          label="Label (optional)"
          value={title}
          placeholder="Storefront"
          disabled={busy}
          onChange={setTitle}
        />
      </Field>

      {createdId === "" ? null : (
        <Notice
          tone="info"
          sentence="Created, as a draft."
          next="It is on the list and the map already -- the cluster broadcast it. A draft answers 404 until you publish a bundle to it and set it live."
          detail={createdId}
        />
      )}

      {error === "" ? null : (
        <Notice
          tone="error"
          sentence="That deployable was not created."
          next="The cluster decides which hostnames may be claimed -- including whether one is already taken, which this window cannot know."
          detail={error}
        />
      )}

      <FormRow>
        <Button
          tone="primary"
          busy={busy}
          busyLabel="Creating"
          disabled={slug.trim() === "" || slugProblem !== "" || storefrontIncomplete}
          onClick={() => void submit()}
        >
          Create
        </Button>
        {storefrontIncomplete ? (
          <Caption>A storefront needs both its store domain and the name of its token secret.</Caption>
        ) : null}
      </FormRow>
    </Panel>
  );
}
