import { useMemo } from "react";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

import { Caption, Chip, Notice, Panel, Subhead } from "../../kit";
import { readSchema, standingSentence, type SchemaField } from "./schema";

// What the concept declares, joined against what its rows carry.
//
// The join is the point -- see `schema.ts`'s header. A declared field no row
// carries, and a key no field declares, are both real defects with no other
// symptom, and neither is visible in the DSL file or in any shaped read.

export function SchemaPanel({
  concept,
  rows,
  showUndeclared,
}: {
  concept: Concept;
  rows: readonly Row[];
  showUndeclared: boolean;
}) {
  const reading = useMemo(() => readSchema(concept, rows), [concept, rows]);
  const fields = showUndeclared
    ? reading.fields
    : reading.fields.filter((f) => f.standing !== "undeclared");

  const unwritten = reading.fields.filter((f) => f.standing === "declared-not-seen").length;
  const undeclared = reading.fields.filter((f) => f.standing === "undeclared").length;

  return (
    <Panel label="Schema">
      <Subhead>Schema</Subhead>

      {/* EMPTY IS NOT "no fields". A server that publishes no declared shape
          is a different answer from a concept with nothing in it, and
          collapsing the two renders a real concept as blank. */}
      {reading.shapeUnpublished ? (
        <Notice
          tone="info"
          sentence="This cluster does not publish a declared shape for this concept."
          next={
            rows.length === 0
              ? "Load some rows and the fields they carry will be listed here."
              : "The fields below are what the loaded rows carry, not what the concept declares."
          }
        />
      ) : null}

      {fields.length === 0 ? (
        <Caption>
          {rows.length === 0
            ? "No declared fields, and no rows loaded yet to profile."
            : "No fields."}
        </Caption>
      ) : (
        <ul className="os-schema-list">
          {fields.map((field) => (
            <FieldLine key={field.name} field={field} sampleSize={reading.sampleSize} />
          ))}
        </ul>
      )}

      {/* The two findings, said once, under the list rather than repeated
          against every row that has them. */}
      {unwritten > 0 || (showUndeclared && undeclared > 0) ? (
        <Caption>
          {unwritten > 0
            ? `${unwritten} declared ${unwritten === 1 ? "field is" : "fields are"} not carried by any of the ${reading.sampleSize} rows loaded. `
            : ""}
          {showUndeclared && undeclared > 0
            ? `${undeclared} ${undeclared === 1 ? "key is" : "keys are"} carried by rows and declared by nothing.`
            : ""}
        </Caption>
      ) : null}
    </Panel>
  );
}

function FieldLine({ field, sampleSize }: { field: SchemaField; sampleSize: number }) {
  return (
    <li className={`os-schema-row os-schema-${field.standing}`}>
      <div className="os-schema-head">
        <span className="os-schema-name">{field.name}</span>
        {field.kind === "" ? null : <span className="os-schema-kind">{field.kind}</span>}
        {field.required ? <Chip tone="muted">required</Chip> : null}
        {/* Only the two exceptional standings are marked. Marking the
            ordinary one would put a chip on nearly every line and hide
            these. */}
        {field.standing === "declared-not-seen" ? (
          <Chip tone="accent" title={standingSentence(field, sampleSize)}>
            not seen
          </Chip>
        ) : null}
        {field.standing === "undeclared" ? (
          <Chip tone="accent" title={standingSentence(field, sampleSize)}>
            undeclared
          </Chip>
        ) : null}
      </div>
      {field.description === "" ? null : (
        <p className="os-schema-desc">{field.description}</p>
      )}
      {field.enumValues.length === 0 ? null : (
        <p className="os-schema-enum">{field.enumValues.join(" / ")}</p>
      )}
      {/* The observed types matter where they DISAGREE with the declaration
          or where there is no declaration at all -- an undeclared key's
          types are the only thing known about it. */}
      {field.standing === "undeclared" && field.observedTypes.length > 0 ? (
        <p className="os-schema-observed">{field.observedTypes.join(" | ")}</p>
      ) : null}
    </li>
  );
}
