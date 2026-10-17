import { useEffect, useState } from "react";
import { Button } from "../../../kit";
import { flatten } from "../../../kit/rows";
import { useOsConnection } from "../../../live/connection";

/** Model details report the cluster's binding; assignments belong on the board. */
export function EmbeddingStatus({ model }: { model: string }) {
  const query = useOsConnection()?.query;
  const [active, setActive] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [revision, setRevision] = useState(0);
  useEffect(() => {
    let disposed = false;
    setActive(null);
    setError("");
    if (query) void query.activeEmbedderBinding().then(result => {
      const row = result.rows()[0]; const binding = row ? flatten(row) : {};
      if (!disposed) setActive(typeof binding.activatedAt === "string" && binding.activatedAt ? String(binding.providerRef || "") : "");
    }).catch(error => { if (!disposed) setError(String(error)); });
    return () => { disposed = true; };
  }, [query, revision]);
  return <div className="fleet-embedding-binding">
    {active === `fleet:${model}` ? <p className="os-caption">Used for memory search</p> : null}
    {error ? <p role="alert">{error}</p> : null}
    {error && active === null ? <Button onClick={() => setRevision(value => value + 1)}>Retry</Button> : null}
  </div>;
}
