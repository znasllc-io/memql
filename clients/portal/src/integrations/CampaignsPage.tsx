import { useState, type FormEvent, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { STAT_TILE_ELEMENT, TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";

import { Empty } from "../components/StatusMessage";
import {
  Band,
  Button,
  Container,
  ErrorNotice,
  Field,
  FormActions,
  FormRow,
  PageHeader,
  Skeleton,
  Textarea,
  TextInput,
} from "../ui";
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
// SENDING LANDED IN memql#3348 and lives one level down, on the campaign
// editor -- deliberately, rather than as a per-row action on these tables.
// Starting a send is the one irreversible thing an operator can do here, and
// an irreversible action reached by a button in a list row is reached by
// accident. The editor is where the audience size, the template and the
// suppression count are on screen at the same time.

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
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          eyebrow={CAMPAIGN_CONCEPT_ID}
          title="Email campaigns"
          blurb="Audiences, templates and the campaigns that pair them. Open a campaign to send it — scheduling records an intended time and does not start a send on its own."
          actions={
            <>
              <Button size="xs" onClick={refresh}>
                Refresh
              </Button>
              <Button size="xs" onClick={() => navigate(newCampaignPath())}>
                New campaign
              </Button>
            </>
          }
        />

        {actionError ? <ErrorNotice sentence="That action did not run." next="Nothing changed; try it again." detail={actionError} /> : null}
        {actionMessage ? (
          <p role="status" className="rounded border border-line bg-raised px-3 py-2 text-sm text-fg">
            {actionMessage}
          </p>
        ) : null}

        {error ? (
          <ErrorNotice sentence="Could not read your campaigns." next="Reload the page to read them again." detail={error} />
        ) : loading && campaigns.length === 0 && audiences.length === 0 ? (
          <Skeleton variant="rows" rows={6} />
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
    </Container>
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
    <FormRow onSubmit={submit}>
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
      <FormActions>
        <Button type="submit" busy={busy} busyLabel="Working…" disabled={name.trim() === ""}>
          Add audience
        </Button>
      </FormActions>
    </FormRow>
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
      <FormRow>
        <Field label="Template name">
          <TextInput value={name} onChange={setName} placeholder="August product update" />
        </Field>
        <Field label="Subject line" grow>
          <TextInput value={subject} onChange={setSubject} placeholder="What lands in the inbox" />
        </Field>
      </FormRow>
      <Field label="Plain-text body" grow>
        <Textarea
          value={textBody}
          onChange={setTextBody}
          rows={4}
          placeholder="The message. Plain text is required; the HTML alternative is added when editing."
        />
      </Field>
      <div>
        <Button type="submit" busy={busy} busyLabel="Working…" disabled={incomplete}>
          Add template
        </Button>
      </div>
    </form>
  );
}
