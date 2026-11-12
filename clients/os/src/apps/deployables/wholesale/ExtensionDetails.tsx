import { Caption, DetailsSection, Fact, Facts } from "../../../kit";
import type { WholesaleApplication } from "./model";

// THE CLIENT'S OWN FIELDS ON ONE APPLICATION (wholesaleGet's `details`), as
// `Facts` labelled by each field's own label -- the words the client's
// package gave it, never the field's name in its schema. "No further details"
// for an application with none (one the store's people added, a store whose
// form no client extends), and a plain sentence when the caller cannot read
// the client's row.

export function ExtensionDetails({ details }: { details: WholesaleApplication["details"] }) {
  return (
    <DetailsSection title="Their details">
      {details.state === "unreadable" ? (
        <Caption>These details are not readable to you</Caption>
      ) : details.fields.length === 0 ? (
        <Caption>No further details</Caption>
      ) : (
        <Facts>
          {details.fields.map((f, i) => <Fact key={`${f.label}:${i}`} label={f.label} value={f.value} copyValue={f.value || undefined} />)}
        </Facts>
      )}
    </DetailsSection>
  );
}
