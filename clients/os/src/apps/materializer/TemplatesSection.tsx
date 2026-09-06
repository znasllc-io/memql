import { useState } from "react";

import { Button, Caption, Chip, Field, Head, Input, Notice, Panel, Row, Select, Subhead, formatMoment } from "../../kit";
import type { NewTemplateFacts } from "./actions";
import type { RecipeRow, TemplateRow } from "./rows";
import { FORMATS, RECIPES_EMPTY, TEMPLATES_EMPTY, formatWord } from "./words";

// TemplatesSection -- what makes an output repeatable, in two halves.
//
// ===========================================================================
// TEMPLATES AND RECIPES ARE DIFFERENT ANSWERS TO THE SAME QUESTION, so
// they share a section rather than each taking one.
// ===========================================================================
// A TEMPLATE makes the output LOOK the same: a branded document, an HTML
// shell. A RECIPE makes it BE the same: the sources, the target and the
// template, re-run against whatever the graph holds now. Somebody asking
// "how do I make this again next quarter" is asking about both, and two
// sections would make them answer it twice.
//
// ===========================================================================
// A TEMPLATE IS A BINDING, AND THE BYTES ARE THE LIBRARY'S
// ===========================================================================
// The form takes a Library FILE ID, not an upload. That is design D7 made
// visible: uploading is what the Files app is for, and this app binding a
// file it did not upload is what gives templates versions, chunked
// transfers and archive-to-Bin without any of them being built here. The
// caption says so, because a form asking for an id with no explanation
// reads as an unfinished upload control.

export interface TemplatesSectionProps {
  templates: TemplateRow[];
  recipes: RecipeRow[];
  busy: boolean;
  error: string;
  onCreateTemplate: (facts: NewTemplateFacts) => void;
  onArchiveTemplate: (templateId: string) => void;
  onRestoreTemplate: (templateId: string) => void;
  onRunRecipe: (recipeId: string) => void;
  onArchiveRecipe: (recipeId: string) => void;
  onRestoreRecipe: (recipeId: string) => void;
  showArchived: boolean;
}

export function TemplatesSection({
  templates,
  recipes,
  busy,
  error,
  onCreateTemplate,
  onArchiveTemplate,
  onRestoreTemplate,
  onRunRecipe,
  onArchiveRecipe,
  onRestoreRecipe,
  showArchived,
}: TemplatesSectionProps) {
  const [adding, setAdding] = useState(false);
  const visibleTemplates = templates.filter((t) => showArchived || !t.archived);
  const visibleRecipes = recipes.filter((r) => showArchived || !r.archived);

  return (
    <div className="os-mz-templates">
      <Head title="Templates" meta={`${visibleTemplates.length} bound`}>
        <Button tone="primary" onClick={() => setAdding((v) => !v)}>
          {adding ? "Cancel" : "Bind a file"}
        </Button>
      </Head>

      {error ? <Notice tone="error" sentence={error} /> : null}

      {adding ? (
        <NewTemplateForm
          busy={busy}
          onCreate={(facts) => {
            onCreateTemplate(facts);
            setAdding(false);
          }}
        />
      ) : null}

      {visibleTemplates.length === 0 ? (
        <p className="os-caption">{TEMPLATES_EMPTY}</p>
      ) : (
        <ul className="os-mz-rows" aria-label="Templates">
          {visibleTemplates.map((t) => (
            <li key={t.id}>
              {/* THE ACTS GO IN THE `state` SLOT, which is the row's
                  right-aligned end. Put in `children` they flowed inline
                  after the description and read as part of the sentence --
                  "The branded report we send Acme  Archive" -- which a
                  rendered pass caught and jsdom cannot see. */}
              <Row
                name={t.name}
                dim={t.archived}
                state={
                  <>
                    <Chip tone="neutral">{formatWord(t.format)}</Chip>
                    {t.archived ? (
                      <Button tone="quiet" onClick={() => onRestoreTemplate(t.id)}>
                        Restore
                      </Button>
                    ) : (
                      <Button tone="quiet" onClick={() => onArchiveTemplate(t.id)}>
                        Archive
                      </Button>
                    )}
                  </>
                }
              >
                <span className="os-mz-template-desc">
                  {t.description || "No description"}
                  {t.placeholders.length > 0
                    ? ` — asks for ${t.placeholders.map((p) => p.name).join(", ")}`
                    : ""}
                </span>
              </Row>
            </li>
          ))}
        </ul>
      )}

      <Subhead>Recipes</Subhead>
      <Caption>
        A recipe re-runs a composition against whatever the graph holds now — it stores the
        selection, not the rows the selection returned. A re-run that matches what was done before
        reaches no model at all.
      </Caption>

      {visibleRecipes.length === 0 ? (
        <p className="os-caption">{RECIPES_EMPTY}</p>
      ) : (
        <ul className="os-mz-rows" aria-label="Recipes">
          {visibleRecipes.map((r) => (
            <li key={r.id}>
              <Row
                name={r.name}
                dim={r.archived}
                state={
                  <>
                    <Chip tone="neutral">{formatWord(r.format)}</Chip>
                    {r.archived ? (
                      <Button tone="quiet" onClick={() => onRestoreRecipe(r.id)}>
                        Restore
                      </Button>
                    ) : (
                      <>
                        <Button tone="quiet" onClick={() => onArchiveRecipe(r.id)}>
                          Archive
                        </Button>
                        {/* DEFAULT, NOT PRIMARY. A primary tone on every row
                            of a list is a column of green, and the accent
                            stops meaning "this is the thing to do" when
                            every row claims it. The one primary act on this
                            section is the Head's Bind a file. */}
                        <Button busy={busy} onClick={() => onRunRecipe(r.id)}>
                          Run it again
                        </Button>
                      </>
                    )}
                  </>
                }
              >
                <span className="os-mz-template-desc">
                  {/* ABSENT IS NOT ZERO for most fields in this shell, and
                      here it genuinely IS zero: nothing wrote runCount
                      before the concept existed, because the concept is
                      new. So "not yet" is honest rather than a guess.

                      `lastRunAt` is rendered CONTINUOUSLY and is absent
                      from the arrival-cue fingerprint: a recipe on a
                      schedule would touch it forever, and naming it as news
                      would strobe this list on that cycle. It goes through
                      the kit's own formatter -- a raw RFC3339 string is the
                      data voice, and "when did this last run" is a question
                      a person asks in months. */}
                  {r.runCount === 0
                    ? "Not run yet"
                    : `Made ${r.runCount} ${r.runCount === 1 ? "time" : "times"}, last on ${formatMoment(r.lastRunAt)}`}
                  {r.description ? ` — ${r.description}` : ""}
                </span>
              </Row>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function NewTemplateForm({
  busy,
  onCreate,
}: {
  busy: boolean;
  onCreate: (facts: NewTemplateFacts) => void;
}) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [fileId, setFileId] = useState("");
  const [format, setFormat] = useState("docx");

  return (
    <Panel label="Bind a Library file as a template">
      <Field label="Name">
        <Input id="mz-tpl-name" value={name} onChange={setName} label="Name" placeholder="Acme quarterly" />
      </Field>
      <Field label="What it is for">
        <Input
          id="mz-tpl-desc"
          value={description}
          onChange={setDescription}
          label="What it is for"
          placeholder="The branded report we send Acme"
        />
      </Field>
      <Field label="Library file">
        <Input
          id="mz-tpl-file"
          value={fileId}
          onChange={setFileId}
          label="Library file"
          placeholder="The file's id, from Files"
        />
      </Field>
      <Field label="What it produces">
        <Select id="mz-tpl-format" value={format} onChange={setFormat} label="What it produces">
          {FORMATS.map((f) => (
            <option key={f} value={f}>
              {formatWord(f)}
            </option>
          ))}
        </Select>
      </Field>
      <Caption>
        Upload the file in Files first — a template is a binding to a file your Library already
        holds, so it keeps that file's versions and archives with it.
      </Caption>
      <Button
        tone="primary"
        busy={busy}
        onClick={() => onCreate({ name, description, fileId, format })}
      >
        Bind it
      </Button>
    </Panel>
  );
}
