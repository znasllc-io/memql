import { useState, type DragEvent, type FormEvent, type ReactNode } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { rowString, type Concept, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useConcepts } from "../cluster/useConcepts";
import { RowList } from "../components/RowList";
import {
  Band,
  Button,
  ButtonLink,
  ConfirmDialog,
  Container,
  DataText,
  EmptyState,
  ErrorNotice,
  Field,
  FormActions,
  FormRow,
  PageHeader,
  Skeleton,
  Textarea,
  TextInput,
} from "../ui";
import { Download, Search, Upload } from "../ui/icons";
import { ARTIFACT_CONCEPT_ID } from "./concepts";
import { artifactContentUrl } from "./transport";
import { ARCHIVED_PARAM, ARCHIVED_VALUE, LABEL_PARAM, SEARCH_PARAM, artifactPath } from "./urls";
import {
  useArtifactSearch,
  useArtifacts,
  type ArtifactHit,
  type CreateArtifactInput,
} from "./useArtifacts";

// The Library, browsed and worked: every artifact the caller owns, narrowed
// to one label at a time, searched by meaning, uploaded to, exported from and
// archived (memql#4288 for the first two, memql#4343 for the rest).
//
// DO NOT HAND-ROLL THE ROW. RowList (components/RowList.tsx) renders every
// artifact card here, driven entirely by v1:library:artifact's declared
// @displayCard(primary="title", secondary="summary", tertiary="updatedAt",
// status="lens") -- enforced for the concept-agnostic browse path by
// portal_render_path_test.go; this feature module is a designed surface about
// one population, the same standing sites/SitesPage.tsx documents for itself.
//
// WHAT DID CHANGE IS THE ROW'S FRAME, NOT ITS CONTENT. The export and archive
// verbs sit BESIDE the card, in a column this page owns, with RowList called
// once per row rather than once for the list. That is the only shape that
// gives a per-row action without teaching RowList about this concept: the
// display card resolves identically either way (renderRowList takes a
// declared card verbatim), and the alternative -- an actions slot on RowList
// -- would be a concept-agnostic component growing a hook that only one
// concept's page uses.
//
// THE LABEL FILTER, THE SEARCH AND THE ARCHIVED TOGGLE ALL LIVE IN THE URL,
// not in a useState -- the rule the whole portal is held to (#3316) and the
// one ConceptsPage's domain chips already follow. A filtered, searched
// Library is itself a link someone can paste, it survives a refresh, and the
// back button undoes a click the way an operator expects. The three compose:
// ?q=budget&label=finance is "artifacts about budgets, among the ones
// labelled finance".
export function ArtifactsPage(): ReactNode {
  const navigate = useNavigate();
  const { concepts, loading: conceptsLoading, error: conceptsError } = useConcepts();
  const [params, setParams] = useSearchParams();
  const label = params.get(LABEL_PARAM) ?? "";
  const search = params.get(SEARCH_PARAM) ?? "";
  const showArchived = params.get(ARCHIVED_PARAM) === ARCHIVED_VALUE;

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
    uploadBusy,
    uploadProgress,
    uploadError,
    uploadMessage,
    uploadFile,
    archiveBusy,
    archiveError,
    archiveArtifact,
  } = useArtifacts(label, showArchived);

  const searchState = useArtifactSearch(search);

  // The artifact the confirm dialog is about, or null when it is closed. One
  // dialog for the whole list rather than one per row: a modal is a
  // page-level thing, and fifty of them mounted at once would be fifty
  // <dialog> elements the browser has to keep inert.
  const [pendingArchive, setPendingArchive] = useState<Row | null>(null);

  const concept = concepts.find((c) => c.id === ARTIFACT_CONCEPT_ID);
  const select = (id: string) => navigate(artifactPath(id));

  function setParam(key: string, next: string): void {
    const nextParams = new URLSearchParams(params);
    if (next === "") nextParams.delete(key);
    else nextParams.set(key, next);
    // replace: narrowing happens in place. A push here would make the back
    // button undo one filter click at a time instead of leaving the list.
    setParams(nextParams, { replace: true });
  }

  const setLabel = (next: string) => setParam(LABEL_PARAM, next);

  // Search results compose with the label filter CLIENT-SIDE, and that is a
  // fact about the builtin rather than a shortcut: librarySimilarArtifacts
  // takes text / artifactId / limit and no facet, so there is no server-side
  // way to ask "about budgets, among the ones labelled finance". The hit
  // already carries the artifact's labels (the builtin re-reads each surviving
  // row through the owner-gated query), so the narrowing needs no second read.
  const hits = label === "" ? searchState.hits : searchState.hits.filter((hit) => hit.labels.includes(label));

  return (
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          eyebrow={ARTIFACT_CONCEPT_ID}
          title="Artifacts"
          blurb="Everything this cluster's Library has indexed for you -- files you uploaded, generated outputs, and the notes, to-dos, calendar events, and memories your agents have created. Put your own labels on one to say what it was for; a label you or an agent added is a filter here too."
          actions={
            <Button size="xs" onClick={reload}>
              Refresh
            </Button>
          }
        />

        <Band
          title="Upload a file"
          meta="any type, up to this cluster's limit; the Library indexes it and analyses it in the background"
        >
          <UploadDropZone
            busy={uploadBusy}
            progress={uploadProgress}
            error={uploadError}
            message={uploadMessage}
            onUpload={(file, labels) => uploadFile({ file, labels })}
          />
        </Band>

        <Band title="New artifact" meta="records a generated output; the Library indexes it automatically">
          <NewArtifactForm busy={createBusy} error={createError} message={createMessage} onCreate={createArtifact} />
        </Band>

        <Band
          title="Search by meaning"
          meta="over what the Library has read of your files, not over their titles"
        >
          <SearchBox value={search} onChange={(next) => setParam(SEARCH_PARAM, next)} />
        </Band>

        <Band
          title={search ? "Matches" : "Artifacts"}
          meta={bandMeta(search, label, showArchived)}
          panel
        >
          {/* The facet bar renders whether or not any labels exist. The
              archived toggle lives in it, and a Library whose every row is
              archived has no labels to show -- so gating the whole bar on the
              label list would hide the one control that gets those rows
              back. */}
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
            <span className="ml-auto">
              <Button
                size="xs"
                tone={showArchived ? "primary" : "quiet"}
                pressed={showArchived}
                onClick={() => setParam(ARCHIVED_PARAM, showArchived ? "" : ARCHIVED_VALUE)}
              >
                Show archived
              </Button>
            </span>
          </div>

          {archiveError ? <ErrorNotice sentence="That artifact was not archived." next="It is still in your Library; try again." detail={archiveError} /> : null}

          {search ? (
            <SearchResults state={searchState} hits={hits} label={label} onSelect={select} />
          ) : conceptsError ? (
            <ErrorNotice sentence="Could not read the concept registry, so the Library cannot be drawn." detail={conceptsError} />
          ) : error ? (
            <ErrorNotice sentence="Could not read your Library." next="Reload the page to read it again." detail={error} />
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
              <EmptyState statement="No artifacts yet. Upload a file above, or wait for the Library to index the outputs, notes, to-dos, calendar events, and memories your agents create." />
            )
          ) : concept ? (
            <ArtifactRows
              rows={rows}
              concept={concept}
              onSelect={select}
              onArchive={setPendingArchive}
            />
          ) : (
            <Skeleton variant="rows" rows={5} />
          )}
        </Band>
      </section>

      <ConfirmDialog
        open={pendingArchive !== null}
        title="Archive this artifact?"
        confirmLabel="Archive"
        tone="danger"
        busy={archiveBusy}
        onCancel={() => setPendingArchive(null)}
        onConfirm={() => {
          const target = pendingArchive;
          setPendingArchive(null);
          if (target) archiveArtifact(rowString(target, "id"));
        }}
      >
        <p>
          {`"${rowString(pendingArchive, "title") || rowString(pendingArchive, "id")}" drops out of this list.`}
        </p>
        <p className="mt-2">
          Nothing is destroyed: the row, its labels and its provenance survive, and the Show
          archived toggle brings it back into view. A file artifact archives its stored file with
          it.
        </p>
      </ConfirmDialog>
    </Container>
  );
}

function bandMeta(search: string, label: string, showArchived: boolean): string | undefined {
  const parts: string[] = [];
  if (search) parts.push(`ranked by closeness to "${search}"`);
  if (label) parts.push(`labelled "${label}"`);
  if (showArchived) parts.push("including archived");
  return parts.length === 0 ? undefined : parts.join(" · ");
}

// ArtifactRows renders the list: one RowList per row for the card, and this
// page's own column for the two verbs. See the file header for why the split
// is here rather than inside RowList.
function ArtifactRows({
  rows,
  concept,
  onSelect,
  onArchive,
}: {
  rows: Row[];
  concept: Concept;
  onSelect: (rowId: string) => void;
  onArchive: (row: Row) => void;
}): ReactNode {
  return (
    <div className="flex flex-col gap-1">
      {rows.map((row) => {
        const id = rowString(row, "id");
        const title = rowString(row, "title") || id;
        return (
          <div key={id} className="flex items-center gap-2">
            <div className="min-w-0 flex-1">
              <RowList rows={[row]} concept={concept} onSelect={onSelect} />
            </div>
            {row["archived"] === true ? (
              <span className="text-xs text-subtle">archived</span>
            ) : null}
            <ButtonLink size="xs" href={artifactContentUrl(id)} title={`Export ${title}`}>
              <Download size={14} aria-hidden="true" />
              Export
            </ButtonLink>
            <Button size="xs" onClick={() => onArchive(row)} title={`Archive ${title}`}>
              Archive
            </Button>
          </div>
        );
      })}
    </div>
  );
}

// The ranked results. NOT RowList: a hit is the builtin's own shape -- an
// artifact folded up from its best-scoring chunk, carrying the score and the
// text that earned it -- and rendering it through the artifact display card
// would throw away the two fields that are the entire point of a search
// result. The card's own fields still come from the hit, so the two lists
// read as the same population seen two ways.
function SearchResults({
  state,
  hits,
  label,
  onSelect,
}: {
  state: { loading: boolean; error: string; searched: boolean };
  hits: ArtifactHit[];
  label: string;
  onSelect: (rowId: string) => void;
}): ReactNode {
  if (state.error) return <ErrorNotice sentence="The search did not run." next="Try it again, or narrow what you asked for." detail={state.error} />;
  if (state.loading && hits.length === 0) return <Skeleton variant="rows" rows={3} />;
  if (hits.length === 0) {
    if (!state.searched) return <Skeleton variant="rows" rows={3} />;
    return (
      <EmptyState
        statement={
          label
            ? `Nothing labelled "${label}" matches that. The Library only searches what it has read -- a file still being analysed, or one of a type it cannot read, will not match.`
            : "Nothing matches that yet. The Library only searches what it has read -- a file still being analysed, or one of a type it cannot read, will not match."
        }
      />
    );
  }

  return (
    <ol className="flex flex-col gap-2">
      {hits.map((hit, index) => (
        <li key={hit.artifactId} className="flex items-start gap-3 rounded border border-line p-2">
          <span className="pt-0.5 text-xs text-subtle">{index + 1}</span>
          <div className="min-w-0 flex-1">
            <button
              type="button"
              onClick={() => onSelect(hit.artifactId)}
              className="text-left text-sm font-medium text-fg hover:underline"
            >
              {hit.title || hit.artifactId}
            </button>
            <p className="text-xs text-muted">
              {hit.kind || "unknown kind"} · closeness <DataText kind="number">{hit.score.toFixed(3)}</DataText>
            </p>
            {hit.snippet ? (
              <p className="mt-1 max-w-prose text-xs text-muted italic">{hit.snippet}</p>
            ) : null}
          </div>
          <ButtonLink size="xs" href={artifactContentUrl(hit.artifactId)} title={`Export ${hit.title}`}>
            <Download size={14} aria-hidden="true" />
            Export
          </ButtonLink>
        </li>
      ))}
    </ol>
  );
}

function SearchBox({ value, onChange }: { value: string; onChange: (next: string) => void }): ReactNode {
  // Uncontrolled-by-the-URL draft: the query lands in the address bar on
  // submit, not on every keystroke, so typing does not push a read (and a
  // vector search) per character.
  const [draft, setDraft] = useState(value);

  function submit(event: FormEvent): void {
    event.preventDefault();
    onChange(draft.trim());
  }

  return (
    <FormRow onSubmit={submit}>
      <Field
        label="Ask the Library"
        grow
        hint="Plain language. Matches on what a file MEANS, not on its title."
      >
        <TextInput value={draft} onChange={setDraft} placeholder="the quarterly hiring plan" />
      </Field>
      <FormActions>
        <Button type="submit">
          <Search size={14} aria-hidden="true" />
          Search
        </Button>
        {value === "" ? null : (
          <Button
            onClick={() => {
              setDraft("");
              onChange("");
            }}
          >
            Clear
          </Button>
        )}
      </FormActions>
    </FormRow>
  );
}

// The drop zone is a Field-wrapped file input, not a new ui/ primitive: one
// page needs it, and ui/README's rule is that a primitive earns its place on
// the second caller. Field already owns the label / hint lines, and the
// <label> it renders is what makes the whole zone clickable -- so the drag
// handlers are the only thing this adds.
function UploadDropZone({
  busy,
  progress,
  error,
  message,
  onUpload,
}: {
  busy: boolean;
  progress: number;
  error: string;
  message: string;
  onUpload: (file: File, labels: string[]) => void;
}): ReactNode {
  const [labels, setLabels] = useState("");
  const [over, setOver] = useState(false);

  function labelList(): string[] {
    return labels
      .split(",")
      .map((one) => one.trim())
      .filter((one) => one !== "");
  }

  function accept(file: File | undefined): void {
    if (!file || busy) return;
    onUpload(file, labelList());
  }

  function drop(event: DragEvent<HTMLDivElement>): void {
    event.preventDefault();
    setOver(false);
    accept(event.dataTransfer?.files?.[0]);
  }

  return (
    <div className="flex flex-col gap-2">
      {error ? <ErrorNotice sentence="The upload did not finish." next="Nothing was added to your Library; try the file again." detail={error} /> : null}
      {message ? (
        <p role="status" className="rounded border border-line bg-raised px-3 py-2 text-sm text-fg">
          {message}
        </p>
      ) : null}
      <div
        role="group"
        aria-label="Drop a file to upload"
        onDragOver={(event) => {
          event.preventDefault();
          setOver(true);
        }}
        onDragLeave={() => setOver(false)}
        onDrop={drop}
        className={
          "rounded-lg border border-dashed p-4 " + (over ? "border-accent bg-raised" : "border-line")
        }
      >
        <Field
          label="File"
          hint="Drop a file here or choose one. Any type -- the Library stores what it cannot read, it does not refuse it."
        >
          <input
            type="file"
            // An explicit accessible name even though Field wraps this in a
            // <label>: the label's text content is "File" plus the hint line,
            // so the wrapper alone names this control after a paragraph.
            aria-label="File"
            disabled={busy}
            onChange={(event) => {
              accept(event.target.files?.[0]);
              // Clear the input so choosing the SAME file twice fires again.
              event.target.value = "";
            }}
            className="w-full text-sm text-fg file:mr-3 file:rounded file:border file:border-line file:bg-surface file:px-2.5 file:py-1 file:text-xs file:text-fg"
          />
        </Field>
      </div>
      <Field label="Labels" hint="Optional, comma-separated. Applied to the artifact this becomes.">
        <TextInput value={labels} onChange={setLabels} placeholder="finance, q3" disabled={busy} />
      </Field>
      {busy || progress > 0 ? (
        <p role="status" className="flex items-center gap-2 text-xs text-muted">
          <Upload size={14} aria-hidden="true" />
          <progress value={progress} max={1} aria-label="Upload progress" className="h-1.5 w-40" />
          <span>{`${Math.round(progress * 100)}%`}</span>
        </p>
      ) : null}
    </div>
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
      {error ? <ErrorNotice sentence="The artifact was not recorded." next="Check the fields above and try again." detail={error} /> : null}
      {message ? (
        <p role="status" className="rounded border border-line bg-raised px-3 py-2 text-sm text-fg">
          {message}
        </p>
      ) : null}
      <FormRow>
        <Field label="Title" grow>
          <TextInput value={title} onChange={setTitle} placeholder="Ten most beautiful birds" />
        </Field>
        <Field label="Summary" grow hint="Optional. Shown on the list row.">
          <TextInput value={summary} onChange={setSummary} placeholder="A short description" />
        </Field>
      </FormRow>
      <Field label="Body" grow hint="Optional. Markdown, rendered in the artifact's viewer.">
        <Textarea value={body} onChange={setBody} rows={4} placeholder="# Ten most beautiful birds…" />
      </Field>
      <div>
        <Button type="submit" busy={busy} busyLabel="Working…" disabled={title.trim() === ""}>
          Create artifact
        </Button>
      </div>
    </form>
  );
}
