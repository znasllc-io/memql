import type { ModelRow, SourceRow } from "./rows";
import { modelsPhrase, sourcesPhrase } from "./words";

export function ProvenanceMark({
  sources,
  models,
}: {
  sources: SourceRow[];
  models: ModelRow[];
}) {
  const thought = models.length > 0;
  return (
    <span className="mz-rowmark">
      <span
        className="mz-mark"
        data-mark={thought ? "filled" : "hollow"}
        role="img"
        aria-label={thought ? `composed by ${modelsPhrase(models)}` : "no model was called"}
      />
      <span className="mz-rowmark-text">{sourcesPhrase(sources)}</span>
    </span>
  );
}
