import type { ModelRow, SourceRow } from "./rows";
import { formatWord, modelsPhrase, provenanceClaim, sourcesPhrase } from "./words";

// Provenance.tsx -- this app's one distinctive device, and the only place
// it spends any.
//
// ===========================================================================
// THE CHAIN IS THE CLAIM
// ===========================================================================
// Everything else in this shell is a list, a rail or a form, and this app
// is deliberately built out of those. What it has that nothing else does
// is one fact worth drawing: THIS FILE, FROM THESE ROWS, THROUGH THIS
// TEMPLATE, BY THESE MODELS -- and whether that survives the file leaving
// the cluster.
//
// So the chain is drawn as a chain: three named links with the direction
// between them, because the direction is a RULE (sources feed the
// template, which produces the file) and this shell already draws a
// direction where one is a rule -- the Files app's backup wire does
// exactly that, for exactly that reason.
//
// It is ONE LINE and it is live. As somebody changes the target the last
// link and the claim change under them, which is how a person learns the
// "some formats cannot carry it" rule by using the app rather than by
// reading a docs page.
//
// ===========================================================================
// WEIGHT, NOT HUE
// ===========================================================================
// The models link is drawn in the WORK APP'S OWN INK AXIS: a hollow mark
// where no model was reached, a filled one where one was. That is the
// same fact the run spine draws and the same vocabulary, deliberately --
// reusing it is consistency rather than repetition, and it means somebody
// who has read one run already knows what a filled mark means here.
//
// Hue was the wrong axis for the reason the Work app records: amber is
// `warn`, red is `error` and the accent is live/primary/yes-here, so a
// colour-per-state legend would put status hues on a partition that has
// nothing to do with status. Everything here is `--os-ink` and
// `--os-muted`, so it survives greyscale and every theme pack.

export interface ProvenanceChainProps {
  sources: SourceRow[];
  templateName: string;
  format: string;
  models: ModelRow[];
  /** null while nothing has decided yet -- rendered as no claim at all. */
  embedded: boolean | null;
  /** The composer renders the pending shape; the record renders the settled one. */
  pending?: boolean;
}

/**
 * The chain, as one line.
 *
 * IT IS A LIST, SEMANTICALLY. The links are ordered and the order is the
 * information, so a screen reader reads them in order with the direction
 * spoken between them -- an arrow that is only a glyph is a link somebody
 * cannot hear.
 */
export function ProvenanceChain({
  sources,
  templateName,
  format,
  models,
  embedded,
  pending = false,
}: ProvenanceChainProps) {
  const claim = provenanceClaim(embedded, format);
  const thought = models.length > 0;

  return (
    <div className="os-mz-chain" data-pending={pending ? "true" : undefined}>
      <ol className="os-mz-chain-links">
        <ChainLink label="from" value={sourcesPhrase(sources)} />
        <ChainLink label="through" value={templateName.trim() || "no template"} muted={!templateName.trim()} />
        <ChainLink
          label="by"
          value={modelsPhrase(models)}
          mark={thought ? "filled" : "hollow"}
          markLabel={thought ? "a model was called" : "no model was called"}
        />
        <ChainLink label="as" value={formatWord(format)} strong />
      </ol>
      {claim ? <p className="os-mz-chain-claim">{claim}</p> : null}
    </div>
  );
}

function ChainLink({
  label,
  value,
  strong = false,
  muted = false,
  mark,
  markLabel,
}: {
  label: string;
  value: string;
  strong?: boolean;
  muted?: boolean;
  mark?: "filled" | "hollow";
  markLabel?: string;
}) {
  return (
    <li className="os-mz-link" data-strong={strong ? "true" : undefined} data-muted={muted ? "true" : undefined}>
      <span className="os-mz-link-label">{label}</span>
      <span className="os-mz-link-value">
        {mark ? (
          <span
            className="os-mz-mark"
            data-mark={mark}
            role="img"
            aria-label={markLabel}
          />
        ) : null}
        {value}
      </span>
    </li>
  );
}

/**
 * The compact form, for a row in the Materialized list.
 *
 * IT DROPS THE TEMPLATE AND THE FORMAT, keeping only the two facts that
 * differ between rows a person is scanning: how much went in, and whether
 * a model was involved. The format is already a chip on the row and the
 * template is a fact you open a composition to read, so repeating either
 * here would be the surface saying it twice (rule 7).
 */
export function ProvenanceMark({
  sources,
  models,
}: {
  sources: SourceRow[];
  models: ModelRow[];
}) {
  const thought = models.length > 0;
  return (
    <span className="os-mz-rowmark">
      <span
        className="os-mz-mark"
        data-mark={thought ? "filled" : "hollow"}
        role="img"
        aria-label={thought ? `composed by ${modelsPhrase(models)}` : "no model was called"}
      />
      <span className="os-mz-rowmark-text">{sourcesPhrase(sources)}</span>
    </span>
  );
}
