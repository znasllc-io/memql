import { useEffect, useRef, useState } from "react";
import { ArrowLeft, Code2 } from "lucide-react";
import type { Concept } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, Chip, Head, Notice } from "../../kit";
import { useSession } from "../../chrome/access";
import { conceptHandoffUrl, openHandoff, VSCODE_NO_ANSWER_MESSAGE } from "../../items/vscode";
import { originBadgeFor, originBadgeLabel } from "./registry";
import { RowsPanel } from "./RowsPanel";
import { SchemaPanel } from "./SchemaPanel";
import type { ConceptsSettings } from "./settings";
import { useConceptRows } from "./useConceptRows";
import type { RegistryState } from "./useConceptRegistry";

// One concept: what it declares, and what it holds.
//
// ===========================================================================
// TWO COLUMNS, TWO SCROLLERS, ONE HEAD (DESIGN.md rule 11)
// ===========================================================================
// The schema is a REFERENCE that does not change while you read rows, and
// the rows are a window you page through. Stacking them puts the thing you
// came back to check above a list you have scrolled past, so they sit side
// by side with their own scrollers -- the `.os-bin-list` / `.os-bin-detail`
// shape. At a narrow window they stack, schema first.
//
// The ROW DETAIL replaces the rows list inside the right column rather than
// becoming a third one. Three columns at this width is how rule 9's "nothing
// paints half a window of dead space" gets broken, and a row belongs where
// the rows were.
//
// The page REPLACES the registry list and carries the back control in its
// own Head, which is the other half of rule 11: one Head per scroller.
//
// ===========================================================================
// ONE WALK, READ BY BOTH COLUMNS
// ===========================================================================
// The schema's "observed" half is derived from the rows this browser has
// loaded, which are the rows the right-hand column is showing. So the walk
// is opened HERE and handed to both, rather than each column opening its
// own. Two walks would page independently, so the schema would describe a
// different sample from the one on screen -- and the Accounts app's rule
// applies exactly: two readings inside one app are free to disagree while
// each decides what to render.

export function ConceptPage({
  conceptId,
  concept,
  registryState,
  settings,
  onBack,
}: {
  conceptId: string;
  /** Null while the registry is still seeding, or if it holds no such id. */
  concept: Concept | null;
  registryState: RegistryState;
  settings: ConceptsSettings;
  onBack: () => void;
}) {
  // A concept the registry does not hold. Two very different causes, and
  // they must not read the same: still arriving, or genuinely not here.
  if (concept === null) {
    return (
      <div className="os-app-stack os-concept-page">
        <Head title={conceptId}>
          <Button tone="quiet" onClick={onBack}>
            <ArrowLeft size={13} aria-hidden /> Concepts
          </Button>
        </Head>
        {registryState === "seeding" ? (
          <Caption>Reading the registry from the cluster.</Caption>
        ) : (
          <Notice
            tone="warn"
            sentence={`This cluster declares no concept called ${conceptId}.`}
            next="It may have been renamed, or it may belong to a package this cluster does not run."
          />
        )}
      </div>
    );
  }
  return <ConceptBody concept={concept} settings={settings} onBack={onBack} />;
}

function ConceptBody({
  concept,
  settings,
  onBack,
}: {
  concept: Concept;
  settings: ConceptsSettings;
  onBack: () => void;
}) {
  const { config } = useSession();
  const [vsNoAnswer, setVsNoAnswer] = useState(false);
  const cancelHandoff = useRef<(() => void) | null>(null);
  useEffect(() => () => cancelHandoff.current?.(), []);

  const walk = useConceptRows(concept.id, settings.pageSize);
  const badge = originBadgeFor(concept);

  function openInEditor() {
    setVsNoAnswer(false);
    cancelHandoff.current?.();
    cancelHandoff.current = openHandoff(conceptHandoffUrl(config.domain, concept.id), () =>
      setVsNoAnswer(true),
    );
  }

  return (
    <div className="os-app-stack os-concept-page">
      <Head title={concept.entity} meta={concept.id}>
        <Button tone="quiet" onClick={onBack}>
          <ArrowLeft size={13} aria-hidden /> Concepts
        </Button>
        {/* The reverse of the extension's own handoff: it opens the console
            at a concept's rows, and this opens the editor at its
            definition. */}
        <Button tone="quiet" onClick={openInEditor} ariaLabel="Open this concept in VS Code">
          <Code2 size={13} aria-hidden /> Open in VS Code
        </Button>
      </Head>

      <div className="os-concept-identity">
        {concept.description === "" ? null : (
          <p className="os-concept-lede">{concept.description}</p>
        )}
        <div className="os-concept-marks">
          {concept.type === "" ? null : <Chip tone="muted">{concept.type}</Chip>}
          {concept.version === "" ? null : <Chip tone="muted">{concept.version}</Chip>}
          {badge.kind === "none" ? null : (
            <Chip tone={badge.kind === "mirror" ? "accent" : "muted"}>
              {originBadgeLabel(badge)}
            </Chip>
          )}
        </div>
        {/* A MIRROR CHANGES WHAT A CALLER MAY DO, so it is stated rather
            than badged and left. The engine refuses every write to a mirror
            that does not come from its connector -- including a cluster
            owner's, and including one made through a mutation that looks
            perfectly ordinary. */}
        {badge.kind === "mirror" ? (
          <Notice
            tone="info"
            sentence={`${concept.entity} is a mirror. ${badge.origin} owns this data.`}
            next="Changes are made there and copied here. The engine refuses every write to these rows that does not come from that connector."
          />
        ) : null}
        {vsNoAnswer ? <Notice tone="warn" sentence={VSCODE_NO_ANSWER_MESSAGE} /> : null}
      </div>

      <div className="os-concept-body">
        <div className="os-concept-schema">
          <SchemaPanel
            concept={concept}
            rows={walk.rows}
            showUndeclared={settings.showUndeclaredFields}
          />
        </div>
        <div className="os-concept-rows">
          <RowsPanel concept={concept} walk={walk} />
        </div>
      </div>
    </div>
  );
}
