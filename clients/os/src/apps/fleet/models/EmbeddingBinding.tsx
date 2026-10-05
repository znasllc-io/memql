import { useEffect, useState } from "react";
import { Button } from "../../../kit";
import { flatten } from "../../../kit/rows";
import { useOsConnection } from "../../../live/connection";
import { useSessionIfPresent } from "../../../chrome/access";

/** Explicit cluster configuration, separate from whether a machine has pulled
 * the model. The server probes the model and owns activation and authorization. */
export function EmbeddingBinding({ model, online }: { model: string; online: boolean }) {
  const query = useOsConnection()?.query;
  const owner = useSessionIfPresent()?.access?.role === "owner";
  const [active, setActive] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [revision, setRevision] = useState(0);
  useEffect(() => {
    let disposed = false;
    setError("");
    if (query) void query.activeEmbedderBinding().then(result => {
      const row = result.rows()[0]; const binding = row ? flatten(row) : {};
      if (!disposed) setActive(typeof binding.activatedAt === "string" && binding.activatedAt ? String(binding.providerRef || "") : "");
    }).catch(error => { if (!disposed) setError(String(error)); });
    return () => { disposed = true; };
  }, [query, revision]);
  async function bind() {
    if (!query || busy) return;
    setBusy(true); setError("");
    try { await query.bindEmbedder({provider:`fleet:${model}`}); setActive(`fleet:${model}`); }
    catch (error) { setError(error instanceof Error ? error.message : String(error)); }
    finally { setBusy(false); }
  }
  return <div className="fleet-embedding-binding">
    {active === `fleet:${model}` ? <p className="os-caption">Used for memory search</p> : owner && active === "" ? <Button disabled={!online} busy={busy} busyLabel="Checking model…" onClick={() => void bind()}>Use for memory search</Button> : null}
    {error ? <p role="alert">{error}</p> : null}
    {error && active === null ? <Button onClick={() => setRevision(value => value + 1)}>Retry</Button> : null}
  </div>;
}
