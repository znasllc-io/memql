import { useMemo, useState } from "react";

import { Button, Chip, Fact, Facts, Field, FormRow, Head, Input, Notice, Select, Subhead } from "../../kit";
import { ActionBar } from "../../kit/ActionBar";
import { actsFor, stateLine, type ActId } from "./acts";
import { ProvenanceChain } from "./Provenance";
import type { CompositionRow, ModelRow, SourceRow, TemplateRow } from "./rows";
import {
  useComposableConcepts,
  useResolvedSources,
  type ComposableConcept,
  type ResolvedSource,
} from "./useCompose";
import {
  DEPLOYABLE_KINDS,
  FORMATS,
  UNOFFERED_FORMATS,
  deployableWord,
  formatWord,
} from "./words";

// ComposerSection -- the three columns, and why there are three.
//
// ===========================================================================
// THE COLUMNS ARE THE COMPOSITION'S OWN STRUCTURE, NOT A LAYOUT
// ===========================================================================
// What goes in, what it is, what comes out. Three columns is a shape this
// shell has not used before and it earns it here because the subject
// genuinely has three parts a person moves between -- not because three
// panels looked balanced.
//
// THE DRAFT LEADS (design D5, the owner's decision). The conversation is
// a column beside the draft rather than the middle of the app, because
// the FILE is the deliverable and a surface where the deliverable is
// never on screen cannot answer "which version am I about to send". It is
// also DESIGN.md rule 11 read forward: a list and its detail never share
// a scroll column, and a transcript and its draft are that pair.
//
// ===========================================================================
// REAL ESTATE BELONGS TO CONTENT (rule 9)
// ===========================================================================
// The draft column takes what is left after two narrow ones. The two
// narrow columns are fixed-measure because they hold controls, and the
// draft is fluid because it holds prose -- which is also why the draft is
// the only text in this app set at `--os-text-md` with a longer
// line-height. It is being READ, and everything around it is being
// scanned.

export interface ComposerSectionProps {
  templates: TemplateRow[];
  /** The composition the composer is looking at, once one exists. */
  composition: CompositionRow | null;
  compositionSources: SourceRow[];
  compositionModels: ModelRow[];
  showUnmarkedConcepts: boolean;
  defaultFormat: string;
  busy: boolean;
  error: string;
  onMaterialize: (facts: {
    name: string;
    statement: string;
    format: string;
    sources: { kind: string; ref: string; label: string }[];
    draft: string;
    templateId: string;
    deployableKind: string;
  }) => void;
  onAct: (act: ActId) => void;
  /** Clears the open composition and returns to a blank form. */
  onNewComposition: () => void;
}

interface PickedSource {
  kind: string;
  ref: string;
  label: string;
}

export function ComposerSection({
  templates,
  composition,
  compositionSources,
  compositionModels,
  showUnmarkedConcepts,
  defaultFormat,
  busy,
  error,
  onMaterialize,
  onAct,
  onNewComposition,
}: ComposerSectionProps) {
  const [name, setName] = useState("");
  const [statement, setStatement] = useState("");
  const [draft, setDraft] = useState("");
  const [format, setFormat] = useState(defaultFormat);
  const [templateId, setTemplateId] = useState("");
  const [deployableKind, setDeployableKind] = useState("");
  const [picked, setPicked] = useState<PickedSource[]>([]);

  const composables = useComposableConcepts(showUnmarkedConcepts);
  const resolved = useResolvedSources(picked);

  const open = composition;
  const draftState = {
    sourceCount: picked.length,
    hasFormat: format !== "",
    submitting: busy,
  };
  const acts = actsFor(open, draftState);

  // The chain reads the OPEN composition once there is one, and the form
  // otherwise -- so the same device says "here is what you are about to
  // make" and then "here is what you made", with the same words.
  const chainSources: SourceRow[] = open
    ? compositionSources
    : picked.map((p) => ({ ...p, capturedAt: "" }));
  const chainTemplate =
    templates.find((t) => t.id === (open ? open.templateId : templateId))?.name ?? "";

  function addSource(next: PickedSource) {
    setPicked((prev) =>
      prev.some((p) => p.kind === next.kind && p.ref === next.ref) ? prev : [...prev, next],
    );
  }

  function removeSource(ref: string) {
    setPicked((prev) => prev.filter((p) => p.ref !== ref));
  }

  return (
    <div className="os-mz">
      <Head
        title={open ? open.name : "Compose"}
        meta={open ? undefined : "a file, from rows in the graph"}
      >
        {open ? (
          <Button tone="quiet" onClick={onNewComposition}>
            Compose another
          </Button>
        ) : null}
      </Head>

      {/* A SETTLED COMPOSITION IS A RECORD, NOT A FORM, and the layout says
          so: the three columns stop stretching and sit at the top. Rendered,
          a finished composition drew three short columns over 500px of dead
          space -- rule 9's "nothing paints half a window of dead space",
          which no jsdom test can see because jsdom lays nothing out. */}
      <div className="os-mz-columns" data-settled={open !== null ? "true" : undefined}>
        <SourcesColumn
          composables={composables.concepts}
          registryAvailable={composables.registryAvailable}
          loading={composables.loading}
          conceptsError={composables.error}
          // A SETTLED COLUMN READS THE COMPOSITION, NOT THE FORM. `picked`
          // is this window's own local state, and a composition opened from
          // the list -- or from another app's intent -- has none of it. The
          // first rendered pass caught this as "Made from: the record holds
          // what this was composed from" over an empty column, on a
          // composition whose record holds two rows.
          picked={open ? chainSources.map((s) => ({ kind: s.kind, ref: s.ref, label: s.label })) : picked}
          resolved={resolved.sources}
          resolveError={resolved.error}
          settled={open !== null}
          onAdd={addSource}
          onRemove={removeSource}
        />

        <DraftColumn
          open={open}
          name={name}
          onName={setName}
          statement={statement}
          onStatement={setStatement}
          draft={draft}
          onDraft={setDraft}
        />

        <TargetColumn
          templates={templates}
          format={open ? open.format : format}
          onFormat={setFormat}
          templateId={open ? open.templateId : templateId}
          onTemplate={setTemplateId}
          deployableKind={open ? open.deployableKind : deployableKind}
          onDeployableKind={setDeployableKind}
          settled={open !== null}
        />
      </div>

      {/* THE ONE DEVICE THIS APP HAS, sitting between the work and the act
          -- which is where it belongs: it is the last thing read before
          Materialize is pressed, and the first thing read after. */}
      <ProvenanceChain
        sources={chainSources}
        templateName={chainTemplate}
        format={open ? open.format : format}
        models={open ? compositionModels : []}
        embedded={open ? open.provenanceEmbedded : null}
        pending={open === null}
      />

      {error ? <Notice tone="error" sentence={error} /> : null}

      <ActionBar
        state={stateLine(open, draftState)}
        acts={acts.map((a) => ({
          label: a.label,
          tone: a.tone === "primary" ? "primary" : a.tone === "danger" ? "danger" : "quiet",
          ariaLabel: a.id === "openFile" ? `Open ${open?.name ?? "the file"} in Files` : undefined,
          busy: busy && a.id === "materialize",
          onAct: () => {
            if (a.id === "materialize") {
              onMaterialize({
                name,
                statement,
                format,
                sources: picked,
                draft,
                templateId,
                deployableKind,
              });
              return;
            }
            onAct(a.id);
          },
        }))}
      />
    </div>
  );
}

// ---------------------------------------------------------------------------
// Sources -- what goes in
// ---------------------------------------------------------------------------

function SourcesColumn({
  composables,
  registryAvailable,
  loading,
  conceptsError,
  picked,
  resolved,
  resolveError,
  settled,
  onAdd,
  onRemove,
}: {
  composables: ComposableConcept[];
  registryAvailable: boolean;
  loading: boolean;
  conceptsError: string;
  picked: PickedSource[];
  resolved: ResolvedSource[];
  resolveError: string;
  /** True once a composition exists: the sources are a RECORD, not a choice. */
  settled: boolean;
  onAdd: (s: PickedSource) => void;
  onRemove: (ref: string) => void;
}) {
  const [query, setQuery] = useState("");
  const countByRef = useMemo(() => {
    const m = new Map<string, ResolvedSource>();
    for (const r of resolved) m.set(r.ref, r);
    return m;
  }, [resolved]);

  // A SETTLED COMPOSITION'S SOURCES ARE WHAT IT WAS MADE FROM, and the
  // picker that produced them is gone rather than dead. Adding a source to
  // a composition that has already been written would be a control whose
  // only outcome is a refusal.
  if (settled) {
    return (
      <section className="os-mz-col os-mz-col-sources" aria-label="Sources">
        <Subhead>Made from</Subhead>
        {picked.length === 0 ? (
          <p className="os-caption">The record holds what this was composed from.</p>
        ) : (
          <ul className="os-mz-picked" aria-label="Sources this was made from">
            {picked.map((p) => (
              <li key={p.ref} className="os-mz-picked-row">
                <span className="os-mz-picked-name">{p.label || p.ref}</span>
              </li>
            ))}
          </ul>
        )}
      </section>
    );
  }

  return (
    <section className="os-mz-col os-mz-col-sources" aria-label="Sources">
      <Subhead>Sources</Subhead>

      {conceptsError ? (
        <Notice tone="error" sentence={conceptsError} />
      ) : !registryAvailable ? (
        // "NOTHING IS MARKED" AND "THIS NODE CANNOT SEE THE REGISTRY" look
        // identical from an empty list, and only one is fixable. So the
        // engine reports which, and this says so instead of rendering a
        // bare empty state over both.
        <Notice
          tone="warn"
          sentence="This node cannot read the concept registry, so there is nothing to offer here."
          next="A query source still works, and the Logs section holds the lines for this app."
        />
      ) : loading && composables.length === 0 ? (
        <p className="os-caption">Reading what this cluster offers</p>
      ) : composables.length === 0 ? (
        <p className="os-caption">
          No concept in this cluster is marked as worth composing from. A query source still works.
        </p>
      ) : (
        <>
          {/* IN A ROW, because the kit's `Input` carries `flex: 1` -- right
              where it belongs, inside a `FormRow`, and wrong as a bare child
              of a flex COLUMN, where growing means growing TALL. Rendered,
              this search box was 458px high with its placeholder floating in
              the middle of it; jsdom lays nothing out, so the whole suite was
              green over it. */}
          <FormRow>
            <Input
              id="mz-source-search"
              value={query}
              onChange={setQuery}
              label="Search what you can compose from"
              placeholder="Search"
            />
          </FormRow>
          <ul className="os-mz-concepts" aria-label="What you can compose from">
            {composables
              .filter((c) => matches(c, query))
              .map((c) => (
                <li key={c.id}>
                  <button
                    type="button"
                    className="os-mz-concept"
                    disabled={c.list === ""}
                    data-unmarked={c.marked ? undefined : "true"}
                    onClick={() =>
                      onAdd({ kind: "query", ref: `query ${c.list}()`, label: c.as })
                    }
                    title={c.list === "" ? "This concept declares no list query, so there is no read to run" : c.description}
                  >
                    <span className="os-mz-concept-name">{c.as}</span>
                    {c.marked ? null : <span className="os-mz-concept-tag">unmarked</span>}
                  </button>
                </li>
              ))}
          </ul>
        </>
      )}

      <Subhead>Picked</Subhead>
      {picked.length === 0 ? (
        <p className="os-caption">Nothing picked yet. Everything above is something this cluster can read for you.</p>
      ) : (
        <ul className="os-mz-picked" aria-label="Picked sources">
          {picked.map((p) => {
            const r = countByRef.get(p.ref);
            return (
              <li key={p.ref} className="os-mz-picked-row">
                <span className="os-mz-picked-name">{p.label || p.ref}</span>
                <span className="os-mz-picked-count">
                  {/* AN UNRESOLVED COUNT IS AN EM DASH, never a zero: a
                      zero here would be this window inventing a fact
                      about somebody's data before the cluster answered. */}
                  {r === undefined ? "—" : `${r.count} ${r.count === 1 ? "row" : "rows"}`}
                </span>
                <button
                  type="button"
                  className="os-mz-picked-remove"
                  onClick={() => onRemove(p.ref)}
                  aria-label={`Remove ${p.label || p.ref}`}
                >
                  Remove
                </button>
                {r?.problem ? <span className="os-mz-picked-problem">{r.problem}</span> : null}
              </li>
            );
          })}
        </ul>
      )}
      {resolveError ? <Notice tone="error" sentence={resolveError} /> : null}
    </section>
  );
}

function matches(c: ComposableConcept, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (q === "") return true;
  return c.as.toLowerCase().includes(q) || c.id.toLowerCase().includes(q);
}

// ---------------------------------------------------------------------------
// The draft -- what it is. The page.
// ---------------------------------------------------------------------------

function DraftColumn({
  open,
  name,
  onName,
  statement,
  onStatement,
  draft,
  onDraft,
}: {
  open: CompositionRow | null;
  name: string;
  onName: (v: string) => void;
  statement: string;
  onStatement: (v: string) => void;
  draft: string;
  onDraft: (v: string) => void;
}) {
  if (open !== null) {
    return (
      <section className="os-mz-col os-mz-col-draft" aria-label="What was made">
        <Subhead>What was asked for</Subhead>
        <p className="os-mz-statement">{open.statement || "No statement was recorded."}</p>
        {open.failureReason ? (
          <Notice tone="error" sentence={open.failureReason} />
        ) : (
          <p className="os-caption">
            {/* THE DRAFT IS NOT RE-RENDERED HERE, and that absence is
                deliberate: the composed text lives in the FILE now, and
                showing a second copy in this window would leave a person
                with two things that could disagree. The act that opens
                the real one is on the bar. */}
            The composed text is in the file. Open it to read what was made.
          </p>
        )}
      </section>
    );
  }

  return (
    <section className="os-mz-col os-mz-col-draft" aria-label="The draft">
      <Field label="Name">
        <Input id="mz-name" value={name} onChange={onName} label="Name" placeholder="Q3 report" />
      </Field>
      <Field label="What do you want made?">
        <Input
          id="mz-statement"
          value={statement}
          onChange={onStatement}
          label="What do you want made?"
          placeholder="Draft the Q3 report for Acme from the open invoices"
        />
      </Field>
      <Subhead>Draft</Subhead>
      <textarea
        className="os-mz-draft"
        value={draft}
        onChange={(e) => onDraft(e.target.value)}
        aria-label="Draft"
        placeholder="Leave this empty and the model writes it from your sources. Anything you type here is what it starts from."
        spellCheck
      />
    </section>
  );
}

// ---------------------------------------------------------------------------
// Target -- what comes out
// ---------------------------------------------------------------------------

function TargetColumn({
  templates,
  format,
  onFormat,
  templateId,
  onTemplate,
  deployableKind,
  onDeployableKind,
  settled,
}: {
  templates: TemplateRow[];
  format: string;
  onFormat: (v: string) => void;
  templateId: string;
  onTemplate: (v: string) => void;
  deployableKind: string;
  onDeployableKind: (v: string) => void;
  /** True once a composition exists: the target is a FACT, not a choice. */
  settled: boolean;
}) {
  const usable = templates.filter((t) => !t.archived && (t.format === format || t.format === ""));
  const chosen = templates.find((t) => t.id === templateId);

  // A SETTLED TARGET IS FACTS, NOT DISABLED CONTROLS. "Nothing happens
  // where nothing is offered" is this shell's own rule, and a column of
  // greyed selects is three controls somebody has to read past to learn
  // they are not for them. What was chosen is still worth showing, so it
  // is shown as what it now is: a record.
  if (settled) {
    return (
      <section className="os-mz-col os-mz-col-target" aria-label="Target">
        <Subhead>Target</Subhead>
        <Facts>
          <Fact label="Kind of file" value={formatWord(format)} />
          <Fact label="Template" value={chosen?.name ?? "No template"} />
          {deployableKind ? <Fact label="Deployed as" value={deployableWord(deployableKind)} /> : null}
        </Facts>
      </section>
    );
  }

  return (
    <section className="os-mz-col os-mz-col-target" aria-label="Target">
      <Subhead>Target</Subhead>

      <Field label="Kind of file">
        <Select id="mz-format" value={format} onChange={onFormat} label="Kind of file">
          {FORMATS.map((f) => (
            <option key={f} value={f}>
              {formatWord(f)}
            </option>
          ))}
        </Select>
      </Field>

      <Field label="Template">
        <Select id="mz-template" value={templateId} onChange={onTemplate} label="Template">
          <option value="">No template</option>
          {usable.map((t) => (
            <option key={t.id} value={t.id}>
              {t.name}
            </option>
          ))}
        </Select>
      </Field>
      {usable.length === 0 && templates.length > 0 ? (
        <p className="os-caption">
          None of your templates make a {formatWord(format)}. Templates lists what each one produces.
        </p>
      ) : null}

      <Field label="Deploy it as">
        <Select
          id="mz-deployable"
          value={deployableKind}
          onChange={onDeployableKind}
          label="Deploy it as"
        >
          <option value="">A file, not a deployable</option>
          {DEPLOYABLE_KINDS.map((k) => (
            <option key={k} value={k}>
              {deployableWord(k)}
            </option>
          ))}
        </Select>
      </Field>
      {deployableKind ? (
        <p className="os-caption">
          This makes a package your Library holds and Deployables can deploy unchanged. It arrives as
          a zip, not as a {formatWord(format)}.
        </p>
      ) : null}

      {/* AN ABSENT OPTION WITH NO ACCOUNT OF ITSELF reads as something
          nobody got round to building -- the rule the Bin states about its
          missing retention control. The brief names audio and video, so
          the surface names them back and says what each is waiting on. */}
      <div className="os-mz-unoffered">
        <Chip tone="neutral">Not offered yet</Chip>
        <ul>
          {UNOFFERED_FORMATS.map((u) => (
            <li key={u.name}>
              <strong>{u.name}</strong> {u.why}.
            </li>
          ))}
        </ul>
      </div>
    </section>
  );
}
