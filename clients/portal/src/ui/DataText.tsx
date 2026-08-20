import type { ReactNode } from "react";

// The data voice. Everything that came out of the graph -- ids, counts,
// timestamps, field values -- renders in the mono face, and numbers/strings
// take the site's data tints (mint / amber). Chrome never wears these; the
// two-voice split is what lets an operator's eye separate "the interface"
// from "the data" without reading either.
//
//   number  counts, sizes, versions-as-numbers -- the mint tint.
//   string  values quoted from a payload -- the amber tint.
//   id      row/concept ids: mono, muted, break-all (they wrap, not clip --
//           an id an operator cannot fully see is an id they cannot trust).
//   time    timestamps: mono, muted.

export type DataKind = "number" | "string" | "id" | "time";

const KIND: Record<DataKind, string> = {
  number: "text-data-number",
  string: "text-data-string",
  id: "break-all text-muted",
  time: "text-muted",
};

export function DataText({
  kind,
  title,
  children,
}: {
  kind: DataKind;
  title?: string;
  children: ReactNode;
}): ReactNode {
  return (
    <span {...(title === undefined ? {} : { title })} className={`font-mono ${KIND[kind]}`}>
      {children}
    </span>
  );
}
