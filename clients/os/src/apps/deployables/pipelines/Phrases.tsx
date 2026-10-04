/**
 * An outcome's detail ("2 stages, deploy skipped, 3m 9s"), wrapping only
 * BETWEEN its phrases, never inside one: a narrow column broke "3m 9s" across
 * two lines, which reads as two facts. The Runs list and a source's Checks
 * draw their details through it.
 */
export function Phrases({ text }: { text: string }) {
  const parts = text.split(", ");
  return (
    <>
      {parts.map((part, i) => (
        <span key={i}>
          <span className="pipeline-nowrap">{part}</span>
          {i < parts.length - 1 ? ", " : ""}
        </span>
      ))}
    </>
  );
}
