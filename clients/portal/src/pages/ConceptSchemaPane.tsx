import { useMemo, type ReactNode } from "react";
import { inferDisplayCard } from "@znasllc-io/memql-view-kit";

import { flattenForList } from "../viewkit/rows";
import { describeConceptSchema, type ConceptSchemaView } from "../concepts/schema";
import { Empty, Loading } from "../components/StatusMessage";
import { useConceptPane } from "./conceptContext";

// The schema view: a concept's fields, their types, and their annotations.
//
// See src/concepts/schema.ts for where the schema comes from -- and for why
// DslSpecMsg, despite being the obvious-sounding candidate, is NOT it (it
// exports the DSL's authoring grammar, which knows nothing about any
// individual concept).
//
// The pane always says WHICH KIND of answer it is showing. "The concept
// declares this field" and "every row I loaded happened to carry it" are
// different claims, and an operator reading a field table to decide what a
// mutation must supply has to be able to tell them apart.

export function ConceptSchemaPane(): ReactNode {
  const { concept, rows } = useConceptPane();
  const walkRows = rows.walk.rows;

  const view = useMemo(() => describeConceptSchema(walkRows), [walkRows]);

  // The display card is part of the concept's declared surface, and the one
  // piece of it an operator can otherwise only infer by staring at the row
  // list. Shown with its PROVENANCE: a declared card is the author's choice,
  // an inferred one is view-kit's documented fallback
  // (docs/public/concepts/display-cards.md), and conflating them would make
  // the annotation look like it says something it does not.
  const inferred = useMemo(
    () => inferDisplayCard(walkRows.map(flattenForList)),
    [walkRows],
  );
  const card = concept.displayCard ?? inferred;
  const cardDeclared = concept.displayCard !== undefined;

  return (
    <div className="flex max-w-4xl flex-col gap-6 pb-6">
      {/* The registry metadata -- id, domain, entity, version, node type,
          description -- is NOT repeated here. ConceptPage's header already
          carries all of it, on every tab; a second copy on one tab is noise
          that also makes the two able to disagree. */}
      <section>
        <SectionHeading>
          Display card
          <Badge tone={cardDeclared ? "declared" : "inferred"}>
            {cardDeclared ? "declared" : "inferred"}
          </Badge>
        </SectionHeading>
        <p className="mb-2 text-xs text-muted">
          {cardDeclared
            ? "The concept declares these slots; the row list projects through exactly them."
            : "The concept declares no display card, so the row list falls back to the " +
              "documented inference over the field names the loaded rows carry."}
        </p>
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 rounded-lg border border-line bg-surface p-3 text-sm">
          <Term label="Primary" value={card.primary ?? "(row id)"} mono />
          <Term label="Secondary" value={card.secondary ?? ""} mono />
          <Term label="Tertiary" value={card.tertiary ?? ""} mono />
          <Term label="Status" value={card.status ?? ""} mono />
        </dl>
      </section>

      <section>
        <SectionHeading>
          Fields
          {view.source === "none" ? null : (
            <Badge tone={view.source}>
              {/* Worded differently from the display card's badge on purpose.
                  Both panes can be showing a "declared" answer at once, and
                  two identical words a few lines apart is how a reader ends
                  up attributing one to the other. */}
              {view.source === "declared" ? "from the declaration" : "observed from rows"}
            </Badge>
          )}
        </SectionHeading>
        <FieldTable view={view} loading={rows.walk.status === "loading"} />
      </section>

      {view.document ? (
        <section>
          <SectionHeading>Schema document</SectionHeading>
          <details className="rounded-lg border border-line bg-surface">
            <summary className="cursor-pointer px-3 py-2 text-sm text-muted">
              The JSON Schema the engine validates writes against
            </summary>
            <pre className="overflow-x-auto border-t border-line px-3 py-2 font-mono text-xs text-fg">
              {JSON.stringify(view.document, null, 2)}
            </pre>
          </details>
        </section>
      ) : null}
    </div>
  );
}

function FieldTable({
  view,
  loading,
}: {
  view: ConceptSchemaView;
  loading: boolean;
}): ReactNode {
  if (view.fields.length === 0) {
    if (loading) return <Loading what="the schema" />;
    return (
      <Empty>
        No schema is available for this concept. The declared document rides on a
        row&apos;s <code className="font-mono">schema</code> intrinsic, and the observed
        shape needs at least one row — this concept currently has neither.
      </Empty>
    );
  }

  return (
    <>
      {view.source === "observed" ? (
        <p className="mb-2 text-xs text-muted">
          Derived from the {view.sampleSize} rows loaded so far, not from the concept
          declaration. &ldquo;Required&rdquo; here means every one of those rows carried
          the field — which is weaker than the concept declaring it required.
        </p>
      ) : null}
      <div className="overflow-x-auto rounded-lg border border-line bg-surface">
        <table className="w-full border-collapse text-sm">
          <thead>
            <tr className="border-b border-line text-left text-xs text-muted">
              <th scope="col" className="px-3 py-2 font-medium">
                Field
              </th>
              <th scope="col" className="px-3 py-2 font-medium">
                Type
              </th>
              <th scope="col" className="px-3 py-2 font-medium">
                Required
              </th>
              <th scope="col" className="px-3 py-2 font-medium">
                Annotations
              </th>
            </tr>
          </thead>
          <tbody>
            {view.fields.map((field) => (
              <tr key={field.name} className="border-b border-line/60 last:border-0">
                <td className="px-3 py-1.5 align-top font-mono text-xs text-fg">
                  {field.name}
                  {field.description ? (
                    <span className="mt-0.5 block font-sans text-xs text-muted">
                      {field.description}
                    </span>
                  ) : null}
                </td>
                <td className="px-3 py-1.5 align-top font-mono text-xs text-muted">
                  {field.type}
                </td>
                <td className="px-3 py-1.5 align-top text-xs">
                  {field.required ? (
                    <span className="text-fg">yes</span>
                  ) : (
                    <span className="text-subtle">no</span>
                  )}
                </td>
                <td className="px-3 py-1.5 align-top">
                  <span className="flex flex-wrap gap-1">
                    {field.annotations.map((annotation) => (
                      <span
                        key={annotation}
                        className="rounded border border-line px-1.5 py-0.5 font-mono text-xs text-muted"
                      >
                        {annotation}
                      </span>
                    ))}
                  </span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  );
}

function SectionHeading({ children }: { children: ReactNode }): ReactNode {
  return (
    <h2 className="mb-2 flex items-center gap-2 text-xs font-semibold tracking-wide text-muted uppercase">
      {children}
    </h2>
  );
}

function Badge({
  tone,
  children,
}: {
  tone: "declared" | "inferred" | "observed" | "none";
  children: ReactNode;
}): ReactNode {
  return (
    <span
      className={
        "rounded-full px-2 py-0.5 text-xs font-medium normal-case " +
        (tone === "declared"
          ? "bg-accent-subtle text-fg"
          : "bg-warn-subtle text-fg")
      }
    >
      {children}
    </span>
  );
}

function Term({
  label,
  value,
  mono,
}: {
  label: string;
  value: string;
  mono?: boolean;
}): ReactNode {
  return (
    <>
      <dt className="text-xs text-muted">{label}</dt>
      <dd className={"break-all " + (mono ? "font-mono text-xs" : "text-sm")}>
        {value === "" ? <span className="text-subtle">—</span> : value}
      </dd>
    </>
  );
}
