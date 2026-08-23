import { useState, type FormEvent, type ReactNode } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";

import { useConcepts } from "../cluster/useConcepts";
import { RowList } from "../components/RowList";
import { ErrorMessage } from "../components/StatusMessage";
import { Band, Button, Container, EmptyState, Field, PageHeader, Skeleton, Textarea, TextInput } from "../ui";
import { ARTIFACT_CONCEPT_ID } from "./concepts";
import { LABEL_PARAM, artifactPath } from "./urls";
import { useArtifacts, type CreateArtifactInput } from "./useArtifacts";

// The Library, browsed (task 4 of the artifacts-labels epic): every artifact
// the caller owns, live-narrowed to one label at a time, plus a form that
// records a new one.
//
// DO NOT HAND-ROLL THE LIST. RowList (components/RowList.tsx) is the ONLY
// renderer used here, driven entirely by v1:library:artifact's declared
// @displayCard(primary="title", secondary="summary", tertiary="updatedAt",
// status="lens") -- enforced for the concept-agnostic browse path by
// portal_render_path_test.go; this feature module is a designed surface
// about one population, the same standing sites/SitesPage.tsx documents for
// itself. This file feeds RowList rows; it does not decide how a row looks.
//
// THE LABEL FILTER LIVES IN THE URL (?label=), not in a useState -- the same
// rule ConceptsPage's domain chips follow, and for the same reason: a
// filtered Library is itself a link someone can paste ("here's what's
// labelled onboarding"), it survives a refresh, and the back button undoes a
// click the way an operator expects. The chip that toggles it is a Button
// (aria-pressed) whose onClick calls setSearchParams -- a real navigation,
// not a re-render -- exactly the shape ConceptsPage's DomainChip already
// uses for its own facet.
export function ArtifactsPage(): ReactNode {
  const navigate = useNavigate();
  const { concepts, loading: conceptsLoading, error: conceptsError } = useConcepts();
  const [params, setParams] = useSearchParams();
  const label = params.get(LABEL_PARAM) ?? "";

  const {
    rows,
    labels: availableLabels,
    loading,
    error,
    reload,
    createBusy,
    createError,
    createMessage,
    createArtifact,
  } = useArtifacts(label);

  const concept = concepts.find((c) => c.id === ARTIFACT_CONCEPT_ID);
  const select = (id: string) => navigate(artifactPath(id));

  function setLabel(next: string): void {
    const nextParams = new URLSearchParams(params);
    if (next === "") nextParams.delete(LABEL_PARAM);
    else nextParams.set(LABEL_PARAM, next);
    // replace: clicking a filter chip narrows in place. A push here would
    // make the back button undo one label click at a time instead of
    // leaving the list.
    setParams(nextParams, { replace: true });
  }

  return (
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          eyebrow={ARTIFACT_CONCEPT_ID}
          title="Artifacts"
          blurb="Everything this cluster's Library has indexed for you -- uploaded documents, generated outputs, and the notes, to-dos, calendar events, and memories your agents have created. Put your own labels on one to say what it was for; a label you or an agent added is a filter here too."
          actions={
            <Button size="xs" onClick={reload}>
              Refresh
            </Button>
          }
        />

        <Band title="New artifact" meta="records a generated output; the Library indexes it automatically">
          <NewArtifactForm busy={createBusy} error={createError} message={createMessage} onCreate={createArtifact} />
        </Band>

        <Band
          title="Artifacts"
          meta={label ? `filtered to "${label}"` : undefined}
          panel
        >
          {availableLabels.length === 0 ? null : (
            <div
              role="group"
              aria-label="Filter by label"
              className="mb-3 flex flex-wrap items-center gap-1.5 border-b border-line pb-3"
            >
              {availableLabels.map((one) => (
                <Button
                  key={one}
                  size="xs"
                  tone={label === one ? "primary" : "quiet"}
                  pressed={label === one}
                  onClick={() => setLabel(label === one ? "" : one)}
                >
                  {one}
                </Button>
              ))}
            </div>
          )}

          {conceptsError ? (
            <ErrorMessage>Could not read the concept registry: {conceptsError}</ErrorMessage>
          ) : error ? (
            <ErrorMessage>Could not read artifacts: {error}</ErrorMessage>
          ) : rows.length === 0 ? (
            loading || conceptsLoading ? (
              <Skeleton variant="rows" rows={5} />
            ) : label ? (
              <EmptyState
                statement={`No artifacts labelled "${label}" yet.`}
                action={
                  <Button size="xs" onClick={() => setLabel("")}>
                    Clear the filter
                  </Button>
                }
              />
            ) : (
              <EmptyState statement="No artifacts yet. Uploaded documents, generated outputs, notes, to-dos, calendar events, and memories will appear here as the Library indexes them, or make one with the form above." />
            )
          ) : concept ? (
            <RowList rows={rows} concept={concept} onSelect={select} />
          ) : (
            <Skeleton variant="rows" rows={5} />
          )}
        </Band>
      </section>
    </Container>
  );
}

function NewArtifactForm({
  busy,
  error,
  message,
  onCreate,
}: {
  busy: boolean;
  error: string;
  message: string;
  onCreate: (input: CreateArtifactInput) => void;
}): ReactNode {
  const [title, setTitle] = useState("");
  const [summary, setSummary] = useState("");
  const [body, setBody] = useState("");

  function submit(event: FormEvent): void {
    event.preventDefault();
    if (title.trim() === "") return;
    onCreate({ title: title.trim(), summary: summary.trim(), body: body.trim() });
    setTitle("");
    setSummary("");
    setBody("");
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-2">
      {error ? <ErrorMessage>{error}</ErrorMessage> : null}
      {message ? (
        <p role="status" className="rounded border border-line bg-raised px-3 py-2 text-sm text-fg">
          {message}
        </p>
      ) : null}
      <div className="flex flex-wrap items-end gap-2">
        <Field label="Title" grow>
          <TextInput value={title} onChange={setTitle} placeholder="Ten most beautiful birds" />
        </Field>
        <Field label="Summary" grow hint="Optional. Shown on the list row.">
          <TextInput value={summary} onChange={setSummary} placeholder="A short description" />
        </Field>
      </div>
      <Field label="Body" grow hint="Optional. Markdown, rendered in the artifact's viewer.">
        <Textarea value={body} onChange={setBody} rows={4} placeholder="# Ten most beautiful birds…" />
      </Field>
      <div>
        <Button type="submit" size="xs" busy={busy} busyLabel="Working…" disabled={title.trim() === ""}>
          Create artifact
        </Button>
      </div>
    </form>
  );
}
