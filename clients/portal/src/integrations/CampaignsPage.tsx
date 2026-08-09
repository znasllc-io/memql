import { useState, type FormEvent, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { STAT_TILE_ELEMENT, TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";

import { Empty, ErrorMessage, Loading } from "../components/StatusMessage";
import { Band, MetaButton } from "../views/ViewLayout";
import { ViewElement } from "../views/ViewElement";
import { useCampaigns } from "./useCampaigns";
import {
  AUDIENCE_CONCEPT_ID,
  CAMPAIGN_CONCEPT_ID,
  TEMPLATE_CONCEPT_ID,
  fallbackConcept,
  useCampaignConcepts,
} from "./concepts";
import { campaignPath, newCampaignPath } from "./urls";

// Email marketing campaigns -- the authoring surface (memql#3323).
//
// Everything on this page is ordinary graph state. Campaigns, audiences and
// templates are v1:campaigns:* rows with declared @displayCards and per-row
// authz, which is why the page contains no rendering code for a row: the
// tables are the shared element library reading the cards the DSL declared,
// exactly as the predefined views do.
//
// NOTHING HERE SENDS MAIL, and the page says so rather than leaving an
// operator to discover it. Authoring and sending are separate pieces of work
// and only the first one has landed: a campaign can be written, pointed at an
// audience, and given an intended time, and then it sits there. A half-sender
// -- one that stages mail with no rate limiting, no suppression enforcement
// and no bounce feedback -- would be worse than none, which is why there is
// not one.

export function CampaignsPage(): ReactNode {
  const navigate = useNavigate();
  const {
    campaigns,
    audiences,
    templates,
    loading,
    error,
    actionMessage,
    actionError,
    busy,
    refresh,
    createAudience,
    createTemplate,
  } = useCampaigns();
  const { descriptor } = useCampaignConcepts();

  const campaignConcept =
    descriptor(CAMPAIGN_CONCEPT_ID) ?? fallbackConcept(CAMPAIGN_CONCEPT_ID, "campaign");
  const audienceConcept =
    descriptor(AUDIENCE_CONCEPT_ID) ?? fallbackConcept(AUDIENCE_CONCEPT_ID, "audience");
  const templateConcept =
    descriptor(TEMPLATE_CONCEPT_ID) ?? fallbackConcept(TEMPLATE_CONCEPT_ID, "template");

  return (
    <section className="mx-auto flex min-h-full max-w-5xl flex-col gap-6 pb-8">
      <header className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3 border-b border-line pb-4">
        <div className="min-w-0">
          <p className="font-mono text-xs break-all text-subtle">{CAMPAIGN_CONCEPT_ID}</p>
          <h1 className="mt-1 text-xl font-semibold tracking-tight">Email campaigns</h1>
          <p className="mt-1 max-w-2xl text-sm text-muted">
            Audiences, templates and the campaigns that pair them. Authoring only — scheduling a
            campaign records the intended time and nothing sends.
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <MetaButton onClick={refresh}>Refresh</MetaButton>
          <MetaButton onClick={() => navigate(newCampaignPath())}>New campaign</MetaButton>
        </div>
      </header>

      {actionError ? <ErrorMessage>{actionError}</ErrorMessage> : null}
      {actionMessage ? (
        <p role="status" className="rounded border border-line bg-raised px-3 py-2 text-sm text-fg">
          {actionMessage}
        </p>
      ) : null}

      {error ? (
        <ErrorMessage>Could not read campaigns: {error}</ErrorMessage>
      ) : loading && campaigns.length === 0 && audiences.length === 0 ? (
        <Loading what="your campaigns" />
      ) : (
        <>
          <Band>
            <ViewElement
              element={STAT_TILE_ELEMENT}
              rows={campaigns}
              concept={campaignConcept}
              options={{ bindings: { metric: [] } }}
            />
          </Band>

          <Band title="Campaigns" meta="pick one to edit it" panel>
            {campaigns.length === 0 ? (
              <Empty>
                No campaigns yet. A campaign needs an audience and a template, so start with
                those below.
              </Empty>
            ) : (
              <ViewElement
                element={TABLE_ELEMENT}
                rows={campaigns}
                concept={campaignConcept}
                options={{ sort: { field: "createdAt", direction: "desc" } }}
                onSelect={(rowId) => navigate(campaignPath(rowId))}
              />
            )}
          </Band>

          <Band title="Audiences" meta="who a campaign is addressed to">
            <NewAudienceForm busy={busy} onCreate={createAudience} />
            <div className="mt-3 overflow-x-auto rounded-lg border border-line bg-surface p-1">
              {audiences.length === 0 ? (
                <Empty>No audiences yet.</Empty>
              ) : (
                <ViewElement
                  element={TABLE_ELEMENT}
                  rows={audiences}
                  concept={audienceConcept}
                  options={{ sort: { field: "createdAt", direction: "desc" } }}
                />
              )}
            </div>
          </Band>

          <Band title="Templates" meta="what a campaign says">
            <NewTemplateForm busy={busy} onCreate={createTemplate} />
            <div className="mt-3 overflow-x-auto rounded-lg border border-line bg-surface p-1">
              {templates.length === 0 ? (
                <Empty>No templates yet.</Empty>
              ) : (
                <ViewElement
                  element={TABLE_ELEMENT}
                  rows={templates}
                  concept={templateConcept}
                  options={{ sort: { field: "createdAt", direction: "desc" } }}
                />
              )}
            </div>
          </Band>
        </>
      )}
    </section>
  );
}

function NewAudienceForm({
  busy,
  onCreate,
}: {
  busy: boolean;
  onCreate: (input: { name: string; description: string }) => void;
}): ReactNode {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");

  function submit(event: FormEvent): void {
    event.preventDefault();
    if (name.trim() === "") return;
    onCreate({ name: name.trim(), description: description.trim() });
    setName("");
    setDescription("");
  }

  return (
    <form onSubmit={submit} className="flex flex-wrap items-end gap-2">
      <Field label="Audience name">
        <TextInput value={name} onChange={setName} placeholder="Beta waitlist" />
      </Field>
      <Field label="Description" grow>
        <TextInput
          value={description}
          onChange={setDescription}
          placeholder="How someone lands in this audience"
        />
      </Field>
      <SubmitButton busy={busy} disabled={name.trim() === ""}>
        Add audience
      </SubmitButton>
    </form>
  );
}

function NewTemplateForm({
  busy,
  onCreate,
}: {
  busy: boolean;
  onCreate: (input: {
    name: string;
    subject: string;
    textBody: string;
    htmlBody: string;
  }) => void;
}): ReactNode {
  const [name, setName] = useState("");
  const [subject, setSubject] = useState("");
  const [textBody, setTextBody] = useState("");

  const incomplete = name.trim() === "" || subject.trim() === "" || textBody.trim() === "";

  function submit(event: FormEvent): void {
    event.preventDefault();
    if (incomplete) return;
    // htmlBody is deliberately not offered here. A template lands as a draft
    // with a plain-text body -- the alternative that keeps a message readable
    // and out of the spam bucket -- and the HTML half is an editing step, not
    // a creation one.
    onCreate({
      name: name.trim(),
      subject: subject.trim(),
      textBody: textBody.trim(),
      htmlBody: "",
    });
    setName("");
    setSubject("");
    setTextBody("");
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-2">
      <div className="flex flex-wrap items-end gap-2">
        <Field label="Template name">
          <TextInput value={name} onChange={setName} placeholder="August product update" />
        </Field>
        <Field label="Subject line" grow>
          <TextInput value={subject} onChange={setSubject} placeholder="What lands in the inbox" />
        </Field>
      </div>
      <Field label="Plain-text body" grow>
        <textarea
          value={textBody}
          onChange={(event) => setTextBody(event.target.value)}
          rows={4}
          placeholder="The message. Plain text is required; the HTML alternative is added when editing."
          className="w-full rounded-lg border border-line bg-surface px-3 py-2 text-sm text-fg placeholder:text-subtle"
        />
      </Field>
      <div>
        <SubmitButton busy={busy} disabled={incomplete}>
          Add template
        </SubmitButton>
      </div>
    </form>
  );
}

// ---------------------------------------------------------------------------
// Form primitives, shared with the campaign editor.
// ---------------------------------------------------------------------------

export function Field({
  label,
  hint,
  grow = false,
  children,
}: {
  label: string;
  hint?: string;
  grow?: boolean;
  children: ReactNode;
}): ReactNode {
  return (
    <label className={"flex flex-col gap-1 " + (grow ? "min-w-48 flex-1" : "")}>
      <span className="text-xs font-medium text-muted">{label}</span>
      {children}
      {hint === undefined ? null : <span className="text-xs text-subtle">{hint}</span>}
    </label>
  );
}

export function TextInput({
  value,
  onChange,
  placeholder,
  type = "text",
}: {
  value: string;
  onChange: (next: string) => void;
  placeholder?: string;
  type?: string;
}): ReactNode {
  return (
    <input
      type={type}
      value={value}
      placeholder={placeholder}
      onChange={(event) => onChange(event.target.value)}
      className="w-full rounded-lg border border-line bg-surface px-3 py-2 text-sm text-fg placeholder:text-subtle"
    />
  );
}

export function SubmitButton({
  busy,
  disabled,
  children,
}: {
  busy: boolean;
  disabled: boolean;
  children: ReactNode;
}): ReactNode {
  return (
    <button
      type="submit"
      disabled={busy || disabled}
      className="rounded border border-line bg-surface px-2.5 py-1.5 text-xs font-medium text-fg hover:bg-raised disabled:cursor-not-allowed disabled:opacity-40"
    >
      {busy ? "Working…" : children}
    </button>
  );
}
